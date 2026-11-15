// Record accessors, column constructors and shared fetch helpers.

package grafana

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
)

// newArgs returns a pointer to a fresh zero value of proto's type.
func newArgs(proto any) (any, error) {
	t := reflect.TypeOf(proto)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return reflect.New(t).Interface(), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// path walks nested objects: path(r, "meta", "created").
func path(r rec, keys ...string) any {
	var cur any = r
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// str reads a string field; "" and non-strings are NULL.
func str(r rec, keys ...string) any {
	if s, ok := path(r, keys...).(string); ok && s != "" {
		return s
	}
	return nil
}

// strOrEmpty reads a string field ("" when absent).
func strOrEmpty(r rec, keys ...string) string {
	s, _ := path(r, keys...).(string)
	return s
}

// i64 reads a numeric field as BIGINT.
func i64(r rec, keys ...string) any {
	switch x := path(r, keys...).(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n
		}
	}
	return nil
}

// f64 reads a numeric field as DOUBLE.
func f64(r rec, keys ...string) any {
	if x, ok := path(r, keys...).(float64); ok {
		return x
	}
	return nil
}

// boolean reads a boolean field.
func boolean(r rec, keys ...string) any {
	if x, ok := path(r, keys...).(bool); ok {
		return x
	}
	return nil
}

// ts reads an RFC 3339 time field; the zero time and "" are NULL.
func ts(r rec, keys ...string) any { return parseTime(path(r, keys...)) }

func parseTime(v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return nil
	}
	return t.UTC()
}

// msTime converts epoch milliseconds to a time (0 is NULL).
func msTime(r rec, keys ...string) any {
	x, ok := path(r, keys...).(float64)
	if !ok || x == 0 {
		return nil
	}
	return time.UnixMilli(int64(x)).UTC()
}

// strList reads a list of strings (NULL when absent, [] when empty).
func strList(r rec, keys ...string) any {
	items, ok := path(r, keys...).([]any)
	if !ok {
		if path(r, keys...) == nil {
			return nil
		}
		return []any{}
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		switch x := it.(type) {
		case string:
			out = append(out, x)
		case nil:
		default:
			b, _ := json.Marshal(x)
			out = append(out, string(b))
		}
	}
	return out
}

// strMap reads an object of scalar values as MAP(VARCHAR, VARCHAR).
func strMap(r rec, keys ...string) any {
	m, ok := path(r, keys...).(map[string]any)
	if !ok {
		if path(r, keys...) == nil {
			return map[string]string{}
		}
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case nil:
		default:
			b, _ := json.Marshal(x)
			out[k] = string(b)
		}
	}
	return out
}

// jsonOf renders a field as JSON (NULL when absent).
func jsonOf(r rec, keys ...string) any {
	v := path(r, keys...)
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return jsonText(b)
}

// ---- column constructors -----------------------------------------------------

func cStr(name, doc string, keys ...string) col {
	return col{column(name, tString, doc, true), func(r rec, _ *scan) any { return str(r, keys...) }}
}

func cKey(name, doc string, keys ...string) col {
	return col{column(name, tString, doc, false), func(r rec, _ *scan) any {
		if s := str(r, keys...); s != nil {
			return s
		}
		if n := i64(r, keys...); n != nil {
			return strconv.FormatInt(n.(int64), 10)
		}
		return ""
	}}
}

func cInt(name, doc string, keys ...string) col {
	return col{column(name, tInt64, doc, true), func(r rec, _ *scan) any { return i64(r, keys...) }}
}

func cFloat(name, doc string, keys ...string) col {
	return col{column(name, tFloat64, doc, true), func(r rec, _ *scan) any { return f64(r, keys...) }}
}

func cBool(name, doc string, keys ...string) col {
	return col{column(name, tBool, doc, true), func(r rec, _ *scan) any { return boolean(r, keys...) }}
}

func cTS(name, doc string, keys ...string) col {
	return col{column(name, tTS, doc, true), func(r rec, _ *scan) any { return ts(r, keys...) }}
}

func cList(name, doc string, keys ...string) col {
	return col{column(name, tStrList, doc, true), func(r rec, _ *scan) any { return strList(r, keys...) }}
}

func cMap(name, doc string, keys ...string) col {
	return col{column(name, tMap, doc, true), func(r rec, _ *scan) any { return strMap(r, keys...) }}
}

func cJSON(name, doc string, keys ...string) col {
	return col{column(name, tJSON, doc, true), func(r rec, _ *scan) any { return jsonOf(r, keys...) }}
}

// cConst is a constant column (tool = 'grafana').
func cConst(name, doc, v string) col {
	return col{column(name, tString, doc, false), func(rec, *scan) any { return v }}
}

// cFn is a computed column.
func cFn(name string, typ arrow.DataType, doc string, fn func(r rec, s *scan) any) col {
	return col{column(name, typ, doc, true), fn}
}

// cURL is the web UI link: base URL + the record's relative url field.
func cURL(doc string, keys ...string) col {
	return cFn("url", tString, doc, func(r rec, s *scan) any {
		rel := strOrEmpty(r, keys...)
		if rel == "" {
			return nil
		}
		return webURL(s.C, rel)
	})
}

// webURL joins the secret's base URL and a relative UI path.
func webURL(c *conn, rel string) string {
	if strings.HasPrefix(rel, "http://") || strings.HasPrefix(rel, "https://") {
		return rel
	}
	base := c.BaseURL
	// Grafana's relative urls already carry the sub-path when one is configured.
	if u, err := url.Parse(base); err == nil && u.Path != "" && strings.HasPrefix(rel, u.Path+"/") {
		u.Path = ""
		base = strings.TrimRight(u.String(), "/")
	}
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return base + rel
}

