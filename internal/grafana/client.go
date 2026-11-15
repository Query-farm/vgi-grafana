// HTTP client for the Grafana HTTP API.
//
// Every function talks to the server named by a `grafana` DuckDB secret: its
// base URL, a service-account token or API key (sent as a bearer token) or a
// basic-auth user and password, and an optional organization id sent as
// X-Grafana-Org-Id. Credentials are read from the secret on every call and
// never kept in table-function state, which travels back to DuckDB over HTTP.
//
// The worker only issues GET requests, plus POST /api/ds/query, which runs a
// read-only data source query and saves nothing.

package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
)

const (
	// SecretType is the DuckDB secret type every function reads.
	SecretType = "grafana"
	// MaxAttempts bounds retries of 429/502/503/504 responses and network errors.
	MaxAttempts = 5
	// Timeout bounds every request.
	Timeout = 120 * time.Second
	// DefaultPageSize is the page size for paged list endpoints.
	DefaultPageSize = 1000
	// maxBody bounds one response body.
	maxBody = 512 << 20
)

// userAgent identifies the worker to the API.
const userAgent = "vgi-grafana-worker/" + Version

func secretField(name, doc string, redact bool) arrow.Field {
	keys := []string{"vgi_doc"}
	vals := []string{doc}
	if redact {
		keys = append(keys, "redact")
		vals = append(vals, "true")
	}
	return arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true,
		Metadata: arrow.NewMetadata(keys, vals)}
}

// APISecretType is registered with DuckDB so users can run
// CREATE SECRET (TYPE grafana, BASE_URL '...', TOKEN '...').
var APISecretType = vgi.SecretTypeSpec{
	Name:        SecretType,
	Description: "Connection to a Grafana HTTP API (Grafana OSS, Enterprise or Grafana Cloud): base URL plus a service account token, API key or basic-auth login.",
	Schema: arrow.NewSchema([]arrow.Field{
		secretField("base_url", "Grafana root URL, e.g. 'https://grafana.example.com' or 'https://acme.grafana.net'. Include the sub-path when Grafana is served from one.", false),
		secretField("token", "Service account token or API key, sent as 'Authorization: Bearer <token>'.", true),
		secretField("username", "Basic-auth login (used when no TOKEN is set).", false),
		secretField("password", "Basic-auth password.", true),
		secretField("org_id", "Organization id sent as the X-Grafana-Org-Id header; default: the credential's current organization.", false),
		secretField("page_size", "Page size for paged list endpoints (search, users, teams, library panels, annotations); default 1000.", false),
	}, &secretSchemaMetadata),
}

// secretSchemaMetadata links the secret type to its documentation.
var secretSchemaMetadata = arrow.NewMetadata(
	[]string{"vgi_doc_url"},
	[]string{SourceURL + "#secrets-and-configuration"})

// secretRequirements is declared by every function: resolve the session's
// grafana secret(s) and pass them to the worker.
var secretRequirements = []vgi.SecretRequirement{{SecretType: SecretType}}

// AttachSecretOption names the secret catalog tables read (ATTACH option).
const AttachSecretOption = "secret_name"

// attachSecretName is the ATTACH-level secret_name option, or "".
func attachSecretName(o *vgi.AttachOptions) string {
	if o == nil {
		return ""
	}
	s, _ := o.String(AttachSecretOption)
	return strings.TrimSpace(s)
}

// secretScope asks the extension for a named secret on the first phase of a
// two-phase bind. It returns nil when no name is set or the secrets are
// already resolved.
func secretScope(params *vgi.BindParams, name string) *vgi.BindResponse {
	if name == "" || params.ResolvedSecretsProvided {
		return nil
	}
	return &vgi.BindResponse{SecretScopeRequest: []vgi.SecretLookup{{SecretType: SecretType, SecretName: name}}}
}

// conn is a resolved connection. It is built per call from the secret and is
// never stored in scan state.
type conn struct {
	BaseURL  string
	Headers  http.Header
	PageSize int
}

// resolveConn picks the secret (the named one when name is non-empty) and
// turns it into a connection.
func resolveConn(secrets vgi.Secrets, name string) (*conn, error) {
	var fields map[string]interface{}
	var ok bool
	if name != "" {
		fields, ok = secrets.LookupNamed(SecretType, name)
		if !ok {
			return nil, fmt.Errorf("no grafana secret named %q: create it with CREATE SECRET %s (TYPE grafana, BASE_URL '...', TOKEN '...')", name, name)
		}
	} else {
		fields, ok = secrets.Lookup(SecretType)
		if !ok {
			return nil, errors.New("no grafana secret found: create one with CREATE SECRET (TYPE grafana, BASE_URL 'https://grafana.example.com', TOKEN '<service account token>') or USERNAME/PASSWORD for basic auth")
		}
	}
	return connFromFields(fields)
}

