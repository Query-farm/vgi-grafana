// BI-inventory tables in the shared vocabulary of the sibling BI workers
// (vgi-looker, vgi-tableau, vgi-metabase): dashboards, folders, users and
// connections (Grafana data sources), plus the search table function.
//
// Shared columns: tool, id, title, folder_path, owner, created_at,
// updated_at, last_viewed_at, view_count, url.

package grafana

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
)

const toolName = "grafana"

func cTool() col {
	return cConst("tool", "Always 'grafana': the BI tool, for UNION ALL BY NAME with vgi-looker, vgi-tableau and vgi-metabase tables.", toolName)
}

// ---- search paging ---------------------------------------------------------------

// searchPage requests one page of /api/search with base parameters and
// advances s.St.Page (or marks the scan done on a short page).
func searchPage(ctx context.Context, s *scan, base url.Values) ([]rec, error) {
	p := url.Values{}
	for k, v := range base {
		p[k] = v
	}
	p.Set("limit", strconv.Itoa(s.C.PageSize))
	p.Set("page", strconv.Itoa(s.St.Page))
	var hits []rec
	if err := s.C.get(ctx, "/api/search", p, &hits); err != nil {
		return nil, err
	}
	if len(hits) < s.C.PageSize {
		s.St.Done = true
	} else {
		s.St.Page++
	}
	return hits, nil
}

// searchPushdown turns pinned columns into /api/search parameters.
func searchPushdown(preds []*expr, p url.Values, uidCol, folderCol, tagsCol, starredCol string) {
	if v := eqValues(preds, uidCol); v != nil {
		p["dashboardUIDs"] = v
	}
	if folderCol != "" {
		if v := eqValues(preds, folderCol); v != nil {
			p["folderUIDs"] = v
		}
	}
	if tagsCol != "" {
		if v := listContains(preds, tagsCol); v != nil {
			sort.Strings(v)
			p["tag"] = v
		}
	}
	if starredCol != "" {
		if v := eqValues(preds, starredCol); len(v) == 1 && v[0] == "true" {
			p.Set("starred", "true")
		}
	}
}

// ---- dashboards ------------------------------------------------------------------

// dashboardDetailCols need GET /api/dashboards/uid/<uid> per dashboard.
var dashboardDetailCols = []string{"owner", "updated_by", "created_at", "updated_at", "version", "schema_version",
	"panel_count", "refresh", "time_from", "time_to", "description", "provisioned", "dashboard_json"}

func dashDetail(r rec, keys ...string) any {
	return path(r, append([]string{"_detail"}, keys...)...)
}

