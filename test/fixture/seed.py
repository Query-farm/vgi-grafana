"""Seed the live Grafana fixture through its HTTP API (run by run-grafana.sh).

    python3 test/fixture/seed.py http://127.0.0.1:13000

Everything the file provisioning cannot express is created here, with fixed
UIDs and fixed 2026-01-01 timestamps so the recorded responses are stable to
read. Logs in as admin:admin (the throwaway fixture's login).
"""

import base64
import json
import sys
import time
import urllib.error
import urllib.request

BASE = sys.argv[1].rstrip("/")
AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()

T0 = 1767225600000  # 2026-01-01T00:00:00Z in ms
HOUR = 3600 * 1000


def call(method, path, body=None, org=None, ok=(200,)):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Authorization", AUTH)
    req.add_header("Content-Type", "application/json")
    if org is not None:
        req.add_header("X-Grafana-Org-Id", str(org))
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        msg = e.read().decode()
        if e.code in ok:
            return json.loads(msg) if msg else None
        raise SystemExit(f"{method} {path}: HTTP {e.code}: {msg}")


def folder(uid, title, parent=None):
    body = {"uid": uid, "title": title}
    if parent:
        body["parentUid"] = parent
    return call("POST", "/api/folders", body, ok=(200, 409, 412))


def dashboard(model, folder_uid, message):
    return call("POST", "/api/dashboards/db",
                {"dashboard": model, "folderUid": folder_uid, "overwrite": True, "message": message})


