// annotations: Grafana annotations with time-window pushdown.
//
// GET /api/annotations has a limit but no offset, and its sort order mixes
// start and end times, so a cursor on "the oldest time seen" can skip rows.
// Instead the scan partitions the requested start-time range into windows:
// a window that returns a full page is split at the oldest start on that
// page and both parts are queued, so every annotation is read
// from exactly one window whose page was not truncated. The window stack lives in ScanState.Keys, so the scan
// resumes over HTTP.

package grafana

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
)

const (
	// annotationsMinMs / annotationsMaxMs bound a scan with no time filter
	// (Grafana ignores from/to unless both are positive).
	annotationsMinMs = int64(1)
	annotationsMaxMs = int64(4102444800000) // 2100-01-01
)

// maxAnnotationLimit bounds the per-request limit when one millisecond
// holds (or overlaps) more annotations than a page.
const maxAnnotationLimit = 1_000_000

// window encodes a start-time window [lo, hi] (ms, inclusive) and the
// request limit to use for it (0 = the page size).
func window(lo, hi int64, limit int) string {
	return strconv.FormatInt(lo, 10) + ":" + strconv.FormatInt(hi, 10) + ":" + strconv.Itoa(limit)
}

func parseWindow(s string) (lo, hi int64, limit int) {
	parts := strings.Split(s, ":")
	lo, _ = strconv.ParseInt(parts[0], 10, 64)
	hi, _ = strconv.ParseInt(parts[1], 10, 64)
	if len(parts) > 2 {
		limit, _ = strconv.Atoi(parts[2])
	}
	return lo, hi, limit
}

