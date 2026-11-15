// Grafana Alerting: rules (provisioning API), rule queries, rule groups and
// evaluation state (Prometheus-compatible rules API), contact points,
// the notification policy tree, mute timings, Alertmanager alerts and
// silences, and the alert state history.

package grafana

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
)

// durationSeconds parses a Prometheus/Go duration ("5m", "1h30m", "0s").
func durationSeconds(v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Seconds()
	}
	// Prometheus durations also allow d, w, y units.
	total := 0.0
	num := ""
	units := map[byte]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800, 'y': 31536000}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c == '.' {
			num += string(c)
			continue
		}
		mult := 0.0
		if c == 'm' && i+1 < len(s) && s[i+1] == 's' {
			mult = 0.001
			i++
		} else if u, ok := units[c]; ok {
			mult = u
		} else {
			return nil
		}
		n, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return nil
		}
		total += n * mult
		num = ""
	}
	if num != "" {
		return nil
	}
	return total
}

func cDuration(name, doc string, keys ...string) col {
	return cFn(name, tFloat64, doc, func(r rec, _ *scan) any { return durationSeconds(path(r, keys...)) })
}

// ---- alert_rules -------------------------------------------------------------

func provisionedRules(ctx context.Context, c *conn) ([]rec, error) {
	var rules []rec
	if err := c.get(ctx, "/api/v1/provisioning/alert-rules", nil, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

var alertRulesDef = &tableDef{
	Name:    "alert_rules",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:    "Alert Rules",
		Category: "alerting",
		Keywords: "alert rules, alerting, conditions, labels, annotations, for, no data, paused, provisioning",
		Description: "Grafana-managed alert rules with folder, rule group, condition, pending period, no-data and error handling, " +
			"labels, annotations and queries",
		DocLLM: "One row per Grafana-managed alert rule (GET /api/v1/provisioning/alert-rules; needs alert.rules:read). uid joins " +
			"alert_rule_queries.rule_uid, alert_rule_states.rule_uid and alerts.rule_uid. condition is the refId whose result fires the " +
			"rule; for_seconds the pending period; no_data_state / exec_err_state what happens on no data or errors; is_paused; labels and " +
			"annotations are MAPs (annotations['summary'], annotations['runbook_url']). queries is the full query + expression chain as JSON " +
			"(alert_rule_queries flattens it). Evaluation state lives in alert_rule_states.",
		DocMD: "## alert_rules\n\nAlert rule definitions. See `alert_rule_states` for whether they fire.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT folder_path, rule_group, title, labels['severity'] AS severity FROM grafana.main.alert_rules WHERE NOT is_paused ORDER BY folder_path, rule_group, title",
				Description: "Active rules with their severity label"},
			{SQL: "SELECT title FROM grafana.main.alert_rules WHERE annotations['runbook_url'] IS NULL ORDER BY title",
				Description: "Rules without a runbook link"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("uid", "Rule UID.", "uid"),
		cStr("title", "Rule title (the alertname label).", "title"),
		cStr("folder_uid", "UID of the rule's folder.", "folderUID"),
		cFolderPath("Folder chain of the rule, e.g. '/Alerts'.", "folderUID"),
		cStr("rule_group", "Evaluation group name.", "ruleGroup"),
		cStr("condition", "refId of the query or expression whose result fires the rule.", "condition"),
		cDuration("for_seconds", "Pending period in seconds (the 'for' duration).", "for"),
		cDuration("keep_firing_for_seconds", "Seconds the alert keeps firing after the condition clears.", "keep_firing_for"),
		cStr("no_data_state", "State on no data: NoData, Alerting, OK or KeepLast.", "noDataState"),
		cStr("exec_err_state", "State on evaluation error: Error, Alerting, OK or KeepLast.", "execErrState"),
		cBool("is_paused", "Evaluation is paused.", "isPaused"),
		cMap("labels", "Rule labels, e.g. labels['severity'].", "labels"),
		cMap("annotations", "Rule annotations, e.g. annotations['summary'], annotations['runbook_url'].", "annotations"),
		cFn("datasource_uids", tStrList, "Data sources the rule queries (server-side expressions excluded).", func(r rec, _ *scan) any {
			data, _ := r["data"].([]any)
			out := []any{}
			seen := map[string]bool{}
			for _, d := range data {
				if m, ok := d.(map[string]any); ok {
					u := strOrEmpty(m, "datasourceUid")
					if u != "" && u != "__expr__" && u != "-100" && !seen[u] {
						seen[u] = true
						out = append(out, u)
					}
				}
			}
			return out
		}),
		cJSON("queries", "The rule's queries and expressions (data) as JSON.", "data"),
		cJSON("notification_settings", "Simplified routing settings (receiver, group_by, ...) when set, as JSON.", "notification_settings"),
		cBool("is_recording", "A recording rule (writes a metric instead of alerting).", "_recording"),
		cStr("provenance", "Where the rule is managed: 'file', 'api' or NULL (UI).", "provenance"),
		cTS("updated_at", "Last update time.", "updated"),
		cInt("org_id", "Organization id.", "orgID"),
		cFn("url", tString, "Link to the rule in the Grafana UI.", func(r rec, s *scan) any {
			if uid := strOrEmpty(r, "uid"); uid != "" {
				return webURL(s.C, "/alerting/grafana/"+url.PathEscape(uid)+"/view")
			}
			return nil
		}),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		rules, err := provisionedRules(ctx, s.C)
		if err != nil {
			return nil, err
		}
		for _, r := range rules {
			r["_recording"] = r["record"] != nil
		}
		s.St.Done = true
		return s.Def.rows(rules, s), nil
	},
}

var alertRuleQueriesDef = &tableDef{
	Name:    "alert_rule_queries",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Alert Rule Queries",
		Category:    "alerting",
		Keywords:    "alert queries, expressions, reduce, threshold, math, datasource, promql, relative time range",
		Description: "The queries and server-side expressions of every alert rule, one row each",
		DocLLM: "One row per query or expression of each alert rule (from the provisioning API's data array). rule_uid joins " +
			"alert_rules.uid. is_expression marks server-side expressions (datasource '__expr__'); expression_type is reduce, math, " +
			"threshold, resample, classic_conditions or sql. expr / query_text hold the query string (PromQL expr, rawSql, ...; the " +
			"expression for an expression). relative_from_seconds / relative_to_seconds are the query window relative to evaluation time. " +
			"is_condition marks the refId that fires the rule.",
		DocMD: "## alert_rule_queries\n\nWhat each alert rule queries, flattened.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT datasource_uid, count(DISTINCT rule_uid) AS rules FROM grafana.main.alert_rule_queries WHERE NOT is_expression GROUP BY datasource_uid ORDER BY rules DESC, datasource_uid",
				Description: "Which data sources alerting depends on"},
			{SQL: "SELECT rule_title, ref_id, expression_type, query_text FROM grafana.main.alert_rule_queries ORDER BY rule_title, ref_id",
				Description: "Each rule's query and expression chain"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("rule_uid", "Rule UID (joins alert_rules.uid).", "_rule", "uid"),
		cStr("rule_title", "Rule title.", "_rule", "title"),
		cStr("ref_id", "Query reference id.", "q", "refId"),
		cStr("datasource_uid", "Data source UID ('__expr__' for an expression).", "q", "datasourceUid"),
		cFn("is_expression", tBool, "A server-side expression rather than a data source query.", func(r rec, _ *scan) any {
			u := strOrEmpty(r, "q", "datasourceUid")
			return u == "__expr__" || u == "-100"
		}),
		cFn("expression_type", tString, "Expression type: reduce, math, threshold, resample, classic_conditions, sql; NULL for queries.", func(r rec, _ *scan) any {
			u := strOrEmpty(r, "q", "datasourceUid")
			if u != "__expr__" && u != "-100" {
				return nil
			}
			return str(r, "q", "model", "type")
		}),
		cFn("is_condition", tBool, "This refId is the rule's condition.", func(r rec, _ *scan) any {
			return strOrEmpty(r, "q", "refId") == strOrEmpty(r, "_rule", "condition")
		}),
		cStr("query_type", "Query type, when the data source uses one.", "q", "queryType"),
		cStr("expr", "PromQL / LogQL expression, or an expression's expression field.", "q", "model", "expr"),
		cFn("query_text", tString, "The query string, whichever field holds it (expr, rawSql, query, expression, ...).", func(r rec, _ *scan) any {
			m, _ := path(r, "q", "model").(map[string]any)
			return queryText(m)
		}),
		cFloat("relative_from_seconds", "Query window start, in seconds before evaluation time.", "q", "relativeTimeRange", "from"),
		cFloat("relative_to_seconds", "Query window end, in seconds before evaluation time.", "q", "relativeTimeRange", "to"),
		cJSON("model", "The query or expression model as JSON.", "q", "model"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		rules, err := provisionedRules(ctx, s.C)
		if err != nil {
			return nil, err
		}
		var recs []rec
		for _, r := range rules {
			data, _ := r["data"].([]any)
			for _, d := range data {
				if q, ok := d.(map[string]any); ok {
					recs = append(recs, rec{"_rule": r, "q": q})
				}
			}
		}
		s.St.Done = true
		return s.Def.rows(recs, s), nil
	},
}

// ---- rule state (Prometheus-compatible API) ------------------------------------

func promRuleGroups(ctx context.Context, c *conn) ([]rec, error) {
	var resp struct {
		Data struct {
			Groups []rec `json:"groups"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/prometheus/grafana/api/v1/rules", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data.Groups, nil
}

var ruleGroupsDef = &tableDef{
	Name:    "rule_groups",
	Catalog: true,
	Folders: true,
	Docs: fnDocs{
		Title:       "Alert Rule Groups",
		Category:    "alerting",
		Keywords:    "rule groups, evaluation interval, alerting groups",
		Description: "Alert rule evaluation groups with their folder, interval, rule count and last evaluation",
		DocLLM: "One row per rule group (GET /api/prometheus/grafana/api/v1/rules). interval_seconds is the evaluation interval. " +
			"rule_count counts its rules; firing / pending count rules in those states. Join alert_rules on (folder_uid, rule_group = name).",
		DocMD: "## rule_groups\n\nRule groups and how often they evaluate.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT folder_path, name, interval_seconds, rule_count FROM grafana.main.rule_groups ORDER BY interval_seconds, name",
				Description: "Groups by evaluation frequency"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("name", "Group name.", "name"),
		cStr("folder_uid", "UID of the group's folder.", "folderUid"),
		cFolderPath("Folder chain of the group.", "folderUid"),
		cStr("folder_title", "Folder title (the 'file' field).", "file"),
		cFloat("interval_seconds", "Evaluation interval in seconds.", "interval"),
		cFn("rule_count", tInt64, "Number of rules in the group.", func(r rec, _ *scan) any {
			rules, _ := r["rules"].([]any)
			return int64(len(rules))
		}),
		cFn("firing", tInt64, "Rules currently firing.", func(r rec, _ *scan) any { return countState(r, "firing") }),
		cFn("pending", tInt64, "Rules currently pending.", func(r rec, _ *scan) any { return countState(r, "pending") }),
		cTS("last_evaluation", "Last evaluation time of the group.", "lastEvaluation"),
		cFloat("evaluation_seconds", "Duration of the last evaluation in seconds.", "evaluationTime"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		groups, err := promRuleGroups(ctx, s.C)
		if err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(groups, s), nil
	},
}

func countState(g rec, state string) any {
	rules, _ := g["rules"].([]any)
	n := int64(0)
	for _, r := range rules {
		if m, ok := r.(map[string]any); ok && strOrEmpty(m, "state") == state {
			n++
		}
	}
	return n
}

var alertRuleStatesDef = &tableDef{
	Name:    "alert_rule_states",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Alert Rule States",
		Category:    "alerting",
		Keywords:    "rule state, firing, pending, inactive, health, last evaluation, evaluation errors",
		Description: "Evaluation state of every alert rule: firing, pending, recovering or inactive, health, last error and timings",
		DocLLM: "One row per rule from Grafana's Prometheus-compatible rules API (GET /api/prometheus/grafana/api/v1/rules). state is " +
			"firing, pending, recovering or inactive; health is ok, nodata or error with last_error; active_at is when the current alert " +
			"started; instance_count is the number of alert instances. rule_uid joins alert_rules.uid.",
		DocMD: "## alert_rule_states\n\nWhich rules are firing right now, and which fail to evaluate.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT rule_name, state, active_at FROM grafana.main.alert_rule_states WHERE state IN ('firing', 'pending') ORDER BY active_at",
				Description: "Rules firing or pending now"},
			{SQL: "SELECT rule_name, last_error FROM grafana.main.alert_rule_states WHERE health = 'error' ORDER BY rule_name",
				Description: "Rules that fail to evaluate"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("rule_uid", "Rule UID (joins alert_rules.uid).", "r", "uid"),
		cStr("rule_name", "Rule title.", "r", "name"),
		cStr("rule_group", "Group name.", "g", "name"),
		cStr("folder_uid", "UID of the rule's folder.", "g", "folderUid"),
		cStr("state", "firing, pending, recovering or inactive.", "r", "state"),
		cStr("health", "ok, nodata or error.", "r", "health"),
		cStr("last_error", "Last evaluation error.", "r", "lastError"),
		cTS("active_at", "When the current alert became active.", "r", "activeAt"),
		cTS("last_evaluation", "Last evaluation time.", "r", "lastEvaluation"),
		cFloat("evaluation_seconds", "Duration of the last evaluation in seconds.", "r", "evaluationTime"),
		cBool("is_paused", "Evaluation is paused.", "r", "isPaused"),
		cFn("instance_count", tInt64, "Number of alert instances.", func(r rec, _ *scan) any {
			a, _ := path(r, "r", "alerts").([]any)
			return int64(len(a))
		}),
		cList("queried_datasource_uids", "Data sources the rule queried.", "r", "queriedDatasourceUIDs"),
		cMap("labels", "Rule labels.", "r", "labels"),
		cStr("type", "'alerting' or 'recording'.", "r", "type"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		groups, err := promRuleGroups(ctx, s.C)
		if err != nil {
			return nil, err
		}
		var recs []rec
		for _, g := range groups {
			rules, _ := g["rules"].([]any)
			for _, r := range rules {
				if m, ok := r.(map[string]any); ok {
					recs = append(recs, rec{"g": g, "r": m})
				}
			}
		}
		s.St.Done = true
		return s.Def.rows(recs, s), nil
	},
}

// ---- contact points ------------------------------------------------------------

// contactTargetKeys are settings that name where notifications go.
var contactTargetKeys = []string{"url", "apiUrl", "webhookUrl", "endpointUrl", "kafkaRestProxy", "gateway", "server", "host"}

var contactPointsDef = &tableDef{
	Name:    "contact_points",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Contact Points",
		Category:    "alerting",
		Keywords:    "contact points, receivers, integrations, slack, pagerduty, email, webhook, notifications",
		Description: "Notification contact points: name, integration type and target host only; settings, secrets and full URLs are never returned",
		DocLLM: "One row per integration of each contact point (GET /api/v1/provisioning/contact-points). name is the contact point " +
			"(receiver) name used by notification_policies.receiver; type the integration (email, slack, webhook, pagerduty, ...). " +
			"target_host is only the host of the integration's URL (webhook / Slack / API URL): paths and query strings, which often carry " +
			"tokens, are dropped. setting_keys lists which settings are configured, without values. address_count counts email addresses.",
		DocMD: "## contact_points\n\nWhere alerts go, with every credential and full URL withheld.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT type, count(*) AS integrations FROM grafana.main.contact_points GROUP BY type ORDER BY integrations DESC, type",
				Description: "Integrations per type"},
			{SQL: "SELECT c.name, c.type FROM grafana.main.contact_points c ANTI JOIN grafana.main.notification_policies p ON p.receiver = c.name ORDER BY c.name",
				Description: "Contact points no notification policy routes to"},
		},
	},
	Cols: []col{
		cTool(),
		cStr("uid", "Integration UID.", "uid"),
		cKey("name", "Contact point (receiver) name.", "name"),
		cStr("type", "Integration type: email, slack, webhook, pagerduty, teams, ...", "type"),
		cFn("target_host", tString, "Host of the integration's target URL; the path and query string are withheld.", func(r rec, _ *scan) any {
			for _, k := range contactTargetKeys {
				if h := urlHost(strOrEmpty(r, "settings", k)); h != "" {
					return h
				}
			}
			return nil
		}),
		cFn("address_count", tInt64, "Number of email addresses (email integrations).", func(r rec, _ *scan) any {
			a := strOrEmpty(r, "settings", "addresses")
			if a == "" {
				return nil
			}
			n := int64(0)
			for _, p := range strings.FieldsFunc(a, func(c rune) bool { return c == ';' || c == ',' || c == '\n' }) {
				if strings.TrimSpace(p) != "" {
					n++
				}
			}
			return n
		}),
		cFn("setting_keys", tStrList, "Names of the configured settings (values withheld).", func(r rec, _ *scan) any {
			m, _ := r["settings"].(map[string]any)
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := make([]any, len(keys))
			for i, k := range keys {
				out[i] = k
			}
			return out
		}),
		cBool("disable_resolve_message", "Resolved notifications are not sent.", "disableResolveMessage"),
		cStr("provenance", "Where it is managed: 'file', 'api' or NULL (UI).", "provenance"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var list []rec
		if err := s.C.get(ctx, "/api/v1/provisioning/contact-points", nil, &list); err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(list, s), nil
	},
}

// ---- notification policies -------------------------------------------------------

var matcherType = arrow.StructOf(
	arrow.Field{Name: "name", Type: tString, Nullable: true},
	arrow.Field{Name: "op", Type: tString, Nullable: true},
	arrow.Field{Name: "value", Type: tString, Nullable: true},
)

func matchers(v any) any {
	list, _ := v.([]any)
	out := []any{}
	for _, m := range list {
		parts, _ := m.([]any)
		if len(parts) == 3 {
			out = append(out, structVal{stringOrNil(parts[0]), stringOrNil(parts[1]), stringOrNil(parts[2])})
		}
	}
	return out
}

func stringOrNil(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	return nil
}

// flattenRoutes walks the policy tree depth first. path is "0" for the root,
// "0.1" for its second child, and so on. Inherited settings are resolved.
func flattenRoutes(node rec, pathStr string, depth int64, parent rec, out *[]rec) {
	eff := rec{}
	for _, k := range []string{"receiver", "group_by", "group_wait", "group_interval", "repeat_interval"} {
		if v, ok := node[k]; ok && v != nil && v != "" {
			eff[k] = v
		} else if parent != nil {
			eff[k] = parent[k]
		}
	}
	r := rec{"node": node, "eff": eff, "path": pathStr, "depth": depth}
	if parent != nil {
		r["parent"] = parent["_path"]
	}
	eff["_path"] = pathStr
	*out = append(*out, r)
	kids, _ := node["routes"].([]any)
	for i, k := range kids {
		if km, ok := k.(map[string]any); ok {
			flattenRoutes(km, pathStr+"."+strconv.Itoa(i), depth+1, eff, out)
		}
	}
}

var notificationPoliciesDef = &tableDef{
	Name:    "notification_policies",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Notification Policies",
		Category:    "alerting",
		Keywords:    "notification policies, routing tree, routes, matchers, receivers, group by, mute timings",
		Description: "The notification policy tree flattened: one row per route with its matchers, receiver and timings (inherited values resolved)",
		DocLLM: "One row per route of the notification policy tree (GET /api/v1/provisioning/policies), depth first. path is '0' for the " +
			"root (default policy), '0.1' for its second child, etc.; parent_path and depth place it in the tree. matchers is a list of " +
			"STRUCT(name, op, value) with op one of =, !=, =~, !~. receiver, group_by and the *_seconds timings are effective values " +
			"(inherited from the parent when the route does not set them). continue_matching is the route's 'continue' flag. " +
			"mute_timings / active_timings name mute_timings rows.",
		DocMD: "## notification_policies\n\nThe routing tree as rows, inherited settings resolved.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT path, depth, receiver, matchers FROM grafana.main.notification_policies ORDER BY path", Description: "The routing tree"},
			{SQL: "SELECT receiver, count(*) AS routes FROM grafana.main.notification_policies GROUP BY receiver ORDER BY routes DESC, receiver",
				Description: "How many routes end at each contact point"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("path", "Position in the tree: '0' = root, '0.1' = the root's second child.", "path"),
		cStr("parent_path", "Path of the parent route; NULL for the root.", "parent"),
		cInt("depth", "Depth: 0 for the root (default policy).", "depth"),
		cFn("is_default", tBool, "The root (default) policy.", func(r rec, _ *scan) any { return r["path"] == "0" }),
		cStr("receiver", "Effective contact point name (joins contact_points.name).", "eff", "receiver"),
		cFn("matchers", arrow.ListOf(matcherType), "Label matchers: list of STRUCT(name, op, value), op one of =, !=, =~, !~.", func(r rec, _ *scan) any {
			return matchers(path(r, "node", "object_matchers"))
		}),
		cList("group_by", "Effective group-by labels.", "eff", "group_by"),
		cDuration("group_wait_seconds", "Effective group wait in seconds.", "eff", "group_wait"),
		cDuration("group_interval_seconds", "Effective group interval in seconds.", "eff", "group_interval"),
		cDuration("repeat_interval_seconds", "Effective repeat interval in seconds.", "eff", "repeat_interval"),
		cFn("continue_matching", tBool, "Keep matching sibling routes after this one matches.", func(r rec, _ *scan) any {
			b, _ := path(r, "node", "continue").(bool)
			return b
		}),
		cList("mute_timings", "Mute timings applied (mute_timings.name).", "node", "mute_time_intervals"),
		cList("active_timings", "Active time intervals applied.", "node", "active_time_intervals"),
		cStr("provenance", "Where the tree is managed: 'file', 'api' or NULL (UI); set on the root only.", "node", "provenance"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var root rec
		if err := s.C.get(ctx, "/api/v1/provisioning/policies", nil, &root); err != nil {
			return nil, err
		}
		var recs []rec
		flattenRoutes(root, "0", 0, nil, &recs)
		s.St.Done = true
		return s.Def.rows(recs, s), nil
	},
}

var muteTimingsDef = &tableDef{
	Name:    "mute_timings",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Mute Timings",
		Category:    "alerting",
		Keywords:    "mute timings, time intervals, maintenance windows, schedules",
		Description: "Mute timings (named time intervals during which notifications are muted)",
		DocLLM: "One row per mute timing (GET /api/v1/provisioning/mute-timings). time_intervals is the list of intervals as JSON " +
			"(times start/end, weekdays, days_of_month, months, years, location); weekdays flattens every interval's weekdays. Referenced by " +
			"notification_policies.mute_timings.",
		DocMD: "## mute_timings\n\nNamed mute windows.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT name, weekdays, interval_count FROM grafana.main.mute_timings ORDER BY name", Description: "Mute timings and their weekdays"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("name", "Mute timing name.", "name"),
		cFn("interval_count", tInt64, "Number of time intervals.", func(r rec, _ *scan) any {
			ti, _ := r["time_intervals"].([]any)
			return int64(len(ti))
		}),
		cFn("weekdays", tStrList, "Weekdays of all intervals, e.g. ['saturday', 'sunday'].", func(r rec, _ *scan) any {
			out := []any{}
			ti, _ := r["time_intervals"].([]any)
			for _, t := range ti {
				if m, ok := t.(map[string]any); ok {
					if l, ok := strList(m, "weekdays").([]any); ok {
						out = append(out, l...)
					}
				}
			}
			return out
		}),
		cJSON("time_intervals", "The intervals as JSON (times, weekdays, days_of_month, months, years, location).", "time_intervals"),
		cStr("version", "Version (etag) of the mute timing.", "version"),
		cStr("provenance", "Where it is managed: 'file', 'api' or NULL (UI).", "provenance"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var list []rec
		if err := s.C.get(ctx, "/api/v1/provisioning/mute-timings", nil, &list); err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(list, s), nil
	},
}

// ---- Alertmanager: alerts and silences -----------------------------------------------

var alertsDef = &tableDef{
	Name:    "alerts",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Alert Instances",
		Category:    "alerting",
		Keywords:    "alerts, alert instances, firing, alertmanager, silenced, inhibited, receivers",
		Description: "Alert instances in Grafana's Alertmanager: labels, annotations, state, silences, receivers and timing",
		DocLLM: "One row per alert instance held by Grafana's built-in Alertmanager (GET /api/alertmanager/grafana/api/v2/alerts). state " +
			"is active, suppressed or unprocessed; silenced_by / inhibited_by list silence ids / fingerprints. rule_uid comes from the " +
			"__alert_rule_uid__ label (joins alert_rules.uid); alertname from the alertname label. labels / annotations are MAPs. receivers " +
			"are the contact points the alert routes to.",
		DocMD: "## alerts\n\nWhat is firing now, and where it is routed.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT alertname, labels['severity'] AS severity, starts_at FROM grafana.main.alerts WHERE state = 'active' ORDER BY starts_at",
				Description: "Active alerts, oldest first"},
			{SQL: "SELECT a.alertname, r.folder_path FROM grafana.main.alerts a JOIN grafana.main.alert_rules r ON r.uid = a.rule_uid ORDER BY a.alertname",
				Description: "Firing alerts with their rule's folder"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("fingerprint", "Alert fingerprint (unique per label set).", "fingerprint"),
		cStr("alertname", "The alertname label.", "labels", "alertname"),
		cStr("rule_uid", "Rule UID from the __alert_rule_uid__ label (joins alert_rules.uid).", "labels", "__alert_rule_uid__"),
		cStr("state", "active, suppressed or unprocessed.", "status", "state"),
		cList("silenced_by", "Ids of the silences muting it (joins silences.id).", "status", "silencedBy"),
		cList("inhibited_by", "Fingerprints of the alerts inhibiting it.", "status", "inhibitedBy"),
		cMap("labels", "Alert labels.", "labels"),
		cMap("annotations", "Alert annotations (summary, runbook_url, and Grafana's __values__ / __value_string__).", "annotations"),
		cFn("receivers", tStrList, "Contact points the alert is routed to.", func(r rec, _ *scan) any {
			list, _ := r["receivers"].([]any)
			out := []any{}
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					if n := str(m, "name"); n != nil {
						out = append(out, n)
					}
				}
			}
			return out
		}),
		cTS("starts_at", "When the alert started firing.", "startsAt"),
		cTS("ends_at", "When it resolves (or the expected end if it keeps firing).", "endsAt"),
		cTS("updated_at", "Last update.", "updatedAt"),
		cStr("generator_url", "Link to the rule that generated it.", "generatorURL"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var list []rec
		if err := s.C.get(ctx, "/api/alertmanager/grafana/api/v2/alerts", nil, &list); err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(list, s), nil
	},
}

var silenceMatcherType = arrow.StructOf(
	arrow.Field{Name: "name", Type: tString, Nullable: true},
	arrow.Field{Name: "value", Type: tString, Nullable: true},
	arrow.Field{Name: "is_regex", Type: tBool, Nullable: true},
	arrow.Field{Name: "is_equal", Type: tBool, Nullable: true},
)

var silencesDef = &tableDef{
	Name:    "silences",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Silences",
		Category:    "alerting",
		Keywords:    "silences, mute, maintenance, alertmanager, matchers",
		Description: "Alertmanager silences: matchers, author, comment, time window and state",
		DocLLM: "One row per silence in Grafana's Alertmanager (GET /api/alertmanager/grafana/api/v2/silences), expired ones included. " +
			"state is active, pending or expired. matchers is a list of STRUCT(name, value, is_regex, is_equal). id joins alerts.silenced_by.",
		DocMD: "## silences\n\nWho silenced what, until when.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT created_by, comment, ends_at FROM grafana.main.silences WHERE state = 'active' ORDER BY ends_at",
				Description: "Active silences by expiry"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("id", "Silence id.", "id"),
		cStr("state", "active, pending or expired.", "status", "state"),
		cFn("matchers", arrow.ListOf(silenceMatcherType), "Label matchers: STRUCT(name, value, is_regex, is_equal).", func(r rec, _ *scan) any {
			list, _ := r["matchers"].([]any)
			out := []any{}
			for _, m := range list {
				if mm, ok := m.(map[string]any); ok {
					eq := boolean(mm, "isEqual")
					if eq == nil {
						eq = true
					}
					out = append(out, structVal{str(mm, "name"), stringOrNil(mm["value"]), boolean(mm, "isRegex"), eq})
				}
			}
			return out
		}),
		cStr("created_by", "Author.", "createdBy"),
		cStr("comment", "Comment.", "comment"),
		cTS("starts_at", "Start of the silence.", "startsAt"),
		cTS("ends_at", "End of the silence.", "endsAt"),
		cTS("updated_at", "Last update.", "updatedAt"),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		var list []rec
		if err := s.C.get(ctx, "/api/alertmanager/grafana/api/v2/silences", nil, &list); err != nil {
			return nil, err
		}
		s.St.Done = true
		return s.Def.rows(list, s), nil
	},
}

// ---- alert state history ----------------------------------------------------------

var alertStateHistoryDef = &tableDef{
	Name:    "alert_state_history",
	Catalog: true,
	Docs: fnDocs{
		Title:       "Alert State History",
		Category:    "alerting",
		Keywords:    "state history, transitions, alert timeline, normal, alerting, pending, flapping",
		Description: "State transitions of alert rules (Normal -> Pending -> Alerting, ...) from Grafana's state history",
		DocLLM: "One row per alert state transition (GET /api/v1/rules/history?ruleUID=..., the annotation-backed state history; " +
			"Loki-backed history is not read). rule_uid = / IN visits only those rules, otherwise every rule from alert_rules is " +
			"read (one request each). time >= / > / < / <= / BETWEEN are pushed as the from/to window. previous_state / current_state are " +
			"Normal, Pending, Alerting, NoData, Error or Recovering (with a reason suffix such as 'Normal (MissingSeries)'). values is the " +
			"evaluation values JSON ({\"B\": 270, \"C\": 1}).",
		DocMD: "## alert_state_history\n\nAlert rule state transitions over time.",
		Examples: []vgi.CatalogExample{
			{SQL: "SELECT rule_uid, count(*) AS transitions FROM grafana.main.alert_state_history WHERE current_state LIKE 'Alerting%' GROUP BY rule_uid ORDER BY transitions DESC, rule_uid",
				Description: "Which rules fired most often"},
			{SQL: "SELECT time, previous_state, current_state FROM grafana.main.alert_state_history WHERE rule_uid = 'rule-high-latency' ORDER BY time",
				Description: "One rule's timeline (only that rule is read)"},
		},
	},
	Cols: []col{
		cTool(),
		cKey("rule_uid", "Rule UID (joins alert_rules.uid).", "rule_uid"),
		cFn("time", tTS, "Time of the transition.", func(r rec, _ *scan) any { return r["time"] }),
		cStr("previous_state", "State before the transition.", "prev"),
		cStr("current_state", "State after the transition.", "next"),
		cStr("text", "Transition summary with the alert's labels and values.", "text"),
		cFn("values", tJSON, "Evaluation values as JSON, e.g. {\"B\": 270, \"C\": 1}.", func(r rec, _ *scan) any {
			d := strOrEmpty(r, "data")
			if d == "" {
				return nil
			}
			var m map[string]any
			if json.Unmarshal([]byte(d), &m) == nil {
				if v, ok := m["values"]; ok {
					return jsonOf(rec{"v": v}, "v")
				}
			}
			return jsonText(d)
		}),
	},
	Next: func(ctx context.Context, s *scan) ([][]any, error) {
		if s.St.Cursor == "" {
			if v := eqValues(s.Preds, "rule_uid"); v != nil {
				s.St.Keys = v
			} else {
				rules, err := provisionedRules(ctx, s.C)
				if err != nil {
					return nil, err
				}
				for _, r := range rules {
					s.St.Keys = append(s.St.Keys, strOrEmpty(r, "uid"))
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
		p := url.Values{"ruleUID": {uid}}
		if lo, ok, hi, okHi := timeBounds(s.Preds, "time"); ok || okHi {
			if ok {
				p.Set("from", strconv.FormatInt(floorDiv(lo, 1_000_000), 10))
			}
			if okHi {
				p.Set("to", strconv.FormatInt(ceilDiv(hi, 1_000_000), 10))
			}
		}
		var fr dataFrame
		if err := s.C.get(ctx, "/api/v1/rules/history", p, &fr); err != nil {
			return nil, err
		}
		idx := map[string]int{}
		for i, f := range fr.Schema.Fields {
			idx[f.Name] = i
		}
		cell := func(name string, row int) json.RawMessage {
			i, ok := idx[name]
			if !ok || i >= len(fr.Data.Values) || row >= len(fr.Data.Values[i]) {
				return nil
			}
			return fr.Data.Values[i][row]
		}
		n := 0
		if i, ok := idx["time"]; ok && i < len(fr.Data.Values) {
			n = len(fr.Data.Values[i])
		}
		var recs []rec
		for row := 0; row < n; row++ {
			r := rec{"rule_uid": uid}
			if raw := cell("time", row); raw != nil {
				r["time"] = frameTime(raw, 0)
			}
			for _, k := range []string{"text", "prev", "next", "data"} {
				var sv string
				if raw := cell(k, row); raw != nil && json.Unmarshal(raw, &sv) == nil {
					r[k] = sv
				}
			}
			recs = append(recs, r)
		}
		return s.Def.rows(recs, s), nil
	},
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 { return -floorDiv(-a, b) }
