"""Strict replay (or record) server for the Grafana HTTP API used by the SQL tests.

    python3 test/fixture_server/server.py                       # replay recorded.json
    python3 test/fixture_server/server.py --record http://127.0.0.1:13000

Prints ``PORT:<n>`` on stdout once listening (ci/run-integration.sh waits for
it). A response is keyed by method, path, the sorted query parameters and,
for POST, the canonical (key-sorted) JSON body. ``recorded.json`` holds
responses captured from the live fixture (test/fixture/run-grafana.sh);
``handwritten.json`` holds hand-made responses for shapes a stock server will
not produce on demand (errors, NaN/Inf entities, legacy shapes). An
unrecorded request gets 404 "not recorded", so a changed request fails the
tests instead of passing silently.

Recording forwards to the live fixture with its throwaway admin:admin login;
the credentials the tests send never reach the upstream.

Environment:
  FIXTURE_FLAKY=1          answer every key with 429 + Retry-After: 0 once
  FIXTURE_REQUIRE_AUTH=1   demand credentials and an org: either
                           "Bearer test-token" or basic auth viewer:secret-pw,
                           plus X-Grafana-Org-Id: 1 (401/403 otherwise)
"""

import base64
import json
import os
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
RECORDED = os.path.join(HERE, "recorded.json")
HANDWRITTEN = os.path.join(HERE, "handwritten.json")

UPSTREAM = None
if "--record" in sys.argv:
    UPSTREAM = sys.argv[sys.argv.index("--record") + 1].rstrip("/")
UPSTREAM_AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()

LOCK = threading.Lock()


def load(path):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


FIXTURES = load(RECORDED)
FIXTURES.update(load(HANDWRITTEN))
SEEN = set()
GOOD_AUTH = {"Bearer test-token", "Basic " + base64.b64encode(b"viewer:secret-pw").decode()}


def key_of(method, path, params, body):
    items = sorted((k, v) for k, vs in params.items() for v in vs)
    key = method + " " + path + ("?" + urllib.parse.urlencode(items) if items else "")
    if body:
        key += " " + json.dumps(json.loads(body), sort_keys=True, separators=(",", ":"))
    return key


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body, headers=None):
        data = body if isinstance(body, bytes) else body.encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def error(self, status, msg):
        self.reply(status, json.dumps({"message": msg}))

    def handle_any(self, method, body=b""):
        url = urllib.parse.urlsplit(self.path)
        params = urllib.parse.parse_qs(url.query, keep_blank_values=True)
        if method == "POST" and url.path != "/api/ds/query":
            return self.error(405, "the fixture only accepts the read-only POST /api/ds/query")
        if os.environ.get("FIXTURE_REQUIRE_AUTH") == "1":
            if self.headers.get("Authorization") not in GOOD_AUTH:
                return self.error(401, "invalid API key")
            if self.headers.get("X-Grafana-Org-Id") != "1":
                return self.error(403, "missing X-Grafana-Org-Id: 1")
        try:
            key = key_of(method, url.path, params, body)
        except ValueError:
            return self.error(400, "body is not JSON")
        if os.environ.get("FIXTURE_FLAKY") == "1":
            with LOCK:
                first = key not in SEEN
                SEEN.add(key)
            if first:
                return self.reply(429, '{"message":"rate limit exceeded"}', {"Retry-After": "0"})
        if key in FIXTURES:
            entry = FIXTURES[key]
            return self.reply(entry["status"], entry["body"], entry.get("headers"))
        if UPSTREAM is None:
            return self.error(404, "not recorded: " + key)
        req = urllib.request.Request(UPSTREAM + self.path, data=body or None, method=method)
        req.add_header("Authorization", UPSTREAM_AUTH)
        req.add_header("Accept", "application/json")
        if body:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                status, data = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            status, data = e.code, e.read()
        with LOCK:
            FIXTURES[key] = {"status": status, "body": data.decode()}
            recorded = load(RECORDED)
            recorded[key] = FIXTURES[key]
            with open(RECORDED, "w") as f:
                json.dump(recorded, f, indent=0, sort_keys=True)
                f.write("\n")
        self.reply(status, data)

    def do_GET(self):
        self.handle_any("GET")

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        self.handle_any("POST", self.rfile.read(n))

    def do_PUT(self):
        self.error(405, "read-only fixture")

    do_DELETE = do_PATCH = do_PUT


def main():
    srv = ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("FIXTURE_PORT", "0"))), Handler)
    print(f"PORT:{srv.server_address[1]}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
