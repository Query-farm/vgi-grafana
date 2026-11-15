// Dashboard contents: panels, panel_queries, dashboard_versions and
// library_panels. The first three fan out over dashboards: the dashboard UIDs
// to visit (pinned by a dashboard_uid filter, or every dashboard the search
// API returns) are kept in ScanState.Keys, and each Process call reads one
// dashboard, so the scan resumes over HTTP.

package grafana

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Query-farm/vgi-go/vgi"
)

// panelRef is one panel with the collapsed row that contains it, if any.
type panelRef struct {
	P        rec
	RowID    any
	RowTitle any
}

// flattenPanels lists every panel of a dashboard model: top-level panels
// (rows included) and the panels nested inside collapsed rows.
func flattenPanels(d rec) []panelRef {
	var out []panelRef
	items, _ := d["panels"].([]any)
	for _, it := range items {
		p, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, panelRef{P: p})
		if strOrEmpty(p, "type") == "row" {
			kids, _ := p["panels"].([]any)
			for _, k := range kids {
				if kp, ok := k.(map[string]any); ok {
					out = append(out, panelRef{P: kp, RowID: i64(p, "id"), RowTitle: str(p, "title")})
				}
			}
		}
	}
	// Legacy (schemaVersion < 16) dashboards keep panels under rows[].panels.
	rows, _ := d["rows"].([]any)
	for _, it := range rows {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		kids, _ := row["panels"].([]any)
		for _, k := range kids {
			if kp, ok := k.(map[string]any); ok {
				out = append(out, panelRef{P: kp, RowTitle: str(row, "title")})
			}
		}
	}
	return out
}

// datasourceRef reads a panel's or target's datasource: an object
// {type, uid}, or a legacy name string (returned as the uid).
func datasourceRef(v any) (uid, typ any) {
	switch x := v.(type) {
	case map[string]any:
		return str(x, "uid"), str(x, "type")
	case string:
		if x != "" {
			return x, nil
		}
	}
	return nil, nil
}

