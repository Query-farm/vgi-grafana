package grafana

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaskURL(t *testing.T) {
	cases := map[string]string{
		"http://scraper:hunter2@prometheus.internal.example:9090":             "http://prometheus.internal.example:9090",
		"https://hooks.example.com/services/T000/B000?token=do-not-leak#frag": "https://hooks.example.com/services/T000/B000",
		"warehouse.internal.example:5432":                                     "warehouse.internal.example:5432",
		"user:pw@db:5432":                                                     "db:5432",
		"":                                                                    "",
	}
	for in, want := range cases {
		if got := maskURL(in); got != want {
			t.Errorf("maskURL(%q) = %q, want %q", in, got, want)
		}
	}
	if h := urlHost("https://hooks.example.com/x?token=1"); h != "hooks.example.com" {
		t.Errorf("urlHost = %q", h)
	}
	if h := urlHost("not a url"); h != "" {
		t.Errorf("urlHost(not a url) = %q", h)
	}
}

func TestMaskJSON(t *testing.T) {
	in := map[string]any{"httpMethod": "POST", "apiKey": "x", "nested": map[string]any{"clientSecret": "y", "tlsAuth": true},
		"list": []any{map[string]any{"password": "z"}}}
	b, _ := json.Marshal(maskJSON(in))
	want := `{"apiKey":"[REDACTED]","httpMethod":"POST","list":[{"password":"[REDACTED]"}],"nested":{"clientSecret":"[REDACTED]","tlsAuth":true}}`
	if string(b) != want {
		t.Errorf("maskJSON = %s", b)
	}
}

