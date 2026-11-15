#!/usr/bin/env bash
# Run the live Grafana fixture used to record test/fixture_server/recorded.json.
#
#   test/fixture/run-grafana.sh start   # prints http://127.0.0.1:13000 once seeded
#   test/fixture/run-grafana.sh stop
#
# grafana/grafana-oss (about 200 MB RAM, capped at 512 MB) is provisioned from
# files (provisioning/: TestData, Prometheus and PostgreSQL data sources, one
# file dashboard, alert rules, contact points, a notification policy tree and
# mute timings), then seed.py adds the rest through the HTTP API (nested
# folders, dashboards with versions, a library panel, users, a team, a service
# account, a second org, annotations and a silence). The admin login is
# admin:admin; it never leaves this machine.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
NAME="${GRAFANA_FIXTURE_NAME:-vgi-grafana-fixture}"
PORT="${GRAFANA_FIXTURE_PORT:-13000}"
IMAGE="${GRAFANA_IMAGE:-grafana/grafana-oss:12.1.1}"
URL="http://127.0.0.1:$PORT"

case "${1:-}" in
  start)
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    docker run -d --name "$NAME" --memory 512m -p "127.0.0.1:$PORT:3000" \
      -e GF_SECURITY_ADMIN_USER=admin -e GF_SECURITY_ADMIN_PASSWORD=admin \
      -e GF_ANALYTICS_REPORTING_ENABLED=false -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
      -e GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false -e GF_NEWS_NEWS_FEED_ENABLED=false \
      -e GF_SERVER_ROOT_URL="http://grafana.example.com/" \
      -v "$HERE/provisioning:/etc/grafana/provisioning:ro" \
      -v "$HERE/dashboards:/var/lib/grafana/dashboards:ro" \
      "$IMAGE" >/dev/null
    for _ in $(seq 1 120); do
      curl -fsS "$URL/api/health" >/dev/null 2>&1 && break
      sleep 1
    done
    curl -fsS "$URL/api/health" >/dev/null
    python3 "$HERE/seed.py" "$URL"
    echo "$URL"
    ;;
  stop)
    docker rm -f "$NAME" >/dev/null
    ;;
  *)
    echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