// nextDashboard collects the dashboard UIDs to visit on the first call, then
// returns one dashboard (its /api/dashboards/uid response) per call; nil when
// the visited UID no longer exists. extra adds query parameters to each
// dashboard request (none today).
func nextDashboard(ctx context.Context, s *scan, uidCol, folderCol string) (rec, error) {
	if s.St.Cursor == "" {
		if v := eqValues(s.Preds, uidCol); v != nil {
			s.St.Keys = v
		} else {
			p := url.Values{"type": {"dash-db"}}
			if folderCol != "" {
				if f := eqValues(s.Preds, folderCol); f != nil {
					p["folderUIDs"] = f
				}
			}
			for page := 1; ; page++ {
				p.Set("limit", strconv.Itoa(s.C.PageSize))
				p.Set("page", strconv.Itoa(page))
				var hits []rec
				if err := s.C.get(ctx, "/api/search", p, &hits); err != nil {
					return nil, err
				}
				for _, h := range hits {
					s.St.Keys = append(s.St.Keys, strOrEmpty(h, "uid"))
				}
				if len(hits) < s.C.PageSize {
					break
				}
			}
		}
		s.St.Cursor = "listed"
	}
	if s.St.KeyIdx >= len(s.St.Keys) {
		s.St.Done = true
		return nil, nil
	}
	uid := s.St.Keys[s.St.KeyIdx]
	s.St.KeyIdx++
	if s.St.KeyIdx >= len(s.St.Keys) {
		s.St.Done = true
	}
	var d rec
	if err := s.C.get(ctx, "/api/dashboards/uid/"+url.PathEscape(uid), nil, &d); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

// dashCtx carries dashboard-level values into panel rows.
func dashCols() []col {
	return []col{
		cTool(),
		cKey("dashboard_uid", "UID of the dashboard holding the panel (joins dashboards.id).", "_dash", "dashboard", "uid"),
		cStr("dashboard_title", "Dashboard title.", "_dash", "dashboard", "title"),
		cStr("folder_uid", "UID of the dashboard's folder; NULL in General.", "_dash", "meta", "folderUid"),
		cFolderPath("Folder chain of the dashboard, e.g. '/Operations/Web'.", "_dash", "meta", "folderUid"),
	}
}

func panelRec(d rec, pr panelRef) rec {
	return rec{"_dash": d, "p": pr.P, "row_id": pr.RowID, "row_title": pr.RowTitle}
}

var panelsDef = &tableDef{
	Name:    "panels",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:    "Dashboard Panels",
		Category: "content",
		Keywords: "panels, visualizations, charts, tiles, datasource usage, library panels, rows",
		Description: "Every panel of every dashboard, flattened (panels inside collapsed rows included), with its type, " +
			"data source, grid position and queries as JSON",
		DocLLM: "One row per panel (GET /api/dashboards/uid/<uid> per dashboard). Rows are panels too (type 'row'); panels inside a " +
			"collapsed row carry row_id / row_title. datasource_uid / datasource_type are the panel's data source ('-- Mixed --' when each " +
			"query picks its own; see panel_queries). library_panel_uid is set for library panel instances (their title/type/targets live in " +
			"library_panels). targets is the query list as JSON. Pushdown: dashboard_uid = / IN visits only those dashboards; folder_uid = / IN " +
			"searches only those folders. Without a dashboard_uid filter every dashboard is read (one request each).",
		DocMD: "## panels\n\nEvery panel, flattened, with data source and queries. Filter on `dashboard_uid` to read one dashboard.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT type, count(*) AS panels FROM grafana.main.panels WHERE type <> 'row' GROUP BY type ORDER BY panels DESC, type",
				Description: "Which visualizations are used most"},
			{SQL: "SELECT dashboard_title, title, datasource_type FROM grafana.main.panels WHERE dashboard_uid = 'web-overview' ORDER BY panel_id",
				Description: "The panels of one dashboard (only that dashboard is read)"},
		},
	},
	Cols: append(dashCols(),
		cInt("panel_id", "Panel id (unique within the dashboard).", "p", "id"),
		cStr("title", "Panel title.", "p", "title"),
		cStr("type", "Visualization plugin id, e.g. 'timeseries', 'stat', 'table', 'row'; NULL for a library panel instance.", "p", "type"),
		cStr("description", "Panel description.", "p", "description"),
		cInt("row_id", "Panel id of the collapsed row that contains this panel; NULL otherwise.", "row_id"),
		cStr("row_title", "Title of the collapsed row that contains this panel.", "row_title"),
		cFn("datasource_uid", tString, "UID of the panel's data source ('-- Mixed --' for mixed; a legacy name for old dashboards).", func(r rec, _ *scan) any {
			u, _ := datasourceRef(path(r, "p", "datasource"))
			return u
		}),
		cFn("datasource_type", tString, "Plugin id of the panel's data source.", func(r rec, _ *scan) any {
			_, t := datasourceRef(path(r, "p", "datasource"))
			return t
		}),
		cStr("library_panel_uid", "UID of the library panel this instance shows (joins library_panels.uid).", "p", "libraryPanel", "uid"),
		cStr("library_panel_name", "Name of the library panel.", "p", "libraryPanel", "name"),
		cInt("grid_x", "Grid column (0-23).", "p", "gridPos", "x"),
		cInt("grid_y", "Grid row.", "p", "gridPos", "y"),
		cInt("grid_w", "Width in grid columns.", "p", "gridPos", "w"),
		cInt("grid_h", "Height in grid rows.", "p", "gridPos", "h"),
		cFn("target_count", tInt64, "Number of queries (targets).", func(r rec, _ *scan) any {
			t, _ := path(r, "p", "targets").([]any)
			return int64(len(t))
		}),
		cJSON("targets", "The panel's queries (targets) as JSON.", "p", "targets"),
		cJSON("panel_json", "The full panel model as JSON.", "p"),
		cFn("url", tString, "Link to the panel (viewPanel) in the Grafana UI.", func(r rec, s *scan) any {
			u := strOrEmpty(r, "_dash", "meta", "url")
			id := i64(r, "p", "id")
			if u == "" || id == nil {
				return nil
			}
			return webURL(s.C, u+"?viewPanel="+strconv.FormatInt(id.(int64), 10))
		}),
	),
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		d, err := nextDashboard(ctx, s, "dashboard_uid", "folder_uid")
		if err != nil || d == nil {
			return nil, err
		}
		model, _ := d["dashboard"].(map[string]any)
		var recs []rec
		for _, pr := range flattenPanels(model) {
			recs = append(recs, panelRec(d, pr))
		}
		return s.Def.rows(recs, s), nil
	},
}

