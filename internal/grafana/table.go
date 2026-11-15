// The table framework every Grafana table uses.
//
// A tableDef names its columns, docs and (for table functions) arguments, and
// supplies Next: fetch the next page of API records into rows, advancing the
// cursor in ScanState. The wrapper resolves the grafana secret on every call,
// compiles the pushed filters once (filters.go) so Next can turn them into
// API parameters, re-applies every pushed filter exactly, projects the
// requested columns and streams one API page per batch.
//
// ScanState's exported fields are gob-encoded by the SDK into the HTTP
// continuation token, so a long scan resumes where it stopped on any worker
// process. It never holds credentials: the secret is re-read on every call.

package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"
	"github.com/Query-farm/vgi-rpc-go/vgirpc"
	"github.com/apache/arrow-go/v18/arrow"
)

// rec is one decoded API object.
type rec = map[string]any

// col is one output column and how to read it from a record.
type col struct {
	F   arrow.Field
	Get func(r rec, s *scan) any
}

// fnDocs are a table's discovery docs.
type fnDocs struct {
	Title       string
	Category    string
	Keywords    string
	DocLLM      string
	DocMD       string
	Description string
	Examples    []vgi.CatalogExample
}

// tableDef defines one table or table function.
type tableDef struct {
	Name string
	Docs fnDocs
	Cols []col
	// Args is a zero value of the argument struct (nil: no arguments). It
	// must implement secretNamed.
	Args secretNamed
	// Catalog registers the definition as a catalog table (no arguments)
	// instead of a table function.
	Catalog bool
	// Folders loads the folder map (uid -> title, parent) before the first
	// page, for folder_path columns.
	Folders bool
	// Key lists the primary-key columns of a catalog table. DuckDB reads
	// only ENFORCED + ALL_ROWS constraints, so a key or foreign key is
	// declared only where Grafana guarantees it for every returned row.
	Key []string
	// FKs reference another table's primary key.
	FKs []foreignKey
	// Next fetches the next page into rows (full width, schema order) and
	// sets s.St.Done when nothing is left.
	Next func(ctx context.Context, s *scan) ([][]any, error)

	schema *arrow.Schema
}

// Schema is the full output schema built from Cols.
func (d *tableDef) Schema() *arrow.Schema {
	if d.schema == nil {
		fields := make([]arrow.Field, len(d.Cols))
		for i, c := range d.Cols {
			fields[i] = c.F
		}
		d.schema = arrow.NewSchema(fields, nil)
	}
	return d.schema
}

// rows maps records through Cols.
func (d *tableDef) rows(recs []rec, s *scan) [][]any {
	out := make([][]any, len(recs))
	for i, r := range recs {
		row := make([]any, len(d.Cols))
		for j, c := range d.Cols {
			if c.Get != nil {
				row[j] = c.Get(r, s)
			}
		}
		out[i] = row
	}
	return out
}

// secretNamed is implemented by every argument struct.
type secretNamed interface{ secret() string }

// foreignKey references another table's primary key.
type foreignKey struct {
	Cols  []string
	Table string
}

// noArgs is the argument struct of catalog tables.
type noArgs struct{}

func (noArgs) secret() string { return "" }

// FolderRef is one folder of the folder map.
type FolderRef struct {
	Title     string
	ParentUID string
}

// ScanState is the resumable cursor shared by every table.
type ScanState struct {
	ArgsJSON    string
	FiltersJSON string
	SecretName  string
	Started     bool
	Done        bool
	// Page is the next page number of a paged endpoint.
	Page int
	// Keys are fan-out keys (dashboard uids, team ids) still to visit, from KeyIdx.
	Keys   []string
	KeyIdx int
	// Cursor is an endpoint-specific continuation (continueToken, time bound).
	Cursor string
	// Seen holds ids already emitted at a time boundary (annotations).
	Seen []string
	// Folders is the folder map when the table needs folder paths.
	Folders map[string]FolderRef
	// Needed lists the projected column names (empty = all).
	Needed []string
}

// scan is the per-call context Next sees.
type scan struct {
	C      *conn
	St     *ScanState
	Preds  []*expr
	Def    *tableDef
	Params *vgi.ProcessParams
}

// args decodes the bound arguments into a.
func (s *scan) args(a any) error { return json.Unmarshal([]byte(s.St.ArgsJSON), a) }

// needs reports whether any of the named columns is projected.
func (s *scan) needs(names ...string) bool {
	if len(s.St.Needed) == 0 {
		return true
	}
	for _, n := range names {
		for _, m := range s.St.Needed {
			if n == m {
				return true
			}
		}
	}
	return false
}

type tableFunc struct {
	def  *tableDef
	name string
}

var _ vgi.TypedTableFunc[ScanState] = (*tableFunc)(nil)

func (t *tableFunc) Name() string { return t.name }

