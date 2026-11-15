// Discovery metadata: the one file to edit when you rename or extend the worker.
//
// DuckDB, coding agents and the vgi-lint metadata checker all read these tags.
// Keep every object documented and `make lint-explain` stays at 100.
//
//   - Catalog-level tags (title, license, author, links, long description)
//     live in CatalogTags below.
//   - Every function carries objectTags(...): a title, a category from
//     SchemaCategories, search keywords, and LLM- and human-facing docs.

package grafana

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
)

// CatalogName is the name DuckDB attaches the worker under.
const CatalogName = "grafana"

// SourceURL is where this worker's code lives.
const SourceURL = "https://github.com/Query-farm/vgi-grafana"

// category is one entry of the schema's vgi.categories list.
type category struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// SchemaCategories group functions in docs and agent tools. Every function's
// vgi.category must name one of these.
var SchemaCategories = []category{
	{Name: "content", Title: "Dashboards And Content", Description: "Dashboards, folders, panels, panel queries, versions, library panels, data sources and search."},
	{Name: "access", Title: "Users And Access", Description: "Users, teams, team members, service accounts, organizations and server health."},
	{Name: "alerting", Title: "Alerting", Description: "Alert rules, rule queries, groups and states, contact points, notification policies, mute timings, alerts, silences and state history."},
	{Name: "annotations", Title: "Annotations", Description: "Events and time regions on dashboards and the organization, with time-window pushdown."},
	{Name: "query", Title: "Data Source Queries", Description: "Run read-only queries against any Grafana data source through /api/ds/query."},
	{Name: "tools", Title: "Offline Tools", Description: "Scalars that need no server: duration parsing and URL redaction."},
}