// queryText picks the query string from a target, whatever the data source:
// PromQL/LogQL expr, SQL rawSql, InfluxQL/Flux query, Elasticsearch query,
// a server-side expression, or a TestData scenario id.
func queryText(t rec) any {
	for _, k := range []string{"expr", "rawSql", "query", "expression", "rawQuery", "queryText", "target"} {
		if s, ok := t[k].(string); ok && s != "" {
			return s
		}
	}
	return nil
}

var panelQueriesDef = &tableDef{
	Name:    "panel_queries",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:    "Panel Queries",
		Category: "content",
		Keywords: "queries, targets, promql, sql, expr, rawSql, lineage, datasource usage, metrics used",
		Description: "Every query (target) of every panel, with its effective data source and the query text " +
			"extracted (PromQL/LogQL expr, SQL rawSql, InfluxQL/Flux query)",
		DocLLM: "One row per panel query (target). datasource_uid / datasource_type are the target's own data source when set (mixed " +
			"panels), else the panel's. expr is the PromQL/LogQL expression, raw_sql the SQL text, query_text whichever query string the target " +
			"has (expr, rawSql, query, expression, rawQuery, ...). hidden is the target's hide flag. Library panel instances have no targets here " +
			"(see library_panels.model). Pushdown: dashboard_uid = / IN, folder_uid = / IN. Use it for lineage: which dashboards read a metric " +
			"or a table (WHERE expr LIKE '%http_requests_total%').",
		DocMD: "## panel_queries\n\nQueries behind every panel, one row per target, with the query text pulled out.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT dashboard_title, panel_title, expr FROM grafana.main.panel_queries WHERE datasource_type = 'prometheus' AND expr IS NOT NULL ORDER BY dashboard_title, panel_title",
				Description: "Every PromQL expression on any dashboard"},
			{SQL: "SELECT datasource_uid, count(DISTINCT dashboard_uid) AS dashboards, count(*) AS queries FROM grafana.main.panel_queries GROUP BY datasource_uid ORDER BY queries DESC, datasource_uid",
				Description: "Data source usage across dashboards"},
		},
	},
	Cols: append(dashCols(),
		cInt("panel_id", "Panel id.", "p", "id"),
		cStr("panel_title", "Panel title.", "p", "title"),
		cStr("panel_type", "Visualization plugin id.", "p", "type"),
		cStr("ref_id", "Query reference id (A, B, ...).", "t", "refId"),
		cFn("datasource_uid", tString, "Effective data source UID: the target's own, else the panel's.", func(r rec, _ *scan) any {
			if u, _ := datasourceRef(path(r, "t", "datasource")); u != nil {
				return u
			}
			u, _ := datasourceRef(path(r, "p", "datasource"))
			return u
		}),
		cFn("datasource_type", tString, "Effective data source plugin id.", func(r rec, _ *scan) any {
			if u, t := datasourceRef(path(r, "t", "datasource")); u != nil || t != nil {
				return t
			}
			_, t := datasourceRef(path(r, "p", "datasource"))
			return t
		}),
		cFn("hidden", tBool, "The query is hidden (hide: true) and does not draw.", func(r rec, _ *scan) any {
			b, _ := path(r, "t", "hide").(bool)
			return b
		}),
		cStr("expr", "PromQL / LogQL expression (Prometheus, Loki, Mimir targets).", "t", "expr"),
		cStr("raw_sql", "SQL text (PostgreSQL, MySQL, MSSQL and other SQL targets).", "t", "rawSql"),
		cFn("query_text", tString, "The target's query string, whichever field holds it (expr, rawSql, query, expression, rawQuery, ...).", func(r rec, _ *scan) any {
			t, _ := path(r, "t").(map[string]any)
			return queryText(t)
		}),
		cStr("legend_format", "Legend template, e.g. '{{service}}'.", "t", "legendFormat"),
		cJSON("target_json", "The full target (query model) as JSON.", "t"),
	),
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		d, err := nextDashboard(ctx, s, "dashboard_uid", "folder_uid")
		if err != nil || d == nil {
			return nil, err
		}
		model, _ := d["dashboard"].(map[string]any)
		var recs []rec
		for _, pr := range flattenPanels(model) {
			targets, _ := pr.P["targets"].([]any)
			for _, t := range targets {
				if tm, ok := t.(map[string]any); ok {
					r := panelRec(d, pr)
					r["t"] = tm
					recs = append(recs, r)
				}
			}
		}
		return s.Def.rows(recs, s), nil
	},
}