// ---- folders -----------------------------------------------------------------

// loadFolders reads every folder (nested ones included) through the search
// API, which returns each folder with its parent's uid.
func loadFolders(ctx context.Context, c *conn) (map[string]FolderRef, error) {
	out := map[string]FolderRef{}
	for page := 1; ; page++ {
		var hits []rec
		p := url.Values{"type": {"dash-folder"}, "limit": {strconv.Itoa(c.PageSize)}, "page": {strconv.Itoa(page)}}
		if err := c.get(ctx, "/api/search", p, &hits); err != nil {
			return nil, err
		}
		for _, h := range hits {
			out[strOrEmpty(h, "uid")] = FolderRef{Title: strOrEmpty(h, "title"), ParentUID: strOrEmpty(h, "folderUid")}
		}
		if len(hits) < c.PageSize {
			return out, nil
		}
	}
}

// folderPath is "/Parent/Child" for a folder uid; "/" for the General
// (root) level, and NULL for an unknown folder.
func folderPath(folders map[string]FolderRef, uid string) any {
	if uid == "" || uid == "general" {
		return "/"
	}
	var parts []string
	seen := map[string]bool{}
	for cur := uid; cur != "" && !seen[cur]; {
		seen[cur] = true
		f, ok := folders[cur]
		if !ok {
			if len(parts) == 0 {
				return nil
			}
			break
		}
		parts = append([]string{f.Title}, parts...)
		cur = f.ParentUID
	}
	return "/" + strings.Join(parts, "/")
}

// folderTitle is the title of a folder uid ("General" at the root).
func folderTitle(folders map[string]FolderRef, uid string) any {
	if uid == "" || uid == "general" {
		return "General"
	}
	if f, ok := folders[uid]; ok {
		return f.Title
	}
	return nil
}

// cFolderPath reads the record's folder uid field and renders its path.
func cFolderPath(doc string, keys ...string) col {
	return cFn("folder_path", tString, doc, func(r rec, s *scan) any {
		return folderPath(s.St.Folders, strOrEmpty(r, keys...))
	})
}

// ---- pushdown helpers ----------------------------------------------------------

// eqValues returns the string values a column is pinned to by a top-level
// `col = 'v'` or `col IN (...)` predicate (nil when not pinned).
func eqValues(preds []*expr, column string) []string {
	preds = conjuncts(preds)
	for _, p := range preds {
		if p.K != "call" || len(p.Args) < 2 || p.Args[0].K != "col" || p.Args[0].S != column {
			continue
		}
		switch p.S {
		case "=":
			if v, ok := literalString(p.Args[1]); ok {
				return []string{v}
			}
		case "in":
			var out []string
			for _, a := range p.Args[1:] {
				v, ok := literalString(a)
				if !ok {
					out = nil
					break
				}
				out = append(out, v)
			}
			if out != nil {
				sort.Strings(out)
				return out
			}
		}
	}
	return nil
}

func literalString(e *expr) (string, bool) {
	switch e.K {
	case "str":
		return e.S, true
	case "num":
		if e.F == float64(int64(e.F)) {
			return strconv.FormatInt(int64(e.F), 10), true
		}
	case "bool":
		return strconv.FormatBool(e.B), true
	}
	return "", false
}

// listContains returns values v from top-level list_contains(column, 'v').
func listContains(preds []*expr, column string) []string {
	preds = conjuncts(preds)
	var out []string
	for _, p := range preds {
		if p.K == "call" && (p.S == "list_contains" || p.S == "array_contains" || p.S == "list_has" || p.S == "array_has") &&
			len(p.Args) == 2 && p.Args[0].K == "col" && p.Args[0].S == column && p.Args[1].K == "str" {
			out = append(out, p.Args[1].S)
		}
	}
	return out
}

// timeBounds returns the tightest [lo, hi] (Unix microseconds) a set of
// top-level comparisons puts on a TIMESTAMPTZ column; ok* are false when unbounded.
func timeBounds(preds []*expr, column string) (lo int64, okLo bool, hi int64, okHi bool) {
	preds = conjuncts(preds)
	for _, p := range preds {
		if p.K != "call" || len(p.Args) != 2 {
			continue
		}
		a, b, op := p.Args[0], p.Args[1], p.S
		if b.K == "col" && a.K == "ts" {
			a, b = b, a
			switch op {
			case "<":
				op = ">"
			case "<=":
				op = ">="
			case ">":
				op = "<"
			case ">=":
				op = "<="
			}
		}
		if a.K != "col" || a.S != column || b.K != "ts" {
			continue
		}
		v := b.I
		switch op {
		case ">", ">=":
			if !okLo || v > lo {
				lo, okLo = v, true
			}
		case "<", "<=":
			if b.F > 0 && op == "<" {
				v++
			}
			if !okHi || v < hi {
				hi, okHi = v, true
			}
		case "=":
			if !okLo || v > lo {
				lo, okLo = v, true
			}
			if !okHi || v < hi {
				hi, okHi = v, true
			}
		}
	}
	return
}

// conjuncts flattens top-level AND nodes.
func conjuncts(preds []*expr) []*expr {
	var out []*expr
	for _, p := range preds {
		if p.K == "and" {
			out = append(out, conjuncts(p.Args)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}