func (t *tableFunc) Metadata() vgi.FunctionMetadata {
	d := t.def.Docs
	tags := objectTags(docs{Title: d.Title, Category: d.Category, Keywords: d.Keywords, DocLLM: d.DocLLM, DocMD: d.DocMD})
	tags["vgi.result_columns_schema"] = mustJSON(schemaColumns(t.def.Schema()))
	return vgi.FunctionMetadata{
		Description:        d.Description,
		Stability:          vgi.StabilityVolatile,
		ProjectionPushdown: true,
		FilterPushdown:     true,
		RequiredSecrets:    secretRequirements,
		Tags:               withExamples(tags, d.Examples),
		Examples:           d.Examples,
	}
}

func (t *tableFunc) ArgumentSpecs() []vgi.ArgSpec {
	if t.def.Args == nil {
		return nil
	}
	return vgi.DeriveArgSpecs(t.def.Args)
}

// bindArgs decodes the call's arguments into a fresh value of the def's Args type.
func (t *tableFunc) bindArgs(a *vgi.Arguments) (secretNamed, error) {
	if t.def.Args == nil {
		return noArgs{}, nil
	}
	v, err := newArgs(t.def.Args)
	if err != nil {
		return nil, err
	}
	if err := vgi.BindArgs(a, v); err != nil {
		return nil, err
	}
	return v.(secretNamed), nil
}

func (t *tableFunc) secretName(a secretNamed, opts *vgi.AttachOptions) string {
	if n := a.secret(); n != "" {
		return n
	}
	return attachSecretName(opts)
}

func (t *tableFunc) OnBind(params *vgi.BindParams) (*vgi.BindResponse, error) {
	a, err := t.bindArgs(params.Args)
	if err != nil {
		return nil, err
	}
	if v, ok := a.(interface{ validate() error }); ok {
		if err := v.validate(); err != nil {
			return nil, err
		}
	}
	if r := secretScope(params, t.secretName(a, params.AttachOptions())); r != nil {
		return r, nil
	}
	return vgi.BindSchema(t.def.Schema())
}

func (t *tableFunc) NewState(params *vgi.ProcessParams) (*ScanState, error) {
	a, err := t.bindArgs(params.Args)
	if err != nil {
		return nil, err
	}
	argsJSON, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	preds, err := compileFilters(params.CurrentPushdownFilters, t.def.Schema())
	if err != nil {
		return nil, err
	}
	filtersJSON, err := json.Marshal(preds)
	if err != nil {
		return nil, err
	}
	st := &ScanState{ArgsJSON: string(argsJSON), FiltersJSON: string(filtersJSON),
		SecretName: t.secretName(a, params.AttachOptions()), Page: 1}
	if params.OutputSchema != nil && params.OutputSchema.NumFields() < t.def.Schema().NumFields() {
		for _, f := range params.OutputSchema.Fields() {
			st.Needed = append(st.Needed, f.Name)
		}
		// Filtered columns are needed for exact re-evaluation.
		for _, p := range preds {
			st.Needed = append(st.Needed, exprColumns(p)...)
		}
	}
	if _, err := resolveConn(params.Secrets, st.SecretName); err != nil {
		return nil, err
	}
	slog.Debug("grafana scan", "table", t.def.Name, "filters", st.FiltersJSON, "needed", st.Needed)
	return st, nil
}

func (t *tableFunc) Process(ctx context.Context, params *vgi.ProcessParams, st *ScanState, out *vgirpc.OutputCollector) error {
	if st.Done {
		return out.Finish()
	}
	c, err := resolveConn(params.Secrets, st.SecretName)
	if err != nil {
		return err
	}
	var preds []*expr
	if err := json.Unmarshal([]byte(st.FiltersJSON), &preds); err != nil {
		return err
	}
	s := &scan{C: c, St: st, Preds: preds, Def: t.def, Params: params}
	if !st.Started {
		if t.def.Folders {
			if st.Folders, err = loadFolders(ctx, c); err != nil {
				return err
			}
		}
		st.Started = true
	}
	full := t.def.Schema()
	env := newRowEnv(full)
	for !st.Done {
		rows, err := t.def.Next(ctx, s)
		if err != nil {
			return err
		}
		kept := rows[:0]
		for _, r := range rows {
			env.row = r
			ok, err := matchAll(preds, env)
			if err != nil {
				return err
			}
			if ok {
				kept = append(kept, r)
			}
		}
		if len(kept) > 0 {
			batch, err := buildBatch(params.OutputSchema, full, kept)
			if err != nil {
				return err
			}
			return out.Emit(batch)
		}
	}
	return out.Finish()
}

// newTable wraps a definition as a table function.
func newTable(def *tableDef) vgi.TableFunction {
	return vgi.AsTableFunction[ScanState](&tableFunc{def: def, name: def.Name})
}