// ---- dashboard_versions --------------------------------------------------------

var dashboardVersionsDef = &tableDef{
	Name:    "dashboard_versions",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Dashboard Versions",
		Category:    "content",
		Keywords:    "versions, history, audit, changes, who changed, save message",
		Description: "The saved version history of every dashboard: version number, author, time and save message",
		DocLLM: "One row per saved dashboard version (GET /api/dashboards/uid/<uid>/versions, paged with continueToken). Columns: " +
			"dashboard_uid, version, parent_version, restored_from (0 = not a restore), created_at, created_by (login), message. " +
			"Pushdown: dashboard_uid = / IN reads only those dashboards; otherwise every dashboard is visited. Use it for change audits " +
			"(who saved what, when).",
		DocMD: "## dashboard_versions\n\nVersion history per dashboard. Filter on `dashboard_uid` to read one dashboard's history.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT created_by, count(*) AS saves FROM grafana.main.dashboard_versions GROUP BY created_by ORDER BY saves DESC, created_by",
				Description: "Who saves dashboards most"},
			{SQL: "SELECT version, created_by, message FROM grafana.main.dashboard_versions WHERE dashboard_uid = 'web-overview' ORDER BY version",
				Description: "One dashboard's history"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("dashboard_uid", "Dashboard UID (joins dashboards.id).", "uid"),
		cInt("version", "Version number.", "version"),
		cInt("parent_version", "The version this one was saved over.", "parentVersion"),
		cInt("restored_from", "Version this one restored (0 when not a restore).", "restoredFrom"),
		cTS("created_at", "When the version was saved.", "created"),
		cStr("created_by", "Login of the user who saved it.", "createdBy"),
		cStr("message", "Save message.", "message"),
		cInt("version_id", "Numeric version record id.", "id"),
		cInt("dashboard_id", "Numeric dashboard id.", "dashboardId"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		if s.St.Keys == nil && s.St.KeyIdx == 0 {
			if v := eqValues(s.Preds, "dashboard_uid"); v != nil {
				s.St.Keys = v
			} else {
				p := url.Values{"type": {"dash-db"}}
				for page := 1; ; page++ {
					p.Set("limit", strconv.Itoa(s.C.PageSize))
					p.Set("page", strconv.Itoa(page))
					var hits []rec
					if err := s.C.get(ctx, "/api/search", p, &hits); err != nil {
						return nil, err
					}
					for _, h := range hits {
						s.St.Keys = append(s.St.Keys, strOrEmpty(h, "uid"))
					}
					if len(hits) < s.C.PageSize {
						break
					}
				}
			}
			if len(s.St.Keys) == 0 {
				s.St.Done = true
				return nil, nil
			}
		}
		uid := s.St.Keys[s.St.KeyIdx]
		p := url.Values{"limit": {strconv.Itoa(min(s.C.PageSize, 1000))}}
		if s.St.Cursor != "" {
			p.Set("continueToken", s.St.Cursor)
		}
		var raw any
		err := s.C.get(ctx, "/api/dashboards/uid/"+url.PathEscape(uid)+"/versions", p, &raw)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			raw, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
		var versions []any
		next := ""
		switch x := raw.(type) {
		case []any: // Grafana < 11.3
			versions = x
		case map[string]any:
			versions, _ = x["versions"].([]any)
			next, _ = x["continueToken"].(string)
		}
		var recs []rec
		for _, v := range versions {
			if m, ok := v.(map[string]any); ok {
				delete(m, "data")
				if strOrEmpty(m, "uid") == "" {
					m["uid"] = uid
				}
				recs = append(recs, m)
			}
		}
		if next != "" && len(versions) > 0 {
			s.St.Cursor = next
		} else {
			s.St.Cursor = ""
			s.St.KeyIdx++
			if s.St.KeyIdx >= len(s.St.Keys) {
				s.St.Done = true
			}
		}
		return s.Def.rows(recs, s), nil
	},
}