// connFromFields turns a grafana secret's fields into a connection.
func connFromFields(fields map[string]interface{}) (*conn, error) {
	get := func(k string) string { return strings.TrimSpace(vgi.RenderSecretValue(fields[k])) }
	base := strings.TrimRight(get("base_url"), "/")
	if base == "" {
		return nil, errors.New("the grafana secret has no BASE_URL")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the grafana secret's BASE_URL must be an http(s) URL, got %q", maskURL(base))
	}
	if u.User != nil {
		return nil, errors.New("the grafana secret's BASE_URL must not contain credentials: use TOKEN or USERNAME/PASSWORD")
	}
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", userAgent)
	if t := get("token"); t != "" {
		h.Set("Authorization", "Bearer "+t)
	} else if user := get("username"); user != "" {
		req := &http.Request{Header: http.Header{}}
		req.SetBasicAuth(user, vgi.RenderSecretValue(fields["password"]))
		h.Set("Authorization", req.Header.Get("Authorization"))
	}
	if o := get("org_id"); o != "" {
		if _, err := strconv.ParseInt(o, 10, 64); err != nil {
			return nil, fmt.Errorf("the grafana secret's ORG_ID must be an integer, got %q", o)
		}
		h.Set("X-Grafana-Org-Id", o)
	}
	ps := DefaultPageSize
	if s := get("page_size"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 5000 {
			return nil, fmt.Errorf("the grafana secret's PAGE_SIZE must be an integer from 1 to 5000, got %q", s)
		}
		ps = n
	}
	return &conn{BaseURL: base, Headers: h, PageSize: ps}, nil
}

// apiError is a failed API call. The message names the HTTP status and the
// server's message so users can tell a permission problem from an outage.
type apiError struct {
	Status  int
	Message string
	Path    string
	// Body is the raw response (never part of the message).
	Body []byte
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("grafana %s: HTTP %d", e.Path, e.Status)
	switch e.Status {
	case http.StatusUnauthorized:
		msg += " (check the grafana secret's TOKEN or USERNAME/PASSWORD)"
	case http.StatusForbidden:
		msg += " (the credential lacks the permission this table needs)"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// httpClient is shared so connections are reused across pages.
var httpClient = &http.Client{Timeout: Timeout}

// sleep is replaced in tests so retries don't wait.
var sleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// retryDelay honours a Retry-After header (seconds or HTTP date), else backs off exponentially.
func retryDelay(header string, attempt int) time.Duration {
	header = strings.TrimSpace(header)
	if s, err := strconv.Atoi(header); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, 60*time.Second)
	}
	if t, err := http.ParseTime(header); err == nil {
		return min(max(time.Until(t), 0), 60*time.Second)
	}
	return time.Duration(250<<attempt) * time.Millisecond
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// encodeParams sorts keys so request URLs are deterministic (recorded
// fixtures key on them).
func encodeParams(params url.Values) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range params[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// get calls GET <base><path>?params and decodes the JSON body into out.
func (c *conn) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, nil, out)
}

// post calls POST <base><path> with a JSON body (only /api/ds/query, which is read-only).
func (c *conn) post(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, nil, data, out)
}

func (c *conn) do(ctx context.Context, method, path string, params url.Values, body []byte, out any) error {
	target := c.BaseURL + path
	if q := encodeParams(params); q != "" {
		target += "?" + q
	}
	for attempt := 1; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return err
		}
		for k, vs := range c.Headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			if attempt >= MaxAttempts || errors.Is(err, context.Canceled) {
				return fmt.Errorf("grafana %s: %w", path, err)
			}
			sleep(ctx, retryDelay("", attempt))
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if retryable(resp.StatusCode) && attempt < MaxAttempts {
			slog.Debug("grafana: retrying", "path", path, "status", resp.StatusCode, "attempt", attempt)
			sleep(ctx, retryDelay(resp.Header.Get("Retry-After"), attempt))
			continue
		}
		if readErr != nil {
			return fmt.Errorf("grafana %s: reading response: %w", path, readErr)
		}
		if resp.StatusCode/100 != 2 {
			return &apiError{Status: resp.StatusCode, Path: path, Message: errorMessage(data), Body: data}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("grafana %s: decoding response: %w", path, err)
		}
		return nil
	}
}

// errorMessage extracts Grafana's {"message": ...} or a short body snippet.
func errorMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		if m.Error != "" {
			return m.Error
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// maskURL removes user info, query and fragment from a URL so that no
// embedded credential (user:password@, ?token=) is returned. Unparseable
// values keep only what precedes the first '@', '?' or '#'.
func maskURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.ForceQuery = false
		return u.String()
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// urlHost returns only the host[:port] of a URL, or "".
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