var dashboardsDef = &tableDef{
	Name:    "dashboards",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:    "Grafana Dashboards",
		Category: "content",
		Keywords: "dashboards, inventory, bi, folders, tags, owner, starred, search",
		Description: "Every dashboard the credential can see, in the shared BI inventory shape (tool, id, title, folder_path, owner, " +
			"created_at, updated_at, last_viewed_at, view_count, url) plus Grafana's tags, version and panel count",
		DocLLM: "One row per Grafana dashboard (GET /api/search?type=dash-db, then GET /api/dashboards/uid/<uid> only when a detail " +
			"column is selected: owner, updated_by, created_at, updated_at, version, schema_version, panel_count, refresh, time_from, time_to, " +
			"description, provisioned, dashboard_json). id is the dashboard UID. folder_path is the folder chain from the root ('/' = General). " +
			"last_viewed_at and view_count are NULL on Grafana OSS (no view statistics API). Pushdown: id = / IN -> dashboardUIDs, " +
			"folder_uid = / IN -> folderUIDs, list_contains(tags, 'x') -> tag, is_starred = true -> starred. Use UNION ALL BY NAME with " +
			"looker.main.dashboards / metabase.main.dashboards for a cross-tool inventory; use grafana.main.panels for panels and queries.",
		DocMD: "## dashboards\n\nEvery dashboard, one row each, in the shared BI inventory columns plus Grafana specifics. " +
			"Detail columns cost one extra request per dashboard and are fetched only when selected.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT folder_path, count(*) AS dashboards FROM grafana.main.dashboards GROUP BY folder_path ORDER BY dashboards DESC, folder_path",
				Description: "How many dashboards each folder holds"},
			{SQL: "SELECT title, owner, updated_at, panel_count FROM grafana.main.dashboards WHERE list_contains(tags, 'ops') ORDER BY updated_at DESC",
				Description: "Dashboards tagged 'ops' (the tag filter is sent to the search API) with owner and size"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("id", "Dashboard UID (stable across instances; the key for panels, versions and annotations).", "uid"),
		cStr("title", "Dashboard title.", "title"),
		cStr("folder_uid", "UID of the containing folder; NULL in the General (root) folder.", "folderUid"),
		cFn("folder_title", tString, "Title of the containing folder ('General' at the root).", func(r rec, s *scan) any {
			return folderTitle(s.St.Folders, strOrEmpty(r, "folderUid"))
		}),
		cFolderPath("Folder chain from the root, e.g. '/Operations/Web'; '/' for the General folder.", "folderUid"),
		cList("tags", "Dashboard tags.", "tags"),
		cBool("is_starred", "Starred by the credential's user.", "isStarred"),
		cFn("owner", tString, "Login of the user who created the dashboard (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "meta", "createdBy")) }),
		cFn("updated_by", tString, "Login of the last user to save it (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "meta", "updatedBy")) }),
		cFn("created_at", tTS, "Creation time (detail column).", func(r rec, _ *scan) any { return parseTime(dashDetail(r, "meta", "created")) }),
		cFn("updated_at", tTS, "Last save time (detail column).", func(r rec, _ *scan) any { return parseTime(dashDetail(r, "meta", "updated")) }),
		cFn("last_viewed_at", tTS, "Last view time: NULL, Grafana OSS has no view statistics API.", func(rec, *scan) any { return nil }),
		cFn("view_count", tInt64, "Lifetime views: NULL, Grafana OSS has no view statistics API.", func(rec, *scan) any { return nil }),
		cURL("Link to the dashboard in the Grafana UI.", "url"),
		cInt("dashboard_id", "Numeric dashboard id (instance-local; prefer id/UID).", "id"),
		cFn("version", tInt64, "Saved version number (detail column).", func(r rec, _ *scan) any { return i64(r, "_detail", "meta", "version") }),
		cFn("schema_version", tInt64, "Dashboard JSON schema version (detail column).", func(r rec, _ *scan) any { return i64(r, "_detail", "dashboard", "schemaVersion") }),
		cFn("panel_count", tInt64, "Number of panels, rows and panels inside collapsed rows included (detail column).", func(r rec, _ *scan) any {
			d, ok := dashDetail(r, "dashboard").(map[string]any)
			if !ok {
				return nil
			}
			return int64(len(flattenPanels(d)))
		}),
		cFn("refresh", tString, "Auto-refresh interval, e.g. '30s'; NULL when off (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "dashboard", "refresh")) }),
		cFn("time_from", tString, "Default time range start, e.g. 'now-6h' (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "dashboard", "time", "from")) }),
		cFn("time_to", tString, "Default time range end, e.g. 'now' (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "dashboard", "time", "to")) }),
		cFn("description", tString, "Dashboard description (detail column).", func(r rec, _ *scan) any { return nonEmpty(dashDetail(r, "dashboard", "description")) }),
		cFn("provisioned", tBool, "Provisioned from a file or Git rather than saved in the UI (detail column).", func(r rec, _ *scan) any {
			if b, ok := dashDetail(r, "meta", "provisioned").(bool); ok {
				return b
			}
			return nil
		}),
		cFn("dashboard_json", tJSON, "The full dashboard model (detail column).", func(r rec, _ *scan) any { return jsonOf(r, "_detail", "dashboard") }),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		p := url.Values{"type": {"dash-db"}}
		searchPushdown(s.Preds, p, "id", "folder_uid", "tags", "is_starred")
		hits, err := searchPage(ctx, s, p)
		if err != nil {
			return nil, err
		}
		if s.needs(dashboardDetailCols...) {
			for _, h := range hits {
				var d rec
				if err := s.C.get(ctx, "/api/dashboards/uid/"+url.PathEscape(strOrEmpty(h, "uid")), nil, &d); err != nil {
					return nil, err
				}
				h["_detail"] = d
			}
		}
		return s.Def.rows(hits, s), nil
	},
}

func nonEmpty(v any) any {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return nil
}

// ---- folders ---------------------------------------------------------------------

var folderDetailCols = []string{"owner", "updated_by", "created_at", "updated_at", "version"}

var foldersDef = &tableDef{
	Name:    "folders",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:       "Grafana Folders",
		Category:    "content",
		Keywords:    "folders, nested folders, folder tree, path, inventory, bi",
		Description: "Every folder (nested folders included) with its parent, depth and full folder_path, in the shared BI inventory shape",
		DocLLM: "One row per Grafana folder (GET /api/search?type=dash-folder; GET /api/folders/<uid> only when owner, updated_by, created_at, " +
			"updated_at or version is selected). folder_path is the chain from the root including the folder itself, e.g. '/Operations/Web'; " +
			"depth is 1 for top-level folders; parent_uid is NULL at the top. The General (root) folder is not a row. Filters are applied in the worker (the folder list is small).",
		DocMD: "## folders\n\nThe folder tree flattened: one row per folder with `parent_uid`, `depth` and `folder_path`.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT folder_path, depth FROM grafana.main.folders ORDER BY folder_path", Description: "The folder tree as paths"},
			{SQL: "SELECT f.folder_path, count(d.id) AS dashboards FROM grafana.main.folders f LEFT JOIN grafana.main.dashboards d ON d.folder_uid = f.id GROUP BY f.folder_path ORDER BY dashboards DESC, f.folder_path",
				Description: "Folders with their dashboard counts, empty folders included"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("id", "Folder UID.", "uid"),
		cStr("title", "Folder title.", "title"),
		cStr("parent_uid", "UID of the parent folder; NULL for a top-level folder.", "folderUid"),
		cFn("folder_path", tString, "Folder chain from the root including this folder, e.g. '/Operations/Web'.", func(r rec, s *scan) any {
			return folderPath(s.St.Folders, strOrEmpty(r, "uid"))
		}),
		cFn("depth", tInt64, "Nesting depth: 1 for a top-level folder.", func(r rec, s *scan) any {
			p, ok := folderPath(s.St.Folders, strOrEmpty(r, "uid")).(string)
			if !ok {
				return nil
			}
			return int64(strings.Count(p, "/"))
		}),
		cFn("owner", tString, "Login of the user who created the folder (detail column).", func(r rec, _ *scan) any { return nonEmpty(path(r, "_detail", "createdBy")) }),
		cFn("updated_by", tString, "Login of the last user to change it (detail column).", func(r rec, _ *scan) any { return nonEmpty(path(r, "_detail", "updatedBy")) }),
		cFn("created_at", tTS, "Creation time (detail column).", func(r rec, _ *scan) any { return parseTime(path(r, "_detail", "created")) }),
		cFn("updated_at", tTS, "Last change time (detail column).", func(r rec, _ *scan) any { return parseTime(path(r, "_detail", "updated")) }),
		cFn("version", tInt64, "Folder version (detail column).", func(r rec, _ *scan) any { return i64(r, "_detail", "version") }),
		cURL("Link to the folder in the Grafana UI.", "url"),
		cInt("folder_id", "Numeric folder id (instance-local).", "id"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		p := url.Values{"type": {"dash-folder"}}
		hits, err := searchPage(ctx, s, p)
		if err != nil {
			return nil, err
		}
		if s.needs(folderDetailCols...) {
			for _, h := range hits {
				var d rec
				if err := s.C.get(ctx, "/api/folders/"+url.PathEscape(strOrEmpty(h, "uid")), nil, &d); err != nil {
					return nil, err
				}
				h["_detail"] = d
			}
		}
		return s.Def.rows(hits, s), nil
	},
}

// ---- users -----------------------------------------------------------------------

var usersDef = &tableDef{
	Name:    "users",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Grafana Users",
		Category:    "access",
		Keywords:    "users, org users, roles, last seen, logins, access review",
		Description: "Users of the current organization with their org role, last-seen time and disabled flag",
		DocLLM: "One row per user of the credential's current organization (GET /api/org/users/search, paged; needs org admin or users:read). " +
			"id is the numeric user id as text (BI shared shape), uid the stable user UID. role is the org role: Admin, Editor, Viewer or None. " +
			"last_seen_at is the last activity time. Pushdown: login = 'x' -> query. Join team_members on user_id.",
		DocMD: "## users\n\nOrganization users: login, name, email, org role, last seen and disabled state.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT role, count(*) AS users FROM grafana.main.users GROUP BY role ORDER BY users DESC, role", Description: "Users per org role"},
			{SQL: "SELECT login, display_name, last_seen_at FROM grafana.main.users WHERE NOT is_disabled ORDER BY last_seen_at NULLS FIRST LIMIT 20",
				Description: "Least recently active enabled users (access review)"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("id", "Numeric user id, as text (shared BI shape).", "userId"),
		cInt("user_id", "Numeric user id.", "userId"),
		cStr("uid", "User UID.", "uid"),
		cStr("login", "Login name.", "login"),
		cStr("display_name", "Display name.", "name"),
		cStr("email", "Email address.", "email"),
		cStr("role", "Org role: Admin, Editor, Viewer or None.", "role"),
		cFn("last_seen_at", tTS, "Last activity time; NULL when the user never signed in (Grafana reports those as 10 years ago).", func(r rec, _ *scan) any {
			if strOrEmpty(r, "lastSeenAtAge") == "10 years" {
				return nil
			}
			return ts(r, "lastSeenAt")
		}),
		cBool("is_disabled", "The account is disabled.", "isDisabled"),
		cList("auth_labels", "External auth providers the user signed in with (e.g. 'OAuth', 'LDAP').", "authLabels"),
		cBool("is_externally_synced", "Role managed by an external identity provider.", "isExternallySynced"),
		cInt("org_id", "Organization id.", "orgId"),
		cFn("url", tString, "Link to the user's admin page in the Grafana UI.", func(r rec, s *scan) any {
			if uid := strOrEmpty(r, "uid"); uid != "" {
				return webURL(s.C, "/admin/users/edit/"+url.PathEscape(uid))
			}
			return nil
		}),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		p := url.Values{"perpage": {strconv.Itoa(s.C.PageSize)}, "page": {strconv.Itoa(s.St.Page)}}
		if v := eqValues(s.Preds, "login"); len(v) == 1 {
			p.Set("query", v[0])
		}
		var resp struct {
			OrgUsers []rec `json:"orgUsers"`
		}
		if err := s.C.get(ctx, "/api/org/users/search", p, &resp); err != nil {
			return nil, err
		}
		pageDone(s, len(resp.OrgUsers))
		return s.Def.rows(resp.OrgUsers, s), nil
	},
}

// pageDone advances a page/perpage cursor.
func pageDone(s *scan, n int) {
	if n < s.C.PageSize {
		s.St.Done = true
	} else {
		s.St.Page++
	}
}

// ---- connections (data sources) ----------------------------------------------------

var secretKey = regexp.MustCompile(`(?i)(pass|secret|token|apikey|api_key|credential|private)`)

// maskJSON replaces values under secret-looking keys with "[REDACTED]".
func maskJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if secretKey.MatchString(k) {
				if _, isBool := val.(bool); !isBool {
					out[k] = "[REDACTED]"
					continue
				}
			}
			out[k] = maskJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, it := range x {
			out[i] = maskJSON(it)
		}
		return out
	}
	return v
}

var connectionsDef = &tableDef{
	Name:    "connections",
	Catalog: true,
	Docs: fnDocs{
		Title:    "Grafana Data Sources",
		Category: "content",
		Keywords: "data sources, datasources, connections, prometheus, loki, postgres, plugins, endpoints",
		Description: "Configured data sources (the BI 'connections'): type, endpoint, database and access mode; " +
			"credentials, secure fields and URL user info are never returned",
		DocLLM: "One row per data source (GET /api/datasources; needs datasources:read, normally Admin). id is the data source UID (use it " +
			"in ds_query and to join panels.datasource_uid), title its name, type the plugin id (prometheus, loki, grafana-postgresql-datasource, ...). " +
			"endpoint is the configured URL with any user:password@, query string and fragment removed; database comes from the database field " +
			"or jsonData.database. json_data is the plugin's non-secret settings with secret-looking keys masked. Passwords, " +
			"secureJsonData and basic-auth passwords are never returned (Grafana's list API does not return them either). created_at is NULL " +
			"(Grafana does not expose it).",
		DocMD: "## connections\n\nData sources in the shared BI `connections` shape. No secret ever leaves the worker: URL user info and query strings " +
			"are stripped and secret-looking `jsonData` keys masked.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT type, count(*) AS sources FROM grafana.main.connections GROUP BY type ORDER BY sources DESC, type", Description: "Data sources per plugin type"},
			{SQL: "SELECT c.title, count(p.panel_id) AS panels FROM grafana.main.connections c LEFT JOIN grafana.main.panels p ON p.datasource_uid = c.id GROUP BY c.title ORDER BY panels, c.title",
				Description: "Panels per data source, unused data sources first"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("id", "Data source UID (use in ds_query; joins panels.datasource_uid).", "uid"),
		cStr("title", "Data source name.", "name"),
		cFn("created_at", tTS, "Creation time: NULL, Grafana does not expose it.", func(rec, *scan) any { return nil }),
		cFn("url", tString, "Link to the data source settings in the Grafana UI.", func(r rec, s *scan) any {
			if uid := strOrEmpty(r, "uid"); uid != "" {
				return webURL(s.C, "/connections/datasources/edit/"+url.PathEscape(uid))
			}
			return nil
		}),
		cInt("datasource_id", "Numeric data source id (instance-local).", "id"),
		cStr("type", "Plugin id, e.g. 'prometheus', 'loki', 'grafana-postgresql-datasource'.", "type"),
		cStr("type_name", "Plugin display name, e.g. 'PostgreSQL'.", "typeName"),
		cStr("access", "Access mode: 'proxy' (server) or 'direct' (browser).", "access"),
		cFn("endpoint", tString, "Configured URL with user info, query string and fragment removed.", func(r rec, _ *scan) any {
			return nonEmpty(maskURL(strOrEmpty(r, "url")))
		}),
		cFn("database", tString, "Database name (database field or jsonData.database).", func(r rec, _ *scan) any {
			if d := str(r, "database"); d != nil {
				return d
			}
			return str(r, "jsonData", "database")
		}),
		cStr("user", "Database / service user name configured on the data source (not a secret; passwords are never returned).", "user"),
		cBool("basic_auth", "Basic authentication is enabled (the password is never returned).", "basicAuth"),
		cBool("is_default", "The organization's default data source.", "isDefault"),
		cBool("read_only", "Provisioned and not editable in the UI.", "readOnly"),
		cFn("json_data", tJSON, "Plugin settings (jsonData) with secret-looking keys masked as [REDACTED].", func(r rec, _ *scan) any {
			v := path(r, "jsonData")
			if v == nil {
				return nil
			}
			return jsonOf(rec{"v": maskJSON(v)}, "v")
		}),
		cInt("org_id", "Organization id.", "orgId"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var list []rec
		if err := s.C.get(ctx, "/api/datasources", nil, &list); err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(list, s), nil
	},
}

// ---- search ----------------------------------------------------------------------

type searchArgs struct {
	Query      *string `vgi:"name=query,doc=Free-text title search (case-insensitive substring)."`
	Tags       *string `vgi:"name=tags,doc=Comma-separated tags; a dashboard must carry all of them."`
	Type       *string `vgi:"name=type,choices='dashboard,folder',doc=Restrict to 'dashboard' or 'folder' hits."`
	FolderUIDs *string `vgi:"name=folder_uids,doc=Comma-separated folder UIDs to search in."`
	Starred    *bool   `vgi:"name=starred,doc=Only items starred by the credential's user."`
	SecretName *string `vgi:"name=secret_name,doc=Name of the grafana secret to use when several exist (default: the ATTACH secret_name option or the default secret)."`
}

func (a *searchArgs) secret() string { return deref(a.SecretName) }

func splitCSV(s *string) []string {
	var out []string
	for _, p := range strings.Split(deref(s), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var searchDef = &tableDef{
	Name:    "search",
	Args:    &searchArgs{},
	Folders: true,
	Docs: fnDocs{
		Title:       "Search Dashboards And Folders",
		Category:    "content",
		Keywords:    "search, find dashboards, query, tags, starred, folders",
		Description: "Grafana's dashboard and folder search, with its query, tag, type, folder and starred filters as arguments",
		DocLLM: "Run Grafana's search (GET /api/search). Named args: query (title substring), tags (comma-separated, all must match), " +
			"type ('dashboard' or 'folder'), folder_uids (comma-separated), starred (BOOLEAN), secret_name. Columns: kind ('dashboard' | 'folder'), " +
			"uid, title, folder_uid, folder_title, folder_path, tags, is_starred, url, id. WHERE kind / uid / folder_uid / is_starred / " +
			"list_contains(tags, 'x') are pushed into the same parameters. Use grafana.main.dashboards for the full inventory with owners and dates.",
		DocMD: "## search(query := ..., tags := ..., type := ..., folder_uids := ..., starred := ...)\n\nGrafana's search box as a table.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT kind, title, folder_path FROM grafana.main.search(query := 'web') ORDER BY kind, title", Description: "Everything whose title contains 'web'"},
			{SQL: "SELECT title, url FROM grafana.main.search(type := 'dashboard', starred := true) ORDER BY title", Description: "My starred dashboards"},
		},
	},
	Cols: []col{
		cFn("kind", tString, "'dashboard' or 'folder'.", func(r rec, _ *scan) any {
			switch strOrEmpty(r, "type") {
			case "dash-db":
				return "dashboard"
			case "dash-folder":
				return "folder"
			}
			return str(r, "type")
		}),
		cKey("uid", "Dashboard or folder UID.", "uid"),
		cStr("title", "Title.", "title"),
		cStr("folder_uid", "UID of the containing folder (the parent, for a folder); NULL at the root.", "folderUid"),
		cFn("folder_title", tString, "Title of the containing folder ('General' at the root).", func(r rec, s *scan) any {
			return folderTitle(s.St.Folders, strOrEmpty(r, "folderUid"))
		}),
		cFolderPath("Path of the containing folder; '/' at the root.", "folderUid"),
		cList("tags", "Tags (dashboards only).", "tags"),
		cBool("is_starred", "Starred by the credential's user.", "isStarred"),
		cURL("Link in the Grafana UI.", "url"),
		cInt("id", "Numeric id (instance-local).", "id"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var a searchArgs
		if err := s.args(&a); err != nil {
			return nil, err
		}
		p := url.Values{}
		if q := strings.TrimSpace(deref(a.Query)); q != "" {
			p.Set("query", q)
		}
		tags := splitCSV(a.Tags)
		tags = append(tags, listContains(s.Preds, "tags")...)
		if tags != nil {
			sort.Strings(tags)
			p["tag"] = tags
		}
		kind := deref(a.Type)
		if v := eqValues(s.Preds, "kind"); len(v) == 1 && kind == "" {
			kind = v[0]
		}
		switch kind {
		case "dashboard":
			p.Set("type", "dash-db")
		case "folder":
			p.Set("type", "dash-folder")
		}
		folders := splitCSV(a.FolderUIDs)
		if folders == nil {
			folders = eqValues(s.Preds, "folder_uid")
		}
		if folders != nil {
			p["folderUIDs"] = folders
		}
		if v := eqValues(s.Preds, "uid"); v != nil {
			p["dashboardUIDs"] = v
		}
		if (a.Starred != nil && *a.Starred) || func() bool { v := eqValues(s.Preds, "is_starred"); return len(v) == 1 && v[0] == "true" }() {
			p.Set("starred", "true")
		}
		hits, err := searchPage(ctx, s, p)
		if err != nil {
			return nil, err
		}
		return s.Def.rows(hits, s), nil
	},
}
