// Access tables: orgs, teams, team_members and service_accounts.

package grafana

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Query-farm/vgi-go/vgi"
)

var orgsDef = &tableDef{
	Name:    "orgs",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Organizations",
		Category:    "access",
		Keywords:    "orgs, organizations, tenants, multi-tenancy",
		Description: "Grafana organizations: every org for a server admin, else the credential's current org",
		DocLLM: "One row per organization. A server admin (basic auth) sees every org (GET /api/orgs, paged); any other credential, " +
			"service account tokens included, gets its current org only (GET /api/org), and is_current tells which row that is. " +
			"Use the org id as the grafana secret's ORG_ID to read another org.",
		DocMD: "## orgs\n\nOrganizations: all of them for a server admin, otherwise the current one.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT org_id, name FROM grafana.main.orgs ORDER BY org_id", Description: "List organizations"},
		},
	},
	Cols: []col{
		cTool(),
		cInt("org_id", "Organization id (the secret's ORG_ID).", "id"),
		cStr("name", "Organization name.", "name"),
		cFn("is_current", tBool, "Listed through /api/org because the credential cannot list every org.", func(r rec, _ *scan) any {
			b, _ := r["_current"].(bool)
			return b
		}),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		// /api/orgs pages from 0 in current Grafana (from 1 in some older
		// releases), so pages are read until one adds no new org.
		page := s.St.Page - 1
		var list []rec
		err := s.C.get(ctx, "/api/orgs", url.Values{"perpage": {strconv.Itoa(s.C.PageSize)}, "page": {strconv.Itoa(page)}}, &list)
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized) && page == 0 {
			var cur rec
			if err := s.C.get(ctx, "/api/org", nil, &cur); err != nil {
				return nil, err
			}
			cur["_current"] = true
			s.St.Done = true
			return s.Def.rows([]rec{cur}, s), nil
		}
		if err != nil {
			return nil, err
		}
		var fresh []rec
		for _, o := range list {
			id := strconv.FormatInt(intOr(i64(o, "id")), 10)
			if containsStr(s.St.Seen, id) {
				continue
			}
			s.St.Seen = append(s.St.Seen, id)
			fresh = append(fresh, o)
		}
		if len(fresh) == 0 || len(list) < s.C.PageSize {
			s.St.Done = true
		} else {
			s.St.Page++
		}
		return s.Def.rows(fresh, s), nil
	},
}

func intOr(v any) int64 {
	n, _ := v.(int64)
	return n
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var teamsDef = &tableDef{
	Name:    "teams",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Teams",
		Category:    "access",
		Keywords:    "teams, groups, members, access",
		Description: "Teams of the current organization with their member counts",
		DocLLM:      "One row per team (GET /api/teams/search, paged). team_id joins team_members.team_id. Pushdown: name = 'x' -> name.",
		DocMD:       "## teams\n\nTeams with member counts.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT name, member_count FROM grafana.main.teams ORDER BY member_count DESC, name", Description: "Largest teams"},
		},
	},
	Cols: []col{
		cTool(),
		cInt("team_id", "Numeric team id.", "id"),
		cStr("uid", "Team UID.", "uid"),
		cStr("name", "Team name.", "name"),
		cStr("email", "Team email.", "email"),
		cInt("member_count", "Number of members.", "memberCount"),
		cBool("is_provisioned", "Provisioned (synced) rather than managed in the UI.", "isProvisioned"),
		cInt("org_id", "Organization id.", "orgId"),
		cFn("url", tString, "Link to the team in the Grafana UI.", func(r rec, s *scan) any {
			if uid := strOrEmpty(r, "uid"); uid != "" {
				return webURL(s.C, "/org/teams/edit/"+url.PathEscape(uid))
			}
			return nil
		}),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		teams, err := teamsPage(ctx, s, s.St.Page)
		if err != nil {
			return nil, err
		}
		pageDone(s, len(teams))
		return s.Def.rows(teams, s), nil
	},
}

func teamsPage(ctx context.Context, s *scan, page int) ([]rec, error) {
	p := url.Values{"perpage": {strconv.Itoa(s.C.PageSize)}, "page": {strconv.Itoa(page)}}
	if v := eqValues(s.Preds, "name"); len(v) == 1 {
		p.Set("name", v[0])
	}
	if v := eqValues(s.Preds, "team_name"); len(v) == 1 {
		p.Set("name", v[0])
	}
	var resp struct {
		Teams []rec `json:"teams"`
	}
	if err := s.C.get(ctx, "/api/teams/search", p, &resp); err != nil {
		return nil, err
	}
	return resp.Teams, nil
}

