// Offline scalars: no server and no secret. They carry the worker's
// executable examples.
//
//	SELECT grafana.main.duration_seconds('1h30m');                      -- 5400.0
//	SELECT grafana.main.redact_url('https://u:p@hooks.example.com/x?t=1'); -- https://hooks.example.com/x

package grafana

import (
	"context"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

type textArgs struct {
	Text *array.String `vgi:"pos=0,const=false,doc=The text to convert. NULL gives NULL."`
}

// executableExample is one vgi.executable_examples entry.
type executableExample struct {
	Description string  `json:"description"`
	SQL         string  `json:"sql"`
	Expected    [][]any `json:"expected_result"`
}

// textScalar is a vectorized one-column scalar over VARCHAR.
type textScalar struct {
	name       string
	docs       docs
	desc       string
	examples   []vgi.CatalogExample
	executable []executableExample
	result     arrow.DataType
	// eval appends the result for one non-NULL input to b.
	eval func(b array.Builder, s string)
}

func (f *textScalar) Name() string { return f.name }

func (f *textScalar) Metadata() vgi.FunctionMetadata {
	tags := withExamples(objectTags(f.docs), f.examples)
	tags["vgi.executable_examples"] = mustJSON(f.executable)
	return vgi.FunctionMetadata{
		Description: f.desc,
		Stability:   vgi.StabilityConsistent,
		Tags:        tags,
		Examples:    f.examples,
	}
}

func (f *textScalar) OnBindTyped(*textArgs, *vgi.BindParams) (*vgi.BindResponse, error) {
	return vgi.BindResult(f.result)
}

func (f *textScalar) ProcessTyped(_ context.Context, _ *textArgs, _ *vgi.ProcessParams, batch arrow.RecordBatch) (arrow.RecordBatch, error) {
	col := batch.Column(0)
	b := array.NewBuilder(memory.NewGoAllocator(), f.result)
	defer b.Release()
	for i := 0; i < int(batch.NumRows()); i++ {
		if col.IsNull(i) {
			b.AppendNull()
			continue
		}
		f.eval(b, vgi.GetStringValue(col, i))
	}
	arr := b.NewArray()
	defer arr.Release()
	schema := arrow.NewSchema([]arrow.Field{{Name: "result", Type: f.result, Nullable: true}}, nil)
	return array.NewRecordBatch(schema, []arrow.Array{arr}, batch.NumRows()), nil
}

func newDurationSeconds() *textScalar {
	return &textScalar{
		name: "duration_seconds",
		desc: "Seconds in a Grafana / Prometheus duration string such as '5m', '1h30m' or '7d' (NULL when invalid)",
		docs: docs{Title: "Parse A Grafana Duration", Category: "tools",
			Keywords: "duration, interval, seconds, for, group_wait, repeat_interval, refresh, parse",
			DocLLM: "duration_seconds(d) converts a duration string as Grafana writes them ('30s', '5m', '1h30m', '7d', '1w', '1y', " +
				"'250ms', '0s') to seconds as a `DOUBLE`. NULL for NULL or an invalid duration. Use it on duration text such as a " +
				"dashboard's refresh column or a time range like 'now-6h' after stripping 'now-'. Needs no server or secret.",
			DocMD: "## duration_seconds(d)\n\n`'1h30m'` → `5400.0`. Units: ms, s, m, h, d, w, y. `NULL` when invalid."},
		examples: []vgi.CatalogExample{{
			SQL:         "SELECT title, grafana.main.duration_seconds(refresh) AS refresh_seconds FROM grafana.main.dashboards WHERE refresh IS NOT NULL ORDER BY refresh_seconds",
			Description: "Dashboards that auto-refresh most often"}},
		executable: []executableExample{{Description: "Convert durations",
			SQL:      "SELECT d, grafana.main.duration_seconds(d) AS seconds FROM (VALUES ('1h30m'), ('5m'), ('250ms'), ('7d'), ('soon')) t(d) ORDER BY d",
			Expected: [][]any{{"1h30m", 5400.0}, {"250ms", 0.25}, {"5m", 300.0}, {"7d", 604800.0}, {"soon", nil}}}},
		result: arrow.PrimitiveTypes.Float64,
		eval: func(b array.Builder, s string) {
			if v, ok := durationSeconds(s).(float64); ok {
				b.(*array.Float64Builder).Append(v)
				return
			}
			b.AppendNull()
		},
	}
}

func newRedactURL() *textScalar {
	return &textScalar{
		name: "redact_url",
		desc: "A URL with its user:password@, query string and fragment removed, the way the worker shows data source endpoints",
		docs: docs{Title: "Redact Credentials From A URL", Category: "tools",
			Keywords: "redact, mask, url, credentials, token, webhook, secret, endpoint",
			DocLLM: "redact_url(url) removes user info (user:password@), the query string and the fragment from a URL, which is how " +
				"grafana.main.connections.endpoint is produced. NULL for NULL; text that is not a URL keeps only what precedes '?' or " +
				"'#' and follows '@'. Use it before storing or sharing URLs taken from dashboard links or panel JSON. Needs no server.",
			DocMD: "## redact_url(url)\n\n`'https://u:p@h.example.com/x?token=1'` → `'https://h.example.com/x'`."},
		examples: []vgi.CatalogExample{{
			SQL:         "SELECT title, grafana.main.redact_url(panel_json->>'$.links[0].url') AS link FROM grafana.main.panels WHERE panel_json->>'$.links[0].url' IS NOT NULL ORDER BY title",
			Description: "Panel links with any embedded credentials removed"}},
		executable: []executableExample{{Description: "Redact two URLs",
			SQL: "SELECT grafana.main.redact_url(u) AS redacted FROM (VALUES ('https://bot:s3cret@hooks.example.com/services/T0?token=abc'), " +
				"('http://prometheus:9090/api#x')) t(u) ORDER BY redacted",
			Expected: [][]any{{"http://prometheus:9090/api"}, {"https://hooks.example.com/services/T0"}}}},
		result: arrow.BinaryTypes.String,
		eval: func(b array.Builder, s string) {
			b.(*array.StringBuilder).Append(maskURL(s))
		},
	}
}

// toolScalars lists the offline scalars.
func toolScalars() []*textScalar { return []*textScalar{newDurationSeconds(), newRedactURL()} }

func registerTools(w *vgi.Worker) {
	for _, f := range toolScalars() {
		w.RegisterScalar(vgi.AsScalarFunction[textArgs](f))
	}
}