// ---- library_panels --------------------------------------------------------------

var libraryPanelsDef = &tableDef{
	Name:    "library_panels",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:       "Library Panels",
		Category:    "content",
		Keywords:    "library panels, reusable panels, shared panels, library elements",
		Description: "Reusable library panels: name, type, folder, how many dashboards use them and their model",
		DocLLM: "One row per library panel (GET /api/library-elements?kind=1, paged). uid joins panels.library_panel_uid. " +
			"connected_dashboards is how many dashboards embed it. model is the panel model (type, targets, options) as JSON. " +
			"Pushdown: folder_uid = / IN -> folderFilterUIDs, name = 'x' -> searchString.",
		DocMD: "## library_panels\n\nLibrary panels with usage counts.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT name, type, folder_path, connected_dashboards FROM grafana.main.library_panels ORDER BY connected_dashboards DESC, name",
				Description: "Most reused library panels"},
			{SQL: "SELECT l.name, p.dashboard_title FROM grafana.main.library_panels l JOIN grafana.main.panels p ON p.library_panel_uid = l.uid ORDER BY l.name, p.dashboard_title",
				Description: "Where each library panel is used"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("uid", "Library panel UID.", "uid"),
		cStr("name", "Library panel name.", "name"),
		cStr("type", "Visualization plugin id.", "type"),
		cStr("description", "Description.", "description"),
		cStr("folder_uid", "UID of the folder it is stored in; NULL in General.", "folderUid"),
		cFolderPath("Folder chain, e.g. '/Operations'.", "folderUid"),
		cInt("version", "Version number.", "version"),
		cInt("connected_dashboards", "Number of dashboards that embed it.", "meta", "connectedDashboards"),
		cTS("created_at", "Creation time.", "meta", "created"),
		cTS("updated_at", "Last update time.", "meta", "updated"),
		cStr("created_by", "Name of the user who created it.", "meta", "createdBy", "name"),
		cStr("updated_by", "Name of the user who last updated it.", "meta", "updatedBy", "name"),
		cJSON("model", "The panel model as JSON.", "model"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		p := url.Values{"kind": {"1"}, "perPage": {strconv.Itoa(s.C.PageSize)}, "page": {strconv.Itoa(s.St.Page)}}
		if v := eqValues(s.Preds, "folder_uid"); v != nil {
			p.Set("folderFilterUIDs", joinComma(v))
		}
		if v := eqValues(s.Preds, "name"); len(v) == 1 {
			p.Set("searchString", v[0])
		}
		var resp struct {
			Result struct {
				Elements []rec `json:"elements"`
			} `json:"result"`
		}
		if err := s.C.get(ctx, "/api/library-elements", p, &resp); err != nil {
			return nil, err
		}
		pageDone(s, len(resp.Result.Elements))
		return s.Def.rows(resp.Result.Elements, s), nil
	},
}

func joinComma(v []string) string {
	out := ""
	for i, x := range v {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}
