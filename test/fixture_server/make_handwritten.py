"""Regenerate handwritten.json: responses a stock Grafana will not produce on demand.

    python3 test/fixture_server/make_handwritten.py

Keys use the replay server's format (server.py key_of): method, path, sorted
query, canonical JSON body.
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
FROM, TO = "1767225600000", "1767229200000"  # 2026-01-01 00:00 .. 01:00 UTC


def ds_key(uid, query):
    q = dict(query, refId="A", datasource={"uid": uid})
    body = {"from": FROM, "queries": [q], "to": TO}
    return "POST /api/ds/query " + json.dumps(body, sort_keys=True, separators=(",", ":"))


out = {
    # A data source that fails: Grafana answers 400 with the per-refId error.
    ds_key("broken", {"expr": "up"}): {
        "status": 400,
        "body": json.dumps({"results": {"A": {"status": 500, "error": "connection refused", "frames": []}}}),
    },
    # NaN / +Inf / -Inf travel out of band as entities; nanos add sub-ms time.
    ds_key("entities", {"expr": "x"}): {
        "status": 200,
        "body": json.dumps({"results": {"A": {"status": 200, "frames": [{
            "schema": {"refId": "A", "meta": {"type": "timeseries-multi"}, "fields": [
                {"name": "time", "type": "time", "typeInfo": {"frame": "time.Time"}},
                {"name": "value", "type": "number", "typeInfo": {"frame": "float64", "nullable": True},
                 "labels": {"series": "special"}, "config": {"unit": "ms"}},
            ]},
            "data": {
                "values": [[1767225600000, 1767225601000, 1767225602000, 1767225603000, 1767225604000],
                           [1.5, None, None, None, None]],
                "entities": [None, {"NaN": [1], "Inf": [2], "NegInf": [3]}],
                "nanos": [[0, 2000, 0, 0, 0], None],
            },
        }]}}}),
    },
    # A credential that may not list every org (PAGE_SIZE 7 in the tests).
    "GET /api/orgs?page=0&perpage=7": {"status": 403, "body": json.dumps({"message": "You'll need additional permissions to perform this action. Permissions needed: orgs:read"})},
}

with open(os.path.join(HERE, "handwritten.json"), "w") as f:
    json.dump(out, f, indent=0, sort_keys=True)
    f.write("\n")
print(f"wrote {len(out)} handwritten responses")
