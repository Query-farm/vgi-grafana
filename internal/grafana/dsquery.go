// ds_query: run a read-only query against any Grafana data source through
// Grafana's query API (POST /api/ds/query) and return the data frames as
// long rows, the same shape vgi-prometheus uses (labels MAP, ts, value).

package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
)

type dsQueryArgs struct {
	DatasourceUID string    `vgi:"pos=0,doc=UID of the data source to query (grafana.main.connections.id)."`
	QueryJSON     string    `vgi:"pos=1,doc=The data source query model as JSON: one object or an array of objects (each becomes one refId; refId defaults to A, B, ...)."`
	StartTime     time.Time `vgi:"pos=2,doc=Start of the query time range."`
	EndTime       time.Time `vgi:"pos=3,doc=End of the query time range."`
	MaxDataPoints *int64    `vgi:"name=max_data_points,ge=1,le=1000000,doc=maxDataPoints sent with each query (default: the data source's own default)."`
	IntervalMs    *int64    `vgi:"name=interval_ms,ge=1,doc=intervalMs sent with each query: the step data sources use for time bucketing."`
	SecretName    *string   `vgi:"name=secret_name,doc=Name of the grafana secret to use when several exist."`
}

func (a *dsQueryArgs) secret() string { return deref(a.SecretName) }

func (a *dsQueryArgs) validate() error {
	if strings.TrimSpace(a.DatasourceUID) == "" {
		return fmt.Errorf("ds_query: datasource_uid is empty; pick one from grafana.main.connections")
	}
	if !a.EndTime.After(a.StartTime) && !a.EndTime.Equal(a.StartTime) {
		return fmt.Errorf("ds_query: end_time is before start_time")
	}
	_, err := a.queries()
	return err
}

// queries parses QueryJSON into the request's query list, filling refId and
// the data source reference.
func (a *dsQueryArgs) queries() ([]map[string]any, error) {
	raw := strings.TrimSpace(a.QueryJSON)
	var list []map[string]any
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, fmt.Errorf("ds_query: query_json is not a JSON array of objects: %v", err)
		}
	} else {
		var one map[string]any
		if err := json.Unmarshal([]byte(raw), &one); err != nil {
			return nil, fmt.Errorf("ds_query: query_json must be a JSON object (or array of objects), e.g. '{\"expr\": \"up\"}': %v", err)
		}
		list = []map[string]any{one}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("ds_query: query_json holds no query")
	}
	for i, q := range list {
		if q == nil {
			return nil, fmt.Errorf("ds_query: query %d is null", i)
		}
		if _, ok := q["refId"].(string); !ok {
			q["refId"] = refName(i)
		}
		if _, ok := q["datasource"]; !ok {
			q["datasource"] = map[string]any{"uid": a.DatasourceUID}
		}
		if a.MaxDataPoints != nil {
			q["maxDataPoints"] = *a.MaxDataPoints
		}
		if a.IntervalMs != nil {
			q["intervalMs"] = *a.IntervalMs
		}
	}
	return list, nil
}

// refName is A, B, ..., Z, AA, AB, ...
func refName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func lr(f func(r *longRow) any) func(rec, *scan) any {
	return func(r rec, _ *scan) any { return f(r["_lr"].(*longRow)) }
}