// catalogTable wraps a definition as a catalog table. Its scan function gets
// its own name (<table>_scan): vgi-go 0.42.0 resolves a catalog table's scan
// by function name, so it must not share a name with a table function.
func catalogTable(def *tableDef) vgi.CatalogTable {
	full := def.Schema()
	comments := map[string]string{}
	for _, f := range full.Fields() {
		c, _ := f.Metadata.GetValue("comment")
		comments[f.Name] = c
	}
	d := def.Docs
	tags := withExamples(objectTags(docs{Title: d.Title, Category: d.Category, Keywords: d.Keywords, DocLLM: d.DocLLM, DocMD: d.DocMD}), d.Examples)
	tags["domain"] = "observability"
	return vgi.CatalogTable{
		Constraints:    constraints(def),
		Name:           def.Name,
		Comment:        d.Description,
		Columns:        full,
		Function:       vgi.AsTableFunction[ScanState](&tableFunc{def: def, name: def.Name + "_scan"}),
		ColumnComments: comments,
		Tags:           tags,
	}
}

// schemaColumns describes a schema for vgi.result_columns_schema.
func schemaColumns(s *arrow.Schema) []resultColumn {
	out := make([]resultColumn, s.NumFields())
	for i, f := range s.Fields() {
		d, _ := f.Metadata.GetValue("comment")
		out[i] = resultColumn{Name: f.Name, Type: duckType(f.Type), Description: d}
	}
	return out
}

func duckType(t arrow.DataType) string {
	switch t.ID() {
	case arrow.STRING:
		return "VARCHAR"
	case arrow.INT64:
		return "BIGINT"
	case arrow.INT32:
		return "INTEGER"
	case arrow.FLOAT64:
		return "DOUBLE"
	case arrow.BOOL:
		return "BOOLEAN"
	case arrow.TIMESTAMP:
		return "TIMESTAMP WITH TIME ZONE"
	case arrow.MAP:
		return "MAP(VARCHAR, VARCHAR)"
	case arrow.EXTENSION:
		return "JSON"
	case arrow.LIST:
		return duckType(t.(*arrow.ListType).Elem()) + "[]"
	case arrow.STRUCT:
		st := t.(*arrow.StructType)
		s := "STRUCT("
		for i, f := range st.Fields() {
			if i > 0 {
				s += ", "
			}
			s += f.Name + " " + duckType(f.Type)
		}
		return s + ")"
	}
	return t.String()
}

// tablesByName indexes allDefs for foreign-key targets.
func tablesByName() map[string]*tableDef {
	m := map[string]*tableDef{}
	for _, d := range allDefs() {
		m[d.Name] = d
	}
	return m
}

func colPaths(cols []string) [][]string {
	out := make([][]string, len(cols))
	for i, c := range cols {
		out[i] = []string{c}
	}
	return out
}

// constraints builds the table's NOT NULL, primary key and foreign key records.
func constraints(def *tableDef) [][]byte {
	rec := func(spec vgi.ConstraintRecordSpec) []byte {
		spec.SchemaPath, spec.Table = []string{"main"}, def.Name
		b, err := vgi.ConstraintRecord(spec)
		if err != nil {
			panic(fmt.Sprintf("constraint %s.%s: %v", def.Name, spec.ID, err))
		}
		return b
	}
	var out [][]byte
	for _, f := range def.Schema().Fields() {
		if !f.Nullable {
			out = append(out, rec(vgi.ConstraintRecordSpec{ID: f.Name + "_not_null", Kind: vgi.ConstraintNotNull,
				Columns: [][]string{{f.Name}}, Enforcement: vgi.ConstraintEnforced, Validation: vgi.ConstraintAllRows}))
		}
	}
	if len(def.Key) > 0 {
		out = append(out, rec(vgi.ConstraintRecordSpec{ID: "pk", Kind: vgi.ConstraintPrimaryKey, Columns: colPaths(def.Key),
			Enforcement: vgi.ConstraintEnforced, Validation: vgi.ConstraintAllRows}))
	}
	defs := tablesByName()
	for _, fk := range def.FKs {
		target, ok := defs[fk.Table]
		if !ok || len(target.Key) != len(fk.Cols) {
			panic(fmt.Sprintf("foreign key %s -> %s: no matching primary key", def.Name, fk.Table))
		}
		out = append(out, rec(vgi.ConstraintRecordSpec{ID: "fk_" + strings.Join(fk.Cols, "_") + "_" + fk.Table, Kind: vgi.ConstraintForeignKey,
			Columns: colPaths(fk.Cols), Enforcement: vgi.ConstraintEnforced, Validation: vgi.ConstraintAllRows,
			References: &vgi.ConstraintReference{SchemaPath: []string{"main"}, Table: fk.Table, Columns: colPaths(target.Key), ConstraintID: "pk"}}))
	}
	return out
}