func TestDurationSeconds(t *testing.T) {
	cases := map[string]any{"5m": 300.0, "1h30m": 5400.0, "7d": 604800.0, "1w": 604800.0, "250ms": 0.25, "0s": 0.0, "soon": nil, "": nil, "5": nil}
	for in, want := range cases {
		if got := durationSeconds(in); got != want {
			t.Errorf("durationSeconds(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRefName(t *testing.T) {
	for i, want := range map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"} {
		if got := refName(i); got != want {
			t.Errorf("refName(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestFolderPath(t *testing.T) {
	f := map[string]FolderRef{"ops": {Title: "Operations"}, "web": {Title: "Web", ParentUID: "ops"}, "loop": {Title: "L", ParentUID: "loop"}}
	for uid, want := range map[string]any{"": "/", "general": "/", "ops": "/Operations", "web": "/Operations/Web", "loop": "/L", "gone": nil} {
		if got := folderPath(f, uid); got != want {
			t.Errorf("folderPath(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestFlattenPanels(t *testing.T) {
	var d rec
	_ = json.Unmarshal([]byte(`{"panels": [
		{"id": 1, "type": "stat"},
		{"id": 2, "type": "row", "title": "R", "panels": [{"id": 3, "type": "table"}]},
		{"id": 4, "libraryPanel": {"uid": "lib"}}],
		"rows": [{"title": "legacy", "panels": [{"id": 9}]}]}`), &d)
	ps := flattenPanels(d)
	if len(ps) != 5 {
		t.Fatalf("got %d panels", len(ps))
	}
	if ps[2].RowID != int64(2) || ps[2].RowTitle != "R" || ps[4].RowTitle != "legacy" {
		t.Errorf("row attribution wrong: %+v", ps)
	}
}

func TestFlattenRoutes(t *testing.T) {
	var root rec
	_ = json.Unmarshal([]byte(`{"receiver": "a", "group_by": ["x"], "group_wait": "30s",
		"routes": [{"receiver": "b", "group_wait": "10s", "routes": [{"object_matchers": [["team", "=~", "web"]]}]}, {"repeat_interval": "1h"}]}`), &root)
	var out []rec
	flattenRoutes(root, "0", 0, nil, &out)
	if len(out) != 4 {
		t.Fatalf("got %d routes", len(out))
	}
	got := []string{}
	for _, r := range out {
		got = append(got, r["path"].(string)+"="+strOrEmpty(r, "eff", "receiver")+"/"+strOrEmpty(r, "eff", "group_wait"))
	}
	want := []string{"0=a/30s", "0.0=b/10s", "0.0.0=b/10s", "0.1=a/30s"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %s, want %s", i, got[i], want[i])
		}
	}
	if out[2]["parent"] != "0.0" || out[0]["parent"] != nil {
		t.Errorf("parents wrong: %v %v", out[2]["parent"], out[0]["parent"])
	}
	m := matchers(path(out[2], "node", "object_matchers")).([]any)
	if len(m) != 1 || m[0].(structVal)[1] != "=~" {
		t.Errorf("matchers = %v", m)
	}
}

func TestDecodeFrame(t *testing.T) {
	var fr dataFrame
	_ = json.Unmarshal([]byte(`{"schema": {"name": "f", "meta": {"type": "timeseries-multi"}, "fields": [
		{"name": "time", "type": "time"},
		{"name": "v", "type": "number", "labels": {"job": "api"}, "config": {"unit": "ms", "displayNameFromDS": "latency"}},
		{"name": "ok", "type": "boolean"}, {"name": "s", "type": "string"}]},
		"data": {"values": [[1767225600000, 1767225601000], [1.5, null], [true, null], ["x", null]],
			"entities": [null, {"Inf": [1]}, null, null], "nanos": [[0, 5000], null, null, null]}}`), &fr)
	rows, err := decodeFrame(&fr, "A", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Field != "latency" || rows[0].Value != 1.5 || rows[0].Labels["job"] != "api" || rows[0].Unit != "ms" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if v, _ := rows[1].Value.(float64); !math.IsInf(v, 1) {
		t.Errorf("row 1 value = %v, want +Inf", rows[1].Value)
	}
	if ts := rows[1].TS.(time.Time); ts.Nanosecond() != 5000 {
		t.Errorf("nanos not applied: %v", ts)
	}
	if rows[2].Value != 1.0 || rows[2].Str != "true" || rows[3].Value != nil || rows[4].Str != "x" {
		t.Errorf("bool/string rows wrong: %+v %+v %+v", rows[2], rows[3], rows[4])
	}
	// Microsecond and nanosecond epochs are recognised by magnitude.
	if got := frameTime(json.RawMessage("1794748660000000"), 0).(time.Time); got.Year() != 2026 {
		t.Errorf("microsecond time decoded as %v", got)
	}
	if got := frameTime(json.RawMessage("1794748660000000000"), 0).(time.Time); got.Year() != 2026 {
		t.Errorf("nanosecond time decoded as %v", got)
	}
}

func TestDSQueryArgs(t *testing.T) {
	n := int64(10)
	a := dsQueryArgs{DatasourceUID: "ds", QueryJSON: `[{"expr": "up"}, {"refId": "Z", "datasource": {"uid": "other"}}]`, MaxDataPoints: &n}
	qs, err := a.queries()
	if err != nil {
		t.Fatal(err)
	}
	if qs[0]["refId"] != "A" || qs[1]["refId"] != "Z" || qs[0]["maxDataPoints"] != int64(10) {
		t.Errorf("queries = %v", qs)
	}
	if qs[0]["datasource"].(map[string]any)["uid"] != "ds" || qs[1]["datasource"].(map[string]any)["uid"] != "other" {
		t.Errorf("datasource refs = %v", qs)
	}
	for _, bad := range []string{"not json", "[]", "[null]", `"x"`} {
		if _, err := (&dsQueryArgs{DatasourceUID: "ds", QueryJSON: bad}).queries(); err == nil {
			t.Errorf("query_json %q accepted", bad)
		}
	}
}

func TestPushdownHelpers(t *testing.T) {
	preds := []*expr{
		{K: "and", Args: []*expr{
			{K: "call", S: "in", Args: []*expr{{K: "col", S: "folder_uid"}, {K: "str", S: "b"}, {K: "str", S: "a"}}},
			{K: "call", S: "list_contains", Args: []*expr{{K: "col", S: "tags"}, {K: "str", S: "ops"}}},
		}},
		{K: "call", S: ">=", Args: []*expr{{K: "col", S: "time"}, {K: "ts", I: 1_000_000}}},
		{K: "call", S: ">", Args: []*expr{{K: "ts", I: 9_000_000}, {K: "col", S: "time"}}},
		{K: "call", S: "=", Args: []*expr{{K: "col", S: "panel_id"}, {K: "num", F: 4}}},
	}
	if v := eqValues(preds, "folder_uid"); len(v) != 2 || v[0] != "a" {
		t.Errorf("eqValues = %v", v)
	}
	if v := eqValues(preds, "panel_id"); len(v) != 1 || v[0] != "4" {
		t.Errorf("eqValues(panel_id) = %v", v)
	}
	if v := listContains(preds, "tags"); len(v) != 1 || v[0] != "ops" {
		t.Errorf("listContains = %v", v)
	}
	lo, okLo, hi, okHi := timeBounds(preds, "time")
	if !okLo || !okHi || lo != 1_000_000 || hi != 9_000_000 {
		t.Errorf("timeBounds = %d %v %d %v", lo, okLo, hi, okHi)
	}
	// An OR does not pin a column.
	or := []*expr{{K: "or", Args: preds[3:]}}
	if v := eqValues(or, "panel_id"); v != nil {
		t.Errorf("eqValues under OR = %v", v)
	}
}

func TestRowFilterEvaluation(t *testing.T) {
	schema := dashboardsDef.Schema()
	env := newRowEnv(schema)
	row := make([]any, schema.NumFields())
	idx := func(n string) int { return schema.FieldIndices(n)[0] }
	row[idx("title")] = "Web Overview"
	row[idx("tags")] = []any{"ops", "web"}
	env.row = row
	ok, err := matchAll([]*expr{
		{K: "call", S: "list_contains", Args: []*expr{{K: "col", S: "tags"}, {K: "str", S: "web"}}},
		{K: "call", S: "~~", Args: []*expr{{K: "col", S: "title"}, {K: "str", S: "Web%"}}},
		{K: "call", S: "isnull", Args: []*expr{{K: "col", S: "folder_uid"}}},
	}, env)
	if err != nil || !ok {
		t.Errorf("matchAll = %v, %v", ok, err)
	}
	ok, _ = matchAll([]*expr{{K: "call", S: "list_contains", Args: []*expr{{K: "col", S: "tags"}, {K: "str", S: "db"}}}}, env)
	if ok {
		t.Error("list_contains matched a missing tag")
	}
}

func TestResolveConn(t *testing.T) {
	c, err := connFromFields(map[string]interface{}{"base_url": "https://g.example.com/", "token": "t", "org_id": "3", "page_size": "50"})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://g.example.com" || c.Headers.Get("Authorization") != "Bearer t" || c.Headers.Get("X-Grafana-Org-Id") != "3" || c.PageSize != 50 {
		t.Errorf("conn = %+v", c)
	}
	c, _ = connFromFields(map[string]interface{}{"base_url": "http://h", "username": "u", "password": "p"})
	if c.Headers.Get("Authorization") != "Basic dTpw" {
		t.Errorf("basic auth header = %q", c.Headers.Get("Authorization"))
	}
	for _, bad := range []map[string]interface{}{{}, {"base_url": "ftp://h"}, {"base_url": "http://u:p@h"},
		{"base_url": "http://h", "org_id": "x"}, {"base_url": "http://h", "page_size": "0"}} {
		if _, err := connFromFields(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestRetries(t *testing.T) {
	sleep = func(context.Context, time.Duration) {}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"version": "12.1.1"}`))
	}))
	defer srv.Close()
	c := &conn{BaseURL: srv.URL, Headers: http.Header{}, PageSize: 10}
	var out rec
	if err := c.get(context.Background(), "/api/health", nil, &out); err != nil || out["version"] != "12.1.1" || calls.Load() != 3 {
		t.Errorf("get = %v %v after %d calls", out, err, calls.Load())
	}
	if d := retryDelay("2", 1); d != 2*time.Second {
		t.Errorf("retryDelay = %v", d)
	}
}

// fakeAnnotations mimics GET /api/annotations: overlap filter on [from, to],
// Grafana's ORDER BY epoch*1000 + epoch_end DESC, and limit.
func fakeAnnotations(all []rec, requests *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		q := r.URL.Query()
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		var hit []rec
		for _, a := range all {
			t0, t1 := int64(a["time"].(float64)), int64(a["timeEnd"].(float64))
			if t0 <= to && t1 >= from {
				hit = append(hit, a)
			}
		}
		key := func(a rec) float64 { return a["time"].(float64)*1000 + a["timeEnd"].(float64) }
		sort.SliceStable(hit, func(i, j int) bool { return key(hit[i]) > key(hit[j]) })
		if len(hit) > limit {
			hit = hit[:limit]
		}
		_ = json.NewEncoder(w).Encode(hit)
	}
}

func TestAnnotationWindowsReadEachOnce(t *testing.T) {
	var all []rec
	id := 0
	add := func(t0, t1 int64) {
		id++
		all = append(all, rec{"id": float64(id), "time": float64(t0), "timeEnd": float64(t1)})
	}
	base := int64(1767225600000)
	for i := int64(0); i < 40; i++ {
		add(base+i*1000, base+i*1000) // points every second
	}
	for i := 0; i < 5; i++ {
		add(base+5000, base+5000) // five at one millisecond
	}
	add(base-3_600_000, base+3_600_000) // a long region overlapping everything
	add(base+2000, base+30_000)
	var requests atomic.Int32
	srv := httptest.NewServer(fakeAnnotations(all, &requests))
	defer srv.Close()
	s := &scan{C: &conn{BaseURL: srv.URL, Headers: http.Header{}, PageSize: 3}, St: &ScanState{}, Def: annotationsDef}
	seen := map[int64]int{}
	idCol := annotationsDef.Schema().FieldIndices("annotation_id")[0]
	for steps := 0; !s.St.Done; steps++ {
		if steps > 10_000 {
			t.Fatal("scan did not terminate")
		}
		rows, err := annotationsDef.Next(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen[r[idCol].(int64)]++
		}
	}
	if len(seen) != len(all) {
		t.Errorf("read %d of %d annotations", len(seen), len(all))
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("annotation %d read %d times", k, n)
		}
	}
	t.Logf("%d annotations in %d requests at page size 3", len(all), requests.Load())
	if requests.Load() > 200 {
		t.Errorf("too many requests: %d", requests.Load())
	}
}
