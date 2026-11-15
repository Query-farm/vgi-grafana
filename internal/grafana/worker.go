// Package grafana exposes the Grafana HTTP API (OSS, Enterprise, Grafana
// Cloud) to DuckDB as read-only catalog tables and table functions.
//
// To add a table: write a tableDef (see bi.go, panels.go, access.go,
// alerting.go, annotations.go or dsquery.go), then list it in allDefs below.
//
// Run it:
//
//	vgi-grafana-worker                      # stdio: what DuckDB launches by default
//	vgi-grafana-worker --http --port 8000   # HTTP: for Docker / hosted deploys
//	vgi-grafana-worker --help
//
// Attach it from DuckDB:
//
//	ATTACH 'grafana' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-grafana-worker');
//	CREATE SECRET (TYPE grafana, BASE_URL 'https://grafana.example.com', TOKEN '...');
package grafana

import (
	"github.com/Query-farm/vgi-go/vgi"
	"github.com/apache/arrow-go/v18/arrow"
)

// allDefs lists every table and table function, in documentation order.
func allDefs() []*tableDef {
	return []*tableDef{
		dashboardsDef, foldersDef, panelsDef, panelQueriesDef, dashboardVersionsDef, libraryPanelsDef, connectionsDef, searchDef,
		usersDef, teamsDef, teamMembersDef, serviceAccountsDef, orgsDef, healthDef,
		alertRulesDef, alertRuleQueriesDef, ruleGroupsDef, alertRuleStatesDef, contactPointsDef, notificationPoliciesDef,
		muteTimingsDef, alertsDef, silencesDef, alertStateTransitionsDef,
		annotationsDef, dsQueryDef,
	}
}

// allExamples lists every object's examples for the schema-level tag.
func allExamples() []exampleQuery {
	// One example per object keeps the schema tag within agent tool limits;
	// every example stays on its own object.
	var lists [][]vgi.CatalogExample
	for _, d := range allDefs() {
		lists = append(lists, d.Docs.Examples[:1])
	}
	for _, f := range toolScalars() {
		lists = append(lists, f.examples[:1])
	}
	return exampleQueries(lists...)
}

// attachOptions: secret_name selects the grafana secret for every table.
var attachOptions = []vgi.AttachOptionSpec{{
	Name: AttachSecretOption,
	Description: "Name of the grafana secret every table and function of this catalog reads (default: the session's grafana secret). " +
		"Attach the worker once per Grafana instance, each with its own secret_name.",
	Type: arrow.BinaryTypes.String,
}}

// NewWorker builds the worker: one catalog, one main schema. Extra options
// (logging, storage) are appended after the catalog options.
func NewWorker(opts ...vgi.WorkerOption) *vgi.Worker {
	sourceURL, version := SourceURL, Version
	base := []vgi.WorkerOption{
		vgi.WithCatalogName(CatalogName),
		vgi.WithCatalogComment(CatalogDocMD),
		vgi.WithCatalogTags(CatalogTags),
		vgi.WithCatalogInfo(vgi.CatalogInfo{SourceURL: &sourceURL, ImplementationVersion: &version}),
		vgi.WithSchemaComments(map[string]string{"main": SchemaDocMD}),
		vgi.WithSchemaTags(map[string]map[string]string{"main": schemaTags(allExamples())}),
		vgi.WithSecretTypes(APISecretType),
		vgi.WithAttachOptions(attachOptions...),
	}
	w := vgi.NewWorker(append(base, opts...)...)
	registerEvaluator()
	registerTools(w)
	for _, d := range allDefs() {
		if d.Catalog {
			w.RegisterCatalogTable("main", catalogTable(d))
		} else {
			w.RegisterTable(newTable(d))
		}
	}
	return w
}