def main():
    # Folder tree: Operations (provisioned, uid ops) / Web; Sales / EMEA.
    folder("ops-web", "Web", "ops")
    folder("sales", "Sales")
    folder("sales-emea", "EMEA", "sales")

    call("POST", "/api/library-elements", {
        "uid": "lib-latency", "name": "Shared latency", "kind": 1, "folderUid": "ops",
        "model": {"type": "timeseries", "title": "Shared latency", "description": "p95 latency across services",
                  "datasource": {"type": "prometheus", "uid": "prom-prod"},
                  "targets": [{"refId": "A", "expr": "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))"}]},
    }, ok=(200, 400, 409))

    web = {
        "uid": "web-overview", "title": "Web Overview", "tags": ["web", "ops"],
        "schemaVersion": 41, "time": {"from": "now-24h", "to": "now"}, "refresh": "30s",
        "description": "Frontend traffic and latency.",
        "panels": [
            {"id": 1, "type": "timeseries", "title": "Latency (testdata)",
             "gridPos": {"x": 0, "y": 0, "w": 12, "h": 8},
             "datasource": {"type": "grafana-testdata-datasource", "uid": "testdata"},
             "targets": [{"refId": "A", "scenarioId": "csv_metric_values", "stringInput": "120,130,150,110"}]},
            {"id": 2, "type": "table", "title": "Slow pages",
             "gridPos": {"x": 12, "y": 0, "w": 12, "h": 8},
             "datasource": {"type": "grafana-postgresql-datasource", "uid": "pg-warehouse"},
             "targets": [{"refId": "A", "format": "table", "rawQuery": True,
                          "rawSql": "SELECT page, p95_ms FROM page_latency ORDER BY p95_ms DESC LIMIT 10"}]},
            {"id": 3, "type": "row", "title": "Details", "collapsed": True,
             "gridPos": {"x": 0, "y": 8, "w": 24, "h": 1},
             "panels": [
                 {"id": 4, "type": "timeseries", "title": "Requests by status",
                  "gridPos": {"x": 0, "y": 9, "w": 24, "h": 8},
                  "datasource": {"type": "prometheus", "uid": "prom-prod"},
                  "targets": [{"refId": "A", "expr": "sum by (code) (rate(http_requests_total{job=\"web\"}[5m]))"}]},
             ]},
            {"id": 5, "gridPos": {"x": 0, "y": 17, "w": 12, "h": 8},
             "libraryPanel": {"uid": "lib-latency", "name": "Shared latency"}},
            {"id": 6, "type": "text", "title": "Notes", "gridPos": {"x": 12, "y": 17, "w": 12, "h": 8},
             "options": {"content": "Owned by the web team."}},
        ],
    }
    dashboard(dict(web, description="Frontend traffic."), "ops-web", "first draft")
    dashboard(web, "ops-web", "add details row and library panel")

    dashboard({
        "uid": "revenue", "title": "EMEA Revenue", "tags": ["sales", "finance"], "schemaVersion": 41,
        "panels": [
            {"id": 1, "type": "barchart", "title": "Revenue by country",
             "gridPos": {"x": 0, "y": 0, "w": 24, "h": 10},
             "datasource": {"type": "grafana-postgresql-datasource", "uid": "pg-warehouse"},
             "targets": [
                 {"refId": "A", "format": "table", "rawQuery": True,
                  "rawSql": "SELECT country, sum(amount) FROM orders GROUP BY country"},
                 {"refId": "B", "datasource": {"type": "grafana-testdata-datasource", "uid": "testdata"},
                  "scenarioId": "csv_content", "csvContent": "country,target\nDE,100\nFR,80"},
             ]},
        ],
    }, "sales-emea", "initial")

    dashboard({"uid": "scratch", "title": "Scratchpad", "tags": [], "schemaVersion": 41, "panels": []},
              "", "empty dashboard in General")

    call("POST", "/api/user/stars/dashboard/uid/web-overview", ok=(200, 409))

    # Users, a team, a service account, a second org.
    ids = {}
    for login, name, role in (("alice", "Alice Analyst", "Editor"), ("bob", "Bob Builder", "Viewer")):
        r = call("POST", "/api/admin/users",
                 {"name": name, "email": f"{login}@example.com", "login": login, "password": "fixture-only-pw"},
                 ok=(200, 412))
        if r and "id" in r:
            ids[login] = r["id"]
        else:
            ids[login] = call("GET", f"/api/users/lookup?loginOrEmail={login}")["id"]
        call("PATCH", f"/api/org/users/{ids[login]}", {"role": role})
    team = call("POST", "/api/teams", {"name": "SRE", "email": "sre@example.com"}, ok=(200, 409))
    if not team or "teamId" not in team:
        team = {"teamId": call("GET", "/api/teams/search?name=SRE")["teams"][0]["id"]}
    for login in ("alice", "bob"):
        call("POST", f"/api/teams/{team['teamId']}/members", {"userId": ids[login]}, ok=(200, 400, 409))
    call("POST", "/api/teams", {"name": "Analysts"}, ok=(200, 409))
    sa = call("POST", "/api/serviceaccounts", {"name": "vgi-reader", "role": "Viewer"}, ok=(200, 400, 409))
    if sa and "id" in sa:
        call("POST", f"/api/serviceaccounts/{sa['id']}/tokens", {"name": "ci"})
    call("POST", "/api/orgs", {"name": "Partner Org"}, ok=(200, 409))

    # Annotations at fixed times.
    for i, (dash, panel, dt, text, tags, end) in enumerate([
        ("web-overview", 1, 0, "Deploy web v1.2.3", ["deploy", "web"], None),
        ("web-overview", 1, 2, "Deploy web v1.2.4", ["deploy", "web"], None),
        ("web-overview", 4, 3, "Incident: elevated 5xx", ["incident", "web"], 4),
        (None, None, 1, "Database maintenance", ["maintenance", "db"], 3),
        (None, None, 5, "Office party", ["social"], None),
        ("revenue", 1, 5, "Price change", ["finance"], None),
    ]):
        body = {"time": T0 + dt * HOUR, "text": text, "tags": tags}
        if dash:
            body["dashboardUID"] = dash
            body["panelId"] = panel
        if end:
            body["timeEnd"] = T0 + end * HOUR
        call("POST", "/api/annotations", body)

    # A silence on the db team's alerts.
    call("POST", "/api/alertmanager/grafana/api/v2/silences", {
        "matchers": [{"name": "team", "value": "db", "isRegex": False, "isEqual": True},
                     {"name": "alertname", "value": "Disk.*", "isRegex": True, "isEqual": True}],
        "startsAt": "2026-01-01T00:00:00Z", "endsAt": "2099-01-01T00:00:00Z",
        "createdBy": "alice", "comment": "db maintenance window"})

    # Wait for the high-latency rule to fire so Alertmanager has an alert.
    for _ in range(90):
        alerts = call("GET", "/api/alertmanager/grafana/api/v2/alerts")
        if alerts:
            break
        time.sleep(2)
    print(f"seeded {BASE}", file=sys.stderr)


if __name__ == "__main__":
    main()