// mustJSON marshals v, panicking on failure (only static data is passed).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// keywordsArray turns "a, b, c" into the JSON array string vgi.keywords requires.
func keywordsArray(keywords string) string {
	out := []string{}
	for _, k := range strings.Split(keywords, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return mustJSON(out)
}

// docs is the set of standard per-object discovery fields.
type docs struct {
	Title    string
	Category string
	Keywords string
	DocLLM   string
	DocMD    string
}

// objectTags builds the standard per-object discovery tags.
func objectTags(d docs) map[string]string {
	known := false
	for _, c := range SchemaCategories {
		known = known || c.Name == d.Category
	}
	if !known {
		panic(fmt.Sprintf("unknown category %q; add it to SchemaCategories", d.Category))
	}
	return map[string]string{
		"vgi.title":    d.Title,
		"vgi.category": d.Category,
		"vgi.keywords": keywordsArray(d.Keywords),
		"vgi.doc_llm":  d.DocLLM,
		"vgi.doc_md":   d.DocMD,
	}
}

// resultColumn describes one output column for vgi.result_columns_schema.
type resultColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// column builds an Arrow field whose comment shows up in DuckDB's DESCRIBE and agent tools.
func column(name string, typ arrow.DataType, comment string, nullable bool) arrow.Field {
	return arrow.Field{Name: name, Type: typ, Nullable: nullable,
		Metadata: arrow.NewMetadata([]string{"comment"}, []string{comment})}
}

// exampleQuery is one entry of vgi.example_queries.
type exampleQuery struct {
	Description string `json:"description"`
	SQL         string `json:"sql"`
}

// exampleQueries converts catalog examples to vgi.example_queries entries.
func exampleQueries(lists ...[]vgi.CatalogExample) []exampleQuery {
	out := []exampleQuery{}
	for _, examples := range lists {
		for _, ex := range examples {
			out = append(out, exampleQuery{Description: ex.Description, SQL: ex.SQL})
		}
	}
	return out
}

// withExamples publishes examples (with descriptions) as vgi.example_queries.
// Overloads share one SQL name, so pass every overload's examples.
func withExamples(tags map[string]string, lists ...[]vgi.CatalogExample) map[string]string {
	tags["vgi.example_queries"] = mustJSON(exampleQueries(lists...))
	return tags
}

const catalogDocLLM = "Read Grafana (OSS, Enterprise or Grafana Cloud) from SQL through its HTTP API, read-only. Needs a DuckDB " +
	"secret: CREATE SECRET (TYPE grafana, BASE_URL 'https://grafana.example.com', TOKEN '<service account token>') or USERNAME/PASSWORD " +
	"for basic auth; optional ORG_ID (sent as X-Grafana-Org-Id) and PAGE_SIZE. ATTACH option secret_name picks a named secret for every " +
	"table; table functions also take secret_name :=. Reach for it to inventory and audit Grafana content (dashboards, panels and " +
	"their queries flattened, folders with paths, data sources, version history), review access (users, teams, service accounts), " +
	"inspect alerting configuration and live state, read annotations over a time window, or run any data source query through " +
	"Grafana and get long rows (labels `MAP`, ts, value). Most objects are catalog tables read without parentheses. The content " +
	"inventory uses the shared BI shape (tool, id, title, folder_path, owner, created_at, updated_at, last_viewed_at, view_count, url) " +
	"that `UNION ALL BY NAME` with vgi-looker / vgi-metabase / vgi-tableau. WHERE filters on uids, folders, tags, time and type are " +
	"pushed into API parameters and re-checked exactly. Results reflect the token's permissions. Secrets never leave Grafana: data " +
	"source passwords, secure fields, URL credentials, contact point settings and service account tokens are not returned."

// CatalogDocMD is the catalog's comment: one short paragraph for directories.
const CatalogDocMD = "Grafana dashboards, panels and their queries, folders, data sources, users, teams, alerting and annotations " +
	"as read-only SQL tables, plus ds_query to run any Grafana data source query from DuckDB."

const catalogDocMDLong = "# Grafana in DuckDB\n\n" +
	"Inventory, audit and query Grafana from SQL. Every object is read-only and comes from Grafana's HTTP API with the " +
	"permissions of a service account token or login.\n\n" +
	"- **Content**: dashboards, folders (nested, with paths), panels and their queries flattened, version history, library panels, " +
	"data sources and search. The BI tables share column names with the Looker, Tableau and Metabase workers.\n" +
	"- **Access**: users, teams and members, service accounts, organizations.\n" +
	"- **Alerting**: rules and their queries, groups, live states, contact points, the notification policy tree, mute timings, " +
	"Alertmanager alerts and silences, state history.\n" +
	"- **Annotations** with the time window pushed to Grafana.\n" +
	"- **ds_query**: any data source query through Grafana's `/api/ds/query`, as long rows.\n\n" +
	"Credentials come from a `grafana` DuckDB secret; secrets stored in Grafana are never returned."

const schemaDocLLM = "The main schema mirrors one Grafana organization through its HTTP API. Most objects are catalog tables, " +
	"read without parentheses and re-fetched on every query; two are table functions (Grafana search and data source queries) " +
	"and two are offline scalars. Inventory tables share the BI vocabulary of the Looker, Tableau and Metabase workers: tool " +
	"('grafana'), id, title, folder_path ('/Parent/Child', '/' = General), owner, created_at, updated_at, last_viewed_at and " +
	"view_count (NULL on OSS), url. Grafana objects are keyed by UID: dashboard_uid, rule_uid, datasource_uid and folder_uid columns " +
	"join to the id / uid of the owning table. Fan-out tables (one request per dashboard, rule or team) read only the keys pinned " +
	"by an equality or IN filter on that key. Equality, IN, list_contains on tags and time ranges become API parameters; every " +
	"filter is re-checked exactly in the worker. Every object is read-only and nothing stored as a secret in Grafana is returned."

// SchemaComment is the main schema's one-line comment.
const SchemaComment = "One Grafana organization as read-only SQL: content inventory, access, alerting, annotations and data source queries."

// SchemaDocMD is the main schema's human-facing description.
const SchemaDocMD = "## main\n\n" +
	"Everything is read from Grafana's HTTP API on each query, with the permissions of the `grafana` secret's service account " +
	"token or login, in the organization the optional `ORG_ID` selects (sent as `X-Grafana-Org-Id`). Nothing is cached and nothing is " +
	"written.\n\n" +
	"Objects are keyed by UID: `dashboard_uid`, `rule_uid`, `datasource_uid` and `folder_uid` columns join to the owning " +
	"table. Filters on keys, folders, tags and time are sent to Grafana so only what you ask for is fetched, and every filter " +
	"is re-checked exactly. Tables that need one request per dashboard, rule or team fetch only the keys pinned by an equality " +
	"or `IN` filter, so pin them when you can.\n\n" +
	"`view_count` and `last_viewed_at` are NULL on Grafana OSS, and `folder_path` is `/` for the General folder. " +
	"The BI inventory columns line up with the Looker, Tableau and Metabase workers for cross-tool reports."

// agentTask is one public analyst task for `vgi-lint simulate`.
type agentTask struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// agentTestTasks are graded privately by vgi-agent-tests.yaml. They assume
// a grafana secret pointing at the recorded fixture (test/fixture_server).
var agentTestTasks = []agentTask{
	{Name: "dashboards_per_folder", Prompt: "Using the grafana worker, how many dashboards are in each folder path?"},
	{Name: "starred_dashboards", Prompt: "Which Grafana dashboards are starred?"},
	{Name: "nested_folders", Prompt: "List every Grafana folder with its full folder path, deepest first."},
	{Name: "panel_types", Prompt: "How many panels of each visualization type are on the Grafana dashboards, rows excluded?"},
	{Name: "promql_queries", Prompt: "Which PromQL expressions are used on Grafana dashboards, and on which dashboard?"},
	{Name: "unused_datasources", Prompt: "Which Grafana data sources are not used by any dashboard panel?"},
	{Name: "dashboard_history", Prompt: "Who saved the 'Web Overview' dashboard and with which messages, in version order?"},
	{Name: "library_usage", Prompt: "Which library panels exist and how many dashboards use each?"},
	{Name: "org_roles", Prompt: "How many Grafana users are there per org role?"},
	{Name: "team_membership", Prompt: "Who are the members of the SRE team?"},
	{Name: "firing_rules", Prompt: "Which Grafana alert rules are firing right now?"},
	{Name: "rule_datasources", Prompt: "Which data sources do the Grafana alert rules query?"},
	{Name: "routing_tree", Prompt: "Show the Grafana notification policy tree: each route's path, receiver and matchers."},
	{Name: "contact_point_types", Prompt: "Which contact points exist and what integration type is each?"},
	{Name: "active_silences", Prompt: "Which silences are active, who created them and why?"},
	{Name: "deploy_annotations", Prompt: "List the deploy annotations on 2026-01-01 in time order."},
	{Name: "testdata_query", Prompt: "Using Grafana's TestData data source, run the csv_metric_values scenario with values 1,20,90 between 2026-01-01 00:00 and 01:00 UTC and return the time and value of each point."},
	{Name: "server_version", Prompt: "Which Grafana version is the server running?"},
	{Name: "rules_per_folder", Prompt: "How many Grafana alert rules are in each rule group, and which are paused?"},
	{Name: "rule_transitions", Prompt: "List the state transitions of the Grafana alert rule 'rule-high-latency' with previous and new state."},
	{Name: "alert_instances", Prompt: "Which Grafana alert instances are active, and which contact points receive them?"},
	{Name: "weekend_mute", Prompt: "Which Grafana mute timings exist and on which weekdays do they apply?"},
	{Name: "organizations", Prompt: "Which Grafana organizations exist?"},
	{Name: "group_intervals", Prompt: "How often is each Grafana alert rule group evaluated, in seconds?"},
	{Name: "search_web", Prompt: "Using Grafana's search, find every dashboard and folder whose title contains 'web'."},
	{Name: "service_account_tokens", Prompt: "Which Grafana service accounts exist, with their role and number of tokens?"},
	{Name: "team_sizes", Prompt: "How many members does each Grafana team have?"},
	{Name: "redacted_endpoints", Prompt: "Using the grafana worker, strip credentials and query strings from the URL 'https://bot:s3cret@hooks.example.com/services/T0?token=abc'."},
	{Name: "refresh_seconds", Prompt: "Which Grafana dashboards auto-refresh, and how often in seconds?"},
}

// CatalogTags are the catalog-level discovery tags.
var CatalogTags = map[string]string{
	"vgi.title":              "VGI Grafana Worker",
	"vgi.subtitle":           "Grafana dashboards, panels, data sources, users, alerting and annotations as SQL, plus any data source query.",
	"vgi.doc_llm":            catalogDocLLM,
	"vgi.doc_md":             catalogDocMDLong,
	"vgi.source_url":         SourceURL,
	"vgi.icon_url":           "https://query.farm/media-kit/products/vgi/vgi-logo.png",
	"vgi.license":            "0BSD",
	"vgi.author":             "Query.Farm",
	"vgi.copyright":          "Copyright 2026 Query Farm LLC - https://query.farm",
	"vgi.keywords":           keywordsArray("grafana, dashboards, panels, alerting, annotations, data sources, observability, bi, grafana cloud, inventory"),
	"vgi.support_contact":    SourceURL + "/issues",
	"vgi.support_policy_url": SourceURL + "#support",
	// Public analyst tasks for `vgi-lint simulate`; private graders live in vgi-agent-tests.yaml.
	"vgi.agent_test_tasks": mustJSON(agentTestTasks),
}

// schemaTags builds the main schema's tags; allExamples feeds vgi.example_queries.
func schemaTags(allExamples []exampleQuery) map[string]string {
	return map[string]string{
		"vgi.title":           "Grafana Tables",
		"domain":              "observability",
		"vgi.keywords":        keywordsArray("dashboards, folders, panels, data sources, users, teams, alert rules, silences, annotations, ds_query"),
		"vgi.doc_llm":         schemaDocLLM,
		"vgi.doc_md":          SchemaDocMD,
		"vgi.categories":      mustJSON(SchemaCategories),
		"vgi.example_queries": mustJSON(allExamples),
	}
}
