# Changelog

## 0.1.0 (unreleased)

First release: the Grafana HTTP API (OSS, Enterprise, Grafana Cloud) as read-only SQL.

- Content: `dashboards`, `folders`, `panels`, `panel_queries`, `dashboard_versions`,
  `library_panels`, `connections` (data sources) and `search(...)`; BI tables in the
  shared vgi-looker / vgi-tableau / vgi-metabase column vocabulary.
- Access: `users`, `teams`, `team_members`, `service_accounts`, `orgs`, `health`.
- Alerting: `alert_rules`, `alert_rule_queries`, `rule_groups`, `alert_rule_states`,
  `contact_points`, `notification_policies`, `mute_timings`, `alerts`, `silences`,
  `alert_state_transitions`.
- `annotations` with time-window, dashboard, panel, tag and type pushdown.
- `ds_query(datasource_uid, query_json, start_time, end_time)`: any data source query
  through `/api/ds/query`, decoded from data frames into long rows.
- Offline scalars `duration_seconds` and `redact_url`.
- `grafana` secret type (token or basic auth, `ORG_ID`, `PAGE_SIZE`); ATTACH option
  `secret_name`.

Started from vgi-starter-go v1.3.0. Targets: vgi-go 0.42 · VGI protocol 3.3 · Haybarn
2.0.0rc11 · vgi-lint-check 2.x (unpinned). Fixture recorded from grafana/grafana-oss 12.1.1.
