<p align="center">
  <img src="https://query.farm/media-kit/products/vgi/vgi-logo.png" alt="Vector Gateway Interface (VGI)" width="320">
</p>

# Query Grafana Dashboards, Alerting and Data Sources With SQL in DuckDB

> **vgi-grafana** · a [Query.Farm](https://query.farm) VGI worker

[![CI](https://github.com/Query-farm/vgi-grafana/actions/workflows/ci.yml/badge.svg)](https://github.com/Query-farm/vgi-grafana/actions/workflows/ci.yml)
[![vgi worker: query.farm](https://query.farm/media-kit/shields/vgi-worker.svg)](https://query.farm)

Grafana knows which dashboards exist, what every panel queries, who changed what, which
alerts fire and where they are routed, but it only shows you one screen at a time. This
worker turns the **Grafana HTTP API** (Grafana OSS, Enterprise and Grafana Cloud) into SQL
tables in DuckDB, so you can audit, inventory and join all of it, and run **any Grafana
data source query** from SQL through Grafana itself.

- **Content inventory**: dashboards, nested folders with full paths, every panel and every
  panel query flattened (PromQL `expr`, SQL `rawSql`, ...), version history, library panels,
  data sources. The BI tables use the same column names as
  [vgi-looker](https://github.com/Query-farm/vgi-looker), vgi-tableau and vgi-metabase, so a
  cross-tool inventory is one `UNION ALL BY NAME`.
- **Access**: users and org roles, teams and members, service accounts, organizations.
- **Alerting**: rule definitions and their query chains, groups, live states, contact points,
  the notification policy tree flattened, mute timings, Alertmanager alerts and silences,
  state transitions.
- **Annotations** with the time window, dashboard, panel, tag and type pushed to Grafana.
- **`ds_query`**: run a query against any data source (Prometheus, Loki, PostgreSQL,
  CloudWatch, TestData, ...) via `POST /api/ds/query` and get long rows
  (`labels` MAP, `ts`, `value`), the same shape as
  [vgi-prometheus](https://github.com/Query-farm/vgi-prometheus).

It is **read-only**: every request is a `GET`, plus `POST /api/ds/query`, which runs a query
and saves nothing. **No secret leaves Grafana**: data source passwords and secure fields,
URL user info, contact point settings and service account tokens are never returned.

## Quick start

```sql
INSTALL vgi FROM community;     -- Haybarn 2.0; on DuckDB 2.0 use the query_farm repository
LOAD vgi;
ATTACH 'grafana' (TYPE vgi, LOCATION 'github-auto://Query-farm/vgi-grafana@v0.1.0');

CREATE SECRET (TYPE grafana, BASE_URL 'https://grafana.example.com', TOKEN 'glsa_...');

-- Which dashboards query Prometheus, and with what?
SELECT dashboard_title, panel_title, expr
FROM grafana.main.panel_queries
WHERE datasource_type = 'prometheus'
ORDER BY dashboard_title, panel_title;
```

The secret type is registered by the worker, so run `CREATE SECRET` after `ATTACH`.

<details><summary>Other ways to run it</summary>

- **DuckDB 2.0**:
  `SET allow_extension_repositories = 'allowed'; CREATE EXTENSION REPOSITORY IF NOT EXISTS query_farm WITH PREFIX 'https://duckdb-extensions.query.farm'; INSTALL vgi FROM query_farm; LOAD vgi FROM query_farm;`
  then attach as above.
- **Docker** (HTTP): `docker run -p 8000:8000 ghcr.io/query-farm/vgi-grafana:0.1.0`, then
  `ATTACH 'grafana' (TYPE vgi, LOCATION 'http://localhost:8000')`.
- **Go**: `go install github.com/Query-farm/vgi-grafana/cmd/vgi-grafana-worker@v0.1.0`,
  then attach the binary by absolute path.
- **From source**: `make build` (Go 1.26+, pure Go, no cgo), then
  `ATTACH 'grafana' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-grafana-worker')`.

v0.1.0 is not published yet. Until it is, build from source.
</details>

## Secrets and configuration

Every table reads a DuckDB secret of type `grafana`:

| Field | Meaning |
|---|---|
| `BASE_URL` | Grafana root, e.g. `https://grafana.example.com`, `https://acme.grafana.net`, or `https://example.com/grafana` when served from a sub-path. Must not contain `user:password@`. |
| `TOKEN` | Service account token or API key, sent as `Authorization: Bearer …`. Redacted. |
| `USERNAME` / `PASSWORD` | Basic auth (used when no `TOKEN` is set). Password redacted. |
| `ORG_ID` | Sent as `X-Grafana-Org-Id`: read another organization than the credential's current one. |
| `PAGE_SIZE` | Page size for paged endpoints (search, users, teams, service accounts, library panels, annotations); default 1000. |

```sql
-- Grafana Cloud with a service account token
CREATE SECRET cloud (TYPE grafana, BASE_URL 'https://acme.grafana.net', TOKEN 'glsa_...');
-- Self-hosted, second organization, basic auth
CREATE SECRET partner (TYPE grafana, BASE_URL 'https://grafana.internal', USERNAME 'auditor', PASSWORD '...', ORG_ID '2');

-- Several Grafanas at once: one ATTACH per instance, each naming its secret.
ATTACH 'grafana' AS cloud (TYPE vgi, LOCATION '...', secret_name 'cloud');
ATTACH 'grafana' AS partner (TYPE vgi, LOCATION '...', secret_name 'partner');
SELECT * FROM cloud.main.dashboards UNION ALL BY NAME SELECT * FROM partner.main.dashboards;

-- The table functions also take secret_name := directly.
SELECT * FROM grafana.main.search(query := 'latency', secret_name := 'partner');
```

With more than one `grafana` secret and no `secret_name`, DuckDB picks one of them, so name
the secret explicitly. Credentials are read from the secret on every request and never enter
table-function state, logs or error messages. With Grafana's default roles a **Viewer** service
account can read dashboards, panels, versions, folders, search, annotations, rule groups and
states, alerts and silences, and run `ds_query` on data sources it may query; data sources,
users, teams, service accounts and the provisioning-API alerting tables (`alert_rules`,
`alert_rule_queries`, `contact_points`, `notification_policies`, `mute_timings`) need
**Admin** (or the matching RBAC read permissions), and listing every org needs a **server
admin** (otherwise `orgs` returns the current org). These are Grafana's defaults, not
verified against every RBAC setup.
Requests that get 429, 502, 503 or 504 are retried with exponential backoff, honouring
`Retry-After`; other errors carry the HTTP status and Grafana's message.

## What's inside

Everything is in `grafana.main`. Catalog tables are read without parentheses.

| Object | Rows | Grafana API |
|---|---|---|
| `dashboards` | one per dashboard (BI shape + tags, version, panel count, JSON) | `/api/search`, `/api/dashboards/uid/<uid>` (only when a detail column is selected) |
| `folders` | one per folder, nested ones included, with `parent_uid`, `depth`, `folder_path` | `/api/search?type=dash-folder`, `/api/folders/<uid>` |
| `panels` | every panel, panels inside collapsed rows included: type, data source, grid, targets JSON | `/api/dashboards/uid/<uid>` per dashboard |
| `panel_queries` | every panel query: effective data source, `expr`, `raw_sql`, `query_text`, target JSON | same |
| `dashboard_versions` | saved versions: author, time, message | `/api/dashboards/uid/<uid>/versions` |
| `library_panels` | library panels with usage counts and model | `/api/library-elements` |
| `connections` | data sources: type, endpoint (credentials stripped), database, access, masked `json_data` | `/api/datasources` |
| `search(query :=, tags :=, type :=, folder_uids :=, starred :=)` | Grafana search | `/api/search` |
| `users` | org users with role, last seen, disabled | `/api/org/users/search` |
| `teams`, `team_members` | teams and membership | `/api/teams/search`, `/api/teams/<id>/members` |
| `service_accounts` | service accounts with token counts (never tokens) | `/api/serviceaccounts/search` |
| `orgs` | organizations | `/api/orgs` (`/api/org` fallback) |
| `health` | version, commit, database status | `/api/health` |
| `alert_rules` | rule definitions: folder, group, condition, for, no-data / error state, labels, annotations, queries | `/api/v1/provisioning/alert-rules` |
| `alert_rule_queries` | each rule's queries and expressions | same |
| `rule_groups` | groups with interval and counts | `/api/prometheus/grafana/api/v1/rules` |
| `alert_rule_states` | live state, health, last error, instance count | same |
| `contact_points` | name, integration type, target **host only**, setting names (no values) | `/api/v1/provisioning/contact-points` |
| `notification_policies` | the routing tree, one row per route, inherited values resolved | `/api/v1/provisioning/policies` |
| `mute_timings` | named mute windows | `/api/v1/provisioning/mute-timings` |
| `alerts` | Alertmanager alert instances | `/api/alertmanager/grafana/api/v2/alerts` |
| `silences` | Alertmanager silences | `/api/alertmanager/grafana/api/v2/silences` |
| `alert_state_transitions` | rule state changes over time | `/api/v1/rules/history` (annotation-backed history) |
| `annotations` | events and regions, alert annotations included | `/api/annotations` |
| `ds_query(datasource_uid, query_json, start_time, end_time)` | data frames as long rows | `POST /api/ds/query` |
| `duration_seconds(d)`, `redact_url(url)` | offline scalars | none |

### Shared BI columns (UNION across Grafana, Looker, Tableau and Metabase)

| Column | Type | Meaning in Grafana |
|---|---|---|
| `tool` | `VARCHAR` | Always `'grafana'` |
| `id` | `VARCHAR` | UID (dashboards, folders, data sources), numeric user id as text (users) |
| `title` | `VARCHAR` | Dashboard, folder or data source name |
| `folder_path` | `VARCHAR` | `/Operations/Web`: the folder chain from the root; `/` is the General folder |
| `owner` | `VARCHAR` | Login of the creating user |
| `created_at` / `updated_at` | `TIMESTAMPTZ` | Creation / last save |
| `last_viewed_at` / `view_count` | `TIMESTAMPTZ` / `BIGINT` | `NULL`: Grafana OSS has no view statistics API |
| `url` | `VARCHAR` | Link into the Grafana UI |

`dashboards` and `folders` carry all of them; `connections` has `tool, id, title, created_at
(NULL), url`; `users` has `tool, id, url` plus `login`, `display_name`, `email`.

```sql
SELECT tool, id, title, folder_path, owner, updated_at, url FROM grafana.main.dashboards
UNION ALL BY NAME
SELECT tool, id, title, folder_path, owner, updated_at, url FROM looker.main.dashboards;
```

### `ds_query` output

One row per (frame row, non-time field): `ref_id`, `frame_index`, `frame_name`,
`frame_type` (dataplane type such as `timeseries-multi`), `field`, `labels`
`MAP(VARCHAR, VARCHAR)` (the field's labels, e.g. Prometheus series labels), `row_index`,
`ts` (the frame's first time field; `NULL` for table frames), `value DOUBLE` (numbers, with
NaN and ±Inf decoded from the frame's entities; booleans as 1/0), `string_value` (strings,
booleans, enums, extra time fields), `unit`. `query_json` is the data source's own query
model, exactly as a panel target stores it, so you can copy one from
`panel_queries.target_json`; a JSON array runs several queries in one request (`refId`
defaults to A, B, ...). `max_data_points :=` and `interval_ms :=` are passed through.

Limits: the whole response is decoded in one batch (bounded at 512 MiB per response); the
data source's own limits apply (Grafana's `[dataproxy] row_limit`, default 1,000,000 rows
for SQL data sources; Prometheus' 11,000 points per series, which Grafana enforces through
`maxDataPoints` / `intervalMs`). Only Grafana's query API is used, so data sources that
only work from the browser (`access: direct`) cannot be queried. A failed query fails the
SQL statement with its `refId` and the data source's error.

## What pushes down

Every table declares filter and projection pushdown. DuckDB removes a pushed predicate
from its own plan, so the worker **re-applies every pushed filter exactly**; translating it
to API parameters only narrows what is fetched.

| SQL | Becomes |
|---|---|
| `dashboards`: `id = / IN`, `folder_uid = / IN`, `list_contains(tags, 'x')`, `is_starred` | `dashboardUIDs`, `folderUIDs`, `tag` (all must match), `starred=true` |
| `search(...)`: the arguments, and `WHERE kind / uid / folder_uid / is_starred / list_contains(tags, …)` | `query`, `tag`, `type`, `folderUIDs`, `dashboardUIDs`, `starred` |
| `panels`, `panel_queries`: `dashboard_uid = / IN`, `folder_uid = / IN` | only those dashboards are read (one request each) |
| `dashboard_versions`: `dashboard_uid = / IN` | only those dashboards' histories |
| `library_panels`: `folder_uid = / IN`, `name = 'x'` | `folderFilterUIDs`, `searchString` |
| `team_members`: `team_id = / IN`; `teams`: `name = 'x'` | only those teams; `name` |
| `users`: `login = 'x'`; `service_accounts`: `name = 'x'` | `query` |
| `alert_state_transitions`: `rule_uid = / IN`, `time` ranges | `ruleUID` per rule, `from` / `to` |
| `annotations`: `time >= / > / < / <= / = / BETWEEN`, `dashboard_uid = 'x'`, `panel_id = n`, `type = 'alert' / 'annotation'`, `list_contains(tags, 'x')` | `from` / `to`, `dashboardUID`, `panelId`, `type`, `tags` |

Detail columns are fetched only when selected: `dashboards.owner`, `created_at`,
`updated_at`, `version`, `panel_count`, `dashboard_json`, ... cost one request per dashboard,
and `folders.owner` / dates one per folder.

**Annotations paging.** `/api/annotations` has a limit but no offset, and orders rows by a
mix of start and end times, so the scan partitions the start-time range into windows: a
window whose page comes back full is split at the oldest start on the page (or, when
regions overlapping the window crowd the page, re-read with a larger limit), until no page
is truncated. Every annotation is read from exactly one window. The window stack, like
every cursor (page numbers, pending dashboard UIDs, version `continueToken`s), is part of
the scan state, so over the HTTP transport each page can be served by a different request.

## Examples

```sql
-- Dashboards per folder, and who owns the stale ones.
SELECT folder_path, count(*) AS dashboards FROM grafana.main.dashboards GROUP BY folder_path ORDER BY dashboards DESC;
SELECT title, owner, updated_at FROM grafana.main.dashboards WHERE updated_at < now() - INTERVAL 1 YEAR ORDER BY updated_at;

-- Data sources nobody uses.
SELECT c.title, c.type FROM grafana.main.connections c
ANTI JOIN grafana.main.panel_queries q ON q.datasource_uid = c.id;

-- Which dashboards would break if http_requests_total were renamed?
SELECT DISTINCT dashboard_title, panel_title FROM grafana.main.panel_queries WHERE expr LIKE '%http_requests_total%';

-- Firing alerts, their rule's folder and where they are routed.
SELECT a.alertname, r.folder_path, a.receivers, a.starts_at
FROM grafana.main.alerts a JOIN grafana.main.alert_rules r ON r.uid = a.rule_uid
WHERE a.state = 'active' ORDER BY a.starts_at;

-- Deploy annotations next to a metric, both from Grafana.
WITH deploys AS (SELECT time FROM grafana.main.annotations
                 WHERE list_contains(tags, 'deploy') AND time >= now() - INTERVAL 1 DAY)
SELECT q.ts, q.value, d.time IS NOT NULL AS deploy
FROM grafana.main.ds_query('prom-prod', '{"expr": "sum(rate(http_requests_total{code=~\"5..\"}[5m]))"}',
                           now() - INTERVAL 1 DAY, now(), interval_ms := 60000) q
LEFT JOIN deploys d ON date_trunc('minute', d.time) = q.ts ORDER BY q.ts;
```

## How it is tested

`make check` runs gofmt, go vet, golangci-lint, `go test`, the SQL suite
(`test/sql/grafana.test`, 100+ statements) over the stdio, launch and HTTP transports, and
vgi-lint at `fail_on = "info"`.

**CI needs no Docker and no credentials.** The SQL suite talks to
`test/fixture_server/server.py`, a **strict replay server**: it answers from
`test/fixture_server/recorded.json`, keyed by method, path, sorted query and canonical JSON
body, and returns 404 for anything unrecorded, so a changed request fails the tests. A few
hand-written responses in `handwritten.json` (made by `make_handwritten.py`) cover shapes a
stock server will not produce on demand: a failing data source, NaN/±Inf entities with
nanosecond time offsets, and a credential that may not list orgs. The runner starts two
copies: one answers every request with `429 + Retry-After: 0` once (so retries are always
exercised), the other demands `Bearer test-token` or basic auth plus `X-Grafana-Org-Id: 1`.
The suite uses `PAGE_SIZE '2'` so every paged endpoint, the annotation window splitting and
the HTTP continuation of multi-page scans are exercised.

**The recordings come from a real Grafana.** `test/fixture/run-grafana.sh start` runs
`grafana/grafana-oss:12.1.1` (about 110 MB RAM, capped at 512 MB), provisioned from files
(TestData, Prometheus and PostgreSQL data sources with fake secrets, a file dashboard, alert
rules, contact points, a nested notification policy tree, mute timings), then
`test/fixture/seed.py` adds nested folders, dashboards with two versions and a collapsed row,
a library panel, users, teams, a service account, a second org, annotations at fixed 2026
times and a silence, and waits until an alert fires. Grafana's built-in TestData data source
makes `ds_query` deterministic offline (`csv_metric_values`, `csv_content`,
`predictable_pulse`). The SQL suite also checks that none of the fake secrets
(`hunter2`, `do-not-leak-*`) appears in any output. To re-record after changing the tests:

```bash
test/fixture/run-grafana.sh start            # prints http://127.0.0.1:13000
rm test/fixture_server/recorded.json
GRAFANA_RECORD_UPSTREAM=http://127.0.0.1:13000 make test-sql
test/fixture/run-grafana.sh stop
```

Recording forwards to the fixture with its throwaway `admin:admin` login. Timestamps that
Grafana sets itself (creation times, alert start times) change with every recording.
Point the worker at either fixture through the secret's `BASE_URL`; there are no test-only
code paths.

## Known limitations

- **Time zone.** vgi-go 0.42.0 cannot decode a pushed `TIMESTAMPTZ` filter (e.g.
  `WHERE time >= TIMESTAMPTZ '…'` on `annotations`, `updated_at > …` on `dashboards`) in a
  session whose `TimeZone` is not UTC: the query fails with "comparison operands have
  incompatible types". Run `SET TimeZone = 'UTC'` first, or filter in a subquery DuckDB does
  not push down.
- A pushed filter the worker cannot evaluate exactly (arithmetic such as `col + 1 > 3` on
  some types also hits an SDK decoding error) fails the query rather than returning wrong
  rows; filter in a subquery or CTE instead.
- `last_viewed_at` / `view_count` are `NULL`: Grafana OSS has no view statistics API
  (Enterprise usage insights are not read).
- Library panel instances in `panels` carry `library_panel_uid` but `NULL` title / type /
  targets; join `library_panels` for those.
- Alert state history is read from the annotation-backed history API; a Loki-backed history
  is not read. Data source-managed (Prometheus / Mimir ruler) rules are not listed.
- Panel ids are not guaranteed unique by Grafana, so `panels` and `panel_queries` declare no
  primary key; `contact_points` and `alert_state_transitions` have no natural key either.
- No TLS client certificates (put a proxy in front, or use a token).

## Development

See [AGENTS.md](AGENTS.md) for the layout, the recipe for adding a table and the sharp
edges. `make help` lists the targets; `make template-diff` shows upstream starter changes
(this worker started from vgi-starter-go v1.3.0).

## Support

Open an issue on this repository. For VGI itself, see [query.farm](https://query.farm).

## Authorship & License

Copyright 2026 Query Farm LLC - https://query.farm

Released under the [BSD Zero Clause License](LICENSE) (0BSD). Grafana is a trademark of
Grafana Labs; this worker is not affiliated with or endorsed by Grafana Labs or any vendor
named here.
