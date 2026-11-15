// Command vgi-grafana-worker serves the grafana catalog to DuckDB.
//
//	vgi-grafana-worker                                  # stdio (what DuckDB launches)
//	vgi-grafana-worker --http --host 0.0.0.0 --port 8000  # HTTP, GET /health
//	vgi-grafana-worker --unix /tmp/grafana.sock         # AF_UNIX (launch: transport)
//
// Attach it from DuckDB:
//
//	ATTACH 'grafana' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-grafana-worker');
package main

import (
	"os"

	"github.com/Query-farm/vgi-go/vgi"

	"github.com/Query-farm/vgi-grafana/internal/grafana"
	"github.com/Query-farm/vgi-grafana/internal/serve"
)

func main() {
	serve.Main("vgi-grafana-worker", os.Args[1:], func() *vgi.Worker { return grafana.NewWorker() })
}