var dsQueryDef = &tableDef{
	Name: "ds_query",
	Args: &dsQueryArgs{},
	Docs: fnDocs{
		Title:    "Query Any Grafana Data Source",
		Category: "query",
		Keywords: "ds_query, data source query, /api/ds/query, data frames, prometheus, loki, sql, testdata, time series, proxy",
		Description: "Run a read-only query against any Grafana data source through Grafana's query API and get the data frames " +
			"back as long rows (labels MAP, ts, value)",
		DocLLM: "ds_query(datasource_uid, query_json, start_time, end_time, max_data_points := ..., interval_ms := ..., secret_name := ...) " +
			"POSTs /api/ds/query with the given query model(s) and time range, through Grafana, so any configured data source (Prometheus, " +
			"Loki, PostgreSQL, Elasticsearch, CloudWatch, TestData, ...) is queryable with the credential's permissions. query_json is the " +
			"data source's own query model as Grafana stores it in a panel target, e.g. '{\"expr\": \"up\"}' for Prometheus, " +
			"'{\"rawSql\": \"SELECT ...\", \"format\": \"table\"}' for SQL, '{\"scenarioId\": \"csv_metric_values\", \"stringInput\": \"1,2,3\"}' " +
			"for TestData; an array runs several queries (refId defaults to A, B, ...). Copy one from grafana.main.panel_queries.target_json. " +
			"Output is long: one row per (frame row, non-time field): ref_id, frame_index, frame_name, frame_type, field, labels " +
			"MAP(VARCHAR, VARCHAR) (the field's labels, e.g. Prometheus series labels), row_index, ts (the frame's first time field; NULL " +
			"for table frames), value DOUBLE (numbers; booleans as 1/0; NaN/Inf kept), string_value (strings, booleans, extra time fields), unit. " +
			"Pivot table frames with PIVOT or by grouping on row_index. A data source error fails the query with its refId and message.",
		DocMD: "## ds_query(datasource_uid, query_json, start_time, end_time)\n\nAny Grafana data source, queried through Grafana " +
			"(`POST /api/ds/query`), as long rows: `labels` MAP, `ts`, `value`, `string_value`. Read-only: queries run, nothing is saved.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT ts, value FROM grafana.main.ds_query('testdata', '{\"scenarioId\": \"csv_metric_values\", \"stringInput\": \"1,20,90\", \"alias\": \"cpu\"}', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 01:00:00+00') ORDER BY ts",
				Description: "A TestData series: three values spread over the hour"},
			{SQL: "SELECT labels['job'] AS job, max(value) AS peak FROM grafana.main.ds_query('prom-prod', '{\"expr\": \"sum by (job) (rate(http_requests_total[5m]))\"}', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 06:00:00+00', interval_ms := 60000) GROUP BY job ORDER BY peak DESC",
				Description: "PromQL through Grafana's Prometheus data source: peak request rate per job"},
		},
	},
	Cols: []col{
		{column("ref_id", tString, "Query reference id (A, B, ...).", false), lr(func(r *longRow) any { return r.RefID })},
		{column("frame_index", tInt64, "Index of the frame within the query's result.", false), lr(func(r *longRow) any { return r.FrameIndex })},
		{column("frame_name", tString, "Frame name (often the series name); NULL when unnamed.", true), lr(func(r *longRow) any { return nonEmpty(r.FrameName) })},
		{column("frame_type", tString, "Dataplane frame type, e.g. 'timeseries-multi', 'numeric-long', 'table'; NULL when the data source does not say.", true), lr(func(r *longRow) any { return nonEmpty(r.FrameType) })},
		{column("field", tString, "Field (column) name, or its display name from the data source.", false), lr(func(r *longRow) any { return r.Field })},
		{column("labels", tMap, "The field's labels (e.g. Prometheus series labels); empty when none.", false), lr(func(r *longRow) any { return r.Labels })},
		{column("row_index", tInt64, "Row number within the frame (0-based).", false), lr(func(r *longRow) any { return r.RowIndex })},
		{column("ts", tTS, "Value of the frame's first time field at this row; NULL for frames without one.", true), lr(func(r *longRow) any { return r.TS })},
		{column("value", tFloat64, "Numeric value (NaN and Inf kept; booleans as 1/0); NULL for strings and nulls.", true), lr(func(r *longRow) any { return r.Value })},
		{column("string_value", tString, "String value for string, boolean, enum and extra time fields; NULL for numbers.", true), lr(func(r *longRow) any { return r.Str })},
		{column("unit", tString, "The field's unit from its config, e.g. 'ms', 'percent'.", true), lr(func(r *longRow) any { return r.Unit })},
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var a dsQueryArgs
		if err := s.args(&a); err != nil {
			return nil, err
		}
		qs, err := a.queries()
		if err != nil {
			return nil, err
		}
		body := map[string]any{
			"queries": qs,
			"from":    strconv.FormatInt(a.StartTime.UnixMilli(), 10),
			"to":      strconv.FormatInt(a.EndTime.UnixMilli(), 10),
		}
		var resp struct {
			Results map[string]struct {
				Status int         `json:"status"`
				Error  string      `json:"error"`
				Frames []dataFrame `json:"frames"`
			} `json:"results"`
		}
		s.St.Done = true
		httpErr := s.C.post(ctx, "/api/ds/query", body, &resp)
		if httpErr != nil {
			// A failed query comes back as 4xx/5xx with the per-refId errors.
			var ae *apiError
			if !errors.As(httpErr, &ae) || json.Unmarshal(ae.Body, &resp) != nil || len(resp.Results) == 0 {
				return nil, httpErr
			}
		}
		refs := make([]string, 0, len(resp.Results))
		for k := range resp.Results {
			refs = append(refs, k)
		}
		sort.Strings(refs)
		var recs []rec
		for _, ref := range refs {
			res := resp.Results[ref]
			if res.Error != "" {
				return nil, fmt.Errorf("ds_query: query %s on data source %s failed: %s", ref, a.DatasourceUID, res.Error)
			}
			for i := range res.Frames {
				rows, err := decodeFrame(&res.Frames[i], ref, i)
				if err != nil {
					return nil, err
				}
				for j := range rows {
					recs = append(recs, rec{"_lr": &rows[j]})
				}
			}
		}
		if httpErr != nil {
			return nil, httpErr
		}
		return s.Def.rows(recs, s), nil
	},
}