var annotationsDef = &tableDef{
	Name:    "annotations",
	Catalog: true,
	Docs: fnDocs{
		Title:    "Annotations",
		Category: "annotations",
		Keywords: "annotations, events, deploys, incidents, regions, alert annotations, timeline",
		Description: "Annotations (events and time regions on dashboards or the whole org, plus alert state annotations) " +
			"with time-range, dashboard, panel, tag and type pushdown",
		DocLLM: "One row per annotation (GET /api/annotations). time is the start, time_end the end (equal for a point event; " +
			"is_region when they differ). type is 'annotation' (user / API events) or 'alert' (alert state changes, with prev_state / " +
			"new_state / alert_name). dashboard_uid / panel_id are NULL for org-wide annotations. Pushdown: time >= / > / < / <= / = / " +
			"BETWEEN -> from/to window on the start time; dashboard_uid = 'x' -> dashboardUID; panel_id = n -> panelId; type = 'alert' | " +
			"'annotation' -> type; list_contains(tags, 'x') -> tags (several must all match). Without a time filter every annotation is " +
			"read. The API has no offset, so a window that returns a full page (the secret's PAGE_SIZE, default 1000) is split at the oldest start on that page " +
			"and re-read until no page is truncated (a window crowded by overlapping regions is re-read with a larger limit).",
		DocMD: "## annotations\n\nDeploys, incidents and alert events as rows, with the time window pushed to Grafana.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT time, text, tags FROM grafana.main.annotations WHERE list_contains(tags, 'deploy') AND time >= TIMESTAMPTZ '2026-01-01 00:00:00+00' ORDER BY time",
				Description: "Deploy annotations since a date (tag and window pushed to Grafana)"},
			{SQL: "SELECT d.title, count(*) AS annotations FROM grafana.main.annotations a JOIN grafana.main.dashboards d ON d.id = a.dashboard_uid WHERE a.type = 'annotation' GROUP BY d.title ORDER BY annotations DESC, d.title",
				Description: "Annotations per dashboard"},
		},
	},
	Cols: []col{
		cTool(),
		{column("id", tInt64, "Annotation id.", false), func(r rec, _ *scan) any { return intOr(i64(r, "id")) }},
		cFn("type", tString, "'alert' for alert state annotations, 'annotation' otherwise.", func(r rec, _ *scan) any {
			if intOr(i64(r, "alertId")) > 0 {
				return "alert"
			}
			return "annotation"
		}),
		cFn("time", tTS, "Start time.", func(r rec, _ *scan) any { return msTime(r, "time") }),
		cFn("time_end", tTS, "End time (equal to time for a point event).", func(r rec, _ *scan) any {
			if v := msTime(r, "timeEnd"); v != nil {
				return v
			}
			return msTime(r, "time")
		}),
		cFn("is_region", tBool, "A time region (end after start) rather than a point event.", func(r rec, _ *scan) any {
			e, ok := path(r, "timeEnd").(float64)
			t, _ := path(r, "time").(float64)
			return ok && e > t
		}),
		cStr("text", "Annotation text.", "text"),
		cList("tags", "Tags.", "tags"),
		cStr("dashboard_uid", "UID of the dashboard it belongs to; NULL for an org-wide annotation.", "dashboardUID"),
		cFn("panel_id", tInt64, "Panel id it belongs to; NULL when not panel-specific.", func(r rec, _ *scan) any {
			if n := intOr(i64(r, "panelId")); n != 0 {
				return n
			}
			return nil
		}),
		cFn("dashboard_id", tInt64, "Numeric dashboard id; NULL for an org-wide annotation.", func(r rec, _ *scan) any {
			if n := intOr(i64(r, "dashboardId")); n != 0 {
				return n
			}
			return nil
		}),
		cStr("login", "Login of the author (NULL for alert annotations).", "login"),
		cFn("alert_id", tInt64, "Alert rule id for alert annotations.", func(r rec, _ *scan) any {
			if n := intOr(i64(r, "alertId")); n != 0 {
				return n
			}
			return nil
		}),
		cStr("alert_name", "Alert rule name for alert annotations.", "alertName"),
		cStr("prev_state", "Previous alert state (alert annotations).", "prevState"),
		cStr("new_state", "New alert state (alert annotations).", "newState"),
		cFn("created_at", tTS, "When the annotation was created.", func(r rec, _ *scan) any { return msTime(r, "created") }),
		cFn("updated_at", tTS, "When it was last updated.", func(r rec, _ *scan) any { return msTime(r, "updated") }),
		cJSON("data", "Extra data (alert evaluation values) as JSON.", "data"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		if s.St.Cursor == "" {
			lo, hi := annotationsMinMs, annotationsMaxMs
			l, okLo, h, okHi := timeBounds(s.Preds, "time")
			if okLo {
				lo = max(lo, floorDiv(l, 1000))
			}
			if okHi {
				hi = min(hi, ceilDiv(h, 1000))
			}
			s.St.Cursor = "started"
			if lo > hi {
				s.St.Done = true
				return nil, nil
			}
			s.St.Keys = []string{window(lo, hi, 0)}
		}
		if len(s.St.Keys) == 0 {
			s.St.Done = true
			return nil, nil
		}
		last := len(s.St.Keys) - 1
		lo, hi, limit := parseWindow(s.St.Keys[last])
		s.St.Keys = s.St.Keys[:last]
		if limit == 0 {
			limit = s.C.PageSize
		}
		p := url.Values{"from": {strconv.FormatInt(lo, 10)}, "to": {strconv.FormatInt(hi, 10)}, "limit": {strconv.Itoa(limit)}}
		if v := eqValues(s.Preds, "dashboard_uid"); len(v) == 1 {
			p.Set("dashboardUID", v[0])
		}
		if v := eqValues(s.Preds, "panel_id"); len(v) == 1 {
			p.Set("panelId", v[0])
		}
		if v := eqValues(s.Preds, "type"); len(v) == 1 && (v[0] == "alert" || v[0] == "annotation") {
			p.Set("type", v[0])
		}
		if v := listContains(s.Preds, "tags"); v != nil {
			p["tags"] = v
		}
		var list []rec
		if err := s.C.get(ctx, "/api/annotations", p, &list); err != nil {
			return nil, err
		}
		if len(list) >= limit {
			// Truncated page. Regions that start before the window but
			// overlap it take page slots too: when they fill most of the
			// page, re-read the window with a larger limit; otherwise split
			// it at the oldest start on the page (the page holds roughly the
			// newest annotations).
			// The older part goes on top of the stack and is read first.
			inWindow, oldest := 0, hi+1
			for _, a := range list {
				if t, ok := a["time"].(float64); ok && int64(t) >= lo && int64(t) <= hi {
					inWindow++
					oldest = min(oldest, int64(t))
				}
			}
			if inWindow*2 < len(list) || hi <= lo {
				if limit >= maxAnnotationLimit {
					return nil, fmt.Errorf("grafana /api/annotations: more than %d annotations overlap %s .. %s",
						limit, time.UnixMilli(lo).UTC().Format(time.RFC3339Nano), time.UnixMilli(hi).UTC().Format(time.RFC3339Nano))
				}
				s.St.Keys = append(s.St.Keys, window(lo, hi, min(limit*4, maxAnnotationLimit)))
				return nil, nil
			}
			mid := oldest - 1
			if oldest == lo {
				// The oldest start is the window's first millisecond: read
				// that millisecond on its own (with a larger limit if it is
				// full) and the rest separately.
				mid = lo
			}
			s.St.Keys = append(s.St.Keys, window(mid+1, hi, 0), window(lo, mid, 0))
			return nil, nil
		}
		var kept []rec
		for _, a := range list {
			t, _ := a["time"].(float64)
			if int64(t) >= lo && int64(t) <= hi {
				kept = append(kept, a)
			}
		}
		if len(s.St.Keys) == 0 {
			s.St.Done = true
		}
		return s.Def.rows(kept, s), nil
	},
}