var teamMembersDef = &tableDef{
	Name:    "team_members",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Team Members",
		Category:    "access",
		Keywords:    "team members, membership, users in teams, team admin",
		Description: "Members of every team, with their login, email and team permission",
		DocLLM: "One row per (team, member) (GET /api/teams/<id>/members per team). permission is 4 for a team admin, 0 for a member " +
			"(is_admin = permission 4). Pushdown: team_id = / IN reads only those teams, team_name = 'x' searches by name.",
		DocMD: "## team_members\n\nTeam membership, one row per member of each team.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT team_name, login, is_admin FROM grafana.main.team_members ORDER BY team_name, login", Description: "Who is in which team"},
			{SQL: "SELECT u.login FROM grafana.main.users u ANTI JOIN grafana.main.team_members m ON m.user_id = u.user_id ORDER BY u.login",
				Description: "Users in no team"},
		},
	},
	Cols: []col{
		cTool(),
		cInt("team_id", "Numeric team id (joins teams.team_id).", "teamId"),
		cStr("team_name", "Team name.", "_team_name"),
		cStr("team_uid", "Team UID.", "teamUID"),
		cInt("user_id", "Numeric user id (joins users.user_id).", "userId"),
		cStr("user_uid", "User UID.", "userUID"),
		cStr("login", "Member login.", "login"),
		cStr("email", "Member email.", "email"),
		cStr("display_name", "Member display name.", "name"),
		cInt("permission", "Team permission: 4 = team admin, 0 = member.", "permission"),
		cFn("is_admin", tBool, "Team admin (permission 4).", func(r rec, _ *scan) any { return intOr(i64(r, "permission")) == 4 }),
		cStr("auth_module", "Auth module that synced the membership, e.g. 'oauth_github'; NULL for manual membership.", "auth_module"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		if s.St.Cursor == "" {
			names := map[string]string{}
			if v := eqValues(s.Preds, "team_id"); v != nil {
				s.St.Keys = v
			} else {
				for page := 1; ; page++ {
					teams, err := teamsPage(ctx, s, page)
					if err != nil {
						return nil, err
					}
					for _, t := range teams {
						id := strconv.FormatInt(intOr(i64(t, "id")), 10)
						s.St.Keys = append(s.St.Keys, id)
						names[id] = strOrEmpty(t, "name")
					}
					if len(teams) < s.C.PageSize {
						break
					}
				}
			}
			s.St.Seen = nil
			for _, k := range s.St.Keys {
				s.St.Seen = append(s.St.Seen, names[k])
			}
			s.St.Cursor = "listed"
		}
		if s.St.KeyIdx >= len(s.St.Keys) {
			s.St.Done = true
			return nil, nil
		}
		id, name := s.St.Keys[s.St.KeyIdx], s.St.Seen[s.St.KeyIdx]
		s.St.KeyIdx++
		if s.St.KeyIdx >= len(s.St.Keys) {
			s.St.Done = true
		}
		if name == "" {
			var t rec
			if err := s.C.get(ctx, "/api/teams/"+url.PathEscape(id), nil, &t); err != nil {
				var ae *apiError
				if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
					return nil, nil
				}
				return nil, err
			}
			name = strOrEmpty(t, "name")
		}
		var members []rec
		if err := s.C.get(ctx, "/api/teams/"+url.PathEscape(id)+"/members", nil, &members); err != nil {
			return nil, err
		}
		for _, m := range members {
			m["_team_name"] = name
		}
		return s.Def.rows(members, s), nil
	},
}

var serviceAccountsDef = &tableDef{
	Name:    "service_accounts",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Service Accounts",
		Category:    "access",
		Keywords:    "service accounts, tokens, automation, api keys, machine users",
		Description: "Service accounts with their role, disabled flag and token count (tokens themselves are never returned)",
		DocLLM: "One row per service account (GET /api/serviceaccounts/search, paged). token_count is how many tokens it has; token " +
			"values are never returned. role is the org role. Pushdown: name = 'x' -> query.",
		DocMD: "## service_accounts\n\nService accounts and how many tokens each holds.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT name, role, token_count FROM grafana.main.service_accounts WHERE NOT is_disabled ORDER BY token_count DESC, name",
				Description: "Enabled service accounts by token count"},
		},
	},
	Cols: []col{
		cTool(),
		cInt("service_account_id", "Numeric id.", "id"),
		cStr("uid", "Service account UID.", "uid"),
		cStr("name", "Name.", "name"),
		cStr("login", "Login (sa-<org>-<name>).", "login"),
		cStr("role", "Org role: Admin, Editor, Viewer or None.", "role"),
		cBool("is_disabled", "Disabled.", "isDisabled"),
		cInt("token_count", "Number of tokens (their values are never returned).", "tokens"),
		cInt("org_id", "Organization id.", "orgId"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		p := url.Values{"perpage": {strconv.Itoa(s.C.PageSize)}, "page": {strconv.Itoa(s.St.Page)}}
		if v := eqValues(s.Preds, "name"); len(v) == 1 {
			p.Set("query", v[0])
		}
		var resp struct {
			ServiceAccounts []rec `json:"serviceAccounts"`
		}
		if err := s.C.get(ctx, "/api/serviceaccounts/search", p, &resp); err != nil {
			return nil, err
		}
		pageDone(s, len(resp.ServiceAccounts))
		return s.Def.rows(resp.ServiceAccounts, s), nil
	},
}

var healthDef = &tableDef{
	Name:    "health",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Server Health",
		Category:    "access",
		Keywords:    "health, version, build, commit, database status",
		Description: "Grafana's health endpoint: version, commit and database status",
		DocLLM:      "One row from GET /api/health: version (e.g. '12.1.1'), commit and database ('ok' when the backing database answers). Needs no permission beyond a valid secret.",
		DocMD:       "## health\n\nServer version and database health.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT version, database FROM grafana.main.health", Description: "Which Grafana version is running, and is its database up"},
		},
	},
	Cols: []col{
		cStr("version", "Grafana version.", "version"),
		cStr("commit", "Build commit.", "commit"),
		cStr("database", "'ok' when the backing database answers.", "database"),
		cBool("enterprise_commit_present", "Built with Grafana Enterprise code.", "_enterprise"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var h rec
		if err := s.C.get(ctx, "/api/health", nil, &h); err != nil {
			return nil, err
		}
		h["_enterprise"] = strOrEmpty(h, "enterpriseCommit") != ""
		s.St.Done = true
		return s.Def.rows([]rec{h}, s), nil
	},
}
