// This file implements column-level SQL lineage on top of the Polyglot SQL
// engine. It is the Go port of the Python `sql_lineage.py` module: given a SQL
// statement it returns, for every physical source table, the set of source
// columns that flow into the statement's result.
//
// Column lineage is computed for every statement that contains a query —
// SELECT, UNION, CREATE VIEW, CREATE TABLE AS SELECT, INSERT ... SELECT, etc.
// (CREATE VIEW / CREATE TABLE AS / INSERT are simply unwrapped to their inner
// query first). The rules are:
//
//   - Only columns that actually reach the result are reported; columns
//     appearing only in filter positions (WHERE / JOIN ... ON / GROUP BY /
//     ORDER BY / ...) are excluded.
//   - Wildcards (SELECT *, t.*) are expanded against the supplied metadata, and
//     an ambiguous unqualified column is attributed to every in-scope candidate
//     source table. Metadata for tables not referenced by the query is ignored.
//   - Source tables are resolved to their root physical tables (CTEs,
//     subqueries and set operations are seen through), and every source table
//     is listed even if no column flows from it.
//
// Unlike the Python version there is no process pool or LRU cache here: that
// infrastructure existed only to work around CPython's GIL for a pure-Python
// parser. The Polyglot client does the heavy lifting in native code and is safe
// for concurrent use, so callers can call LineageSourceColumns directly (and add
// their own caching if desired).

package easysql

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// OpenLineage metadata required by the engine but irrelevant to the
// source-column result. These are only the defaults: callers can override the
// producer with WithLineageProducer and the dataset namespace with
// WithLineageNamespace. lineageOutputName is the fixed name of the
// required-but-irrelevant OpenLineage output dataset.
const (
	lineageProducer   = "https://github.com/dcalsky/easysql"
	lineageNamespace  = "easysql"
	lineageOutputName = "result"
)

// innerQueryKeys are the AST keys under which a wrapping statement (CREATE VIEW,
// CREATE TABLE AS, INSERT ... SELECT, ...) holds the query it wraps. Hoisted to
// avoid re-allocating the slice on every call.
var innerQueryKeys = []string{"query", "as_select", "expression", "this"}

// lineageOptions configures the lineage and column-analysis calls.
type lineageOptions struct {
	dialect   string
	producer  string
	namespace string
	metadata  map[string][]string
}

// LineageOption configures LineageSourceColumns, ParseColumns,
// ReferencedColumns, and ReferencedColumnUsages.
type LineageOption func(*lineageOptions)

// lineageDialectAliases maps convenient dialect spellings to the token the
// Polyglot analysis engine expects, so a value that parses but is rejected by
// AnalyzeQuery (e.g. "postgres", which the analyzer expects as "postgresql")
// does not surface as an ErrInternal. Dialects not listed here pass through
// unchanged.
var lineageDialectAliases = map[string]string{
	"postgres":   "postgresql",
	"postgresql": "postgresql",
	"pg":         "postgresql",
}

// normalizeLineageDialect trims and lower-cases the configured dialect,
// defaulting blank values to "trino" and translating known aliases to the
// engine's expected token.
func normalizeLineageDialect(dialect string) string {
	d := strings.ToLower(strings.TrimSpace(dialect))
	if d == "" {
		return "trino"
	}
	if mapped, ok := lineageDialectAliases[d]; ok {
		return mapped
	}
	return d
}

// WithLineageDialect selects the SQL dialect used to parse and analyze the
// statement (e.g. "trino", "hive", "spark", "postgres", "mysql"). It defaults
// to "trino".
func WithLineageDialect(dialect string) LineageOption {
	return func(o *lineageOptions) { o.dialect = dialect }
}

// WithLineageProducer sets the OpenLineage `_producer` URI recorded on the
// emitted lineage events. It is pure provenance metadata that identifies the
// program producing the lineage; it does not affect the source-column result
// LineageSourceColumns returns. It defaults to lineageProducer.
func WithLineageProducer(producer string) LineageOption {
	return func(o *lineageOptions) { o.producer = producer }
}

// WithLineageNamespace sets the OpenLineage dataset namespace used for the
// input/output datasets of the emitted lineage events. Like the producer it is
// pure provenance metadata and does not affect the source-column result
// LineageSourceColumns returns. It defaults to lineageNamespace.
func WithLineageNamespace(namespace string) LineageOption {
	return func(o *lineageOptions) { o.namespace = namespace }
}

// WithLineageMetadata supplies table metadata used to expand wildcards
// (SELECT *, t.*) and to resolve ambiguous unqualified columns among the query's
// source tables. Keys are fully-qualified table names ("catalog.schema.table" or
// "schema.table") and values are their column lists. Catalog entries for tables
// not referenced by the query are ignored. It corresponds to the Python
// `metadata` argument backed by DummyMetaDataProvider.
func WithLineageMetadata(metadata map[string][]string) LineageOption {
	return func(o *lineageOptions) { o.metadata = metadata }
}

func configuredLineageOptions(opts ...LineageOption) (lineageOptions, error) {
	cfg := lineageOptions{dialect: "trino", producer: lineageProducer, namespace: lineageNamespace}
	for i, opt := range opts {
		if opt == nil {
			return lineageOptions{}, fmt.Errorf("easysql: lineage option %d must not be nil", i)
		}
		opt(&cfg)
	}
	cfg.dialect = normalizeLineageDialect(cfg.dialect)
	if strings.TrimSpace(cfg.producer) == "" {
		cfg.producer = lineageProducer
	}
	if strings.TrimSpace(cfg.namespace) == "" {
		cfg.namespace = lineageNamespace
	}
	return cfg, nil
}

// LineageSourceColumns returns, for each physical source table referenced by sql,
// the sorted list of source columns that flow into the statement's result. It
// works for any statement that contains a query (SELECT, UNION, CREATE VIEW,
// CREATE TABLE AS, INSERT ... SELECT, ...). UPDATE assignment values and MERGE
// UPDATE/INSERT values are also resolved structurally, including values sourced
// through CTEs or derived tables.
//
// It is the Go equivalent of the Python `get_source_table_columns`. The result
// keys are the fully-qualified table names as resolved by the analyzer (e.g.
// "hive.raw.orders"); every source table appears in the map even when no column
// flows from it. The native SQL engine is loaded automatically on first use
// (see Init), so no setup is required.
func LineageSourceColumns(sql string, opts ...LineageOption) (map[string][]string, error) {
	client, err := defaultClient()
	if err != nil {
		return nil, err
	}
	req, err := prepareLineage(client, sql, opts...)
	if err != nil {
		return nil, err
	}

	// Trace the complete query exactly once. Polyglot handles set operations
	// natively, including their value and filter branches.
	if req.innerSQL != "" {
		if err := aggregateColumns(client, req.innerSQL, req.cfg, req.schema, req.tableCols); err != nil {
			return nil, err
		}
	}

	return sortedResult(req.tableCols), nil
}

// lineageRequest carries the setup for one full-query OpenLineage analysis.
type lineageRequest struct {
	cfg       lineageOptions
	schema    *polyglot.ValidationSchema
	innerSQL  string
	tableCols map[string]map[string]struct{}
}

// prepareLineage performs the dialect-agnostic setup shared by every lineage
// driver: parse the statement, unwrap it to its inner query, render that query
// back to SQL, build the validation schema, and seed every root physical source
// table with an empty column set (so a table that contributes no flowing column
// still appears in the result). A statement with no query at all yields an empty
// (non-nil) result map.
func prepareLineage(client *polyglot.Client, sql string, opts ...LineageOption) (*lineageRequest, error) {
	if client == nil {
		return nil, errors.New("easysql: nil polyglot client")
	}

	cfg, err := configuredLineageOptions(opts...)
	if err != nil {
		return nil, err
	}

	stmt, err := parseFirstStatement(client, sql, cfg.dialect)
	if err != nil {
		return nil, err
	}

	// Find the query body to analyze. CREATE VIEW / CREATE TABLE AS /
	// INSERT ... SELECT are unwrapped to their inner query; a bare SELECT/UNION
	// is its own body. Column lineage is computed for all of them alike.
	inner := innerQuery(stmt)
	if inner == nil {
		// Polyglot does not expose OpenLineage for UPDATE/MERGE, even when their
		// assigned values come from query-bearing CTEs or derived tables. Resolve
		// those value positions structurally so a real source flow is not silently
		// reported as empty. Filter-only ON/WHERE/WHEN columns stay excluded.
		if tableCols, handled, err := lineageDMLValueColumns(stmt, cfg.metadata); err != nil {
			return nil, err
		} else if handled {
			return &lineageRequest{cfg: cfg, tableCols: tableCols}, nil
		}
		// A statement with no query at all (e.g. CREATE TABLE (...) / DROP):
		// nothing to analyze, so there are no source tables or columns. The
		// driver returns an empty result.
		return &lineageRequest{cfg: cfg, tableCols: map[string]map[string]struct{}{}}, nil
	}

	innerSQL, err := generateStatement(client, inner, cfg.dialect)
	if err != nil {
		return nil, err
	}

	// Seed every root physical source table with an empty set so a table that
	// contributes no flowing column still appears in the result.
	sources, err := sourceTables(client, innerSQL, cfg.dialect)
	if err != nil {
		return nil, err
	}
	tableCols := map[string]map[string]struct{}{}
	for _, name := range sources {
		ensureSet(tableCols, name)
	}

	schema := metadataToSchema(metadataForSources(cfg.metadata, sources))

	return &lineageRequest{
		cfg:       cfg,
		schema:    schema,
		innerSQL:  innerSQL,
		tableCols: tableCols,
	}, nil
}

// aggregateColumns runs OpenLineage column lineage for a complete query and
// folds its direct value-source columns into tableCols.
func aggregateColumns(
	client *polyglot.Client,
	querySQL string,
	cfg lineageOptions,
	schema *polyglot.ValidationSchema,
	tableCols map[string]map[string]struct{},
) error {
	olOpts := polyglot.OpenLineageOptions{
		Dialect:          cfg.dialect,
		Producer:         cfg.producer,
		DatasetNamespace: cfg.namespace,
		OutputDataset:    &polyglot.OpenLineageDatasetID{Namespace: cfg.namespace, Name: lineageOutputName},
	}
	if schema != nil {
		olOpts.Schema = schema
	}

	res, err := client.OpenLineageColumnLineage(querySQL, olOpts)
	if err != nil {
		return fmt.Errorf("%w: column lineage failed: %v", ErrInternal, err)
	}

	// Fold only DIRECT input fields into tableCols. Set-operation right branches
	// of INTERSECT/EXCEPT are reported as INDIRECT/FILTER by Polyglot: they
	// decide whether a left value survives, but do not produce an output value.
	// A source can be both DIRECT and FILTER for one output, so retain it whenever
	// any transformation is DIRECT.
	sourceless := false
	for _, field := range res.Facet.Fields {
		if len(field.InputFields) == 0 {
			sourceless = true
		}
		for _, in := range field.InputFields {
			if in.Name == "" || in.Field == "" {
				continue
			}
			if !hasDirectTransformation(in.Transformations) {
				continue
			}
			scope, ok := resolveScopeTable(tableCols, in.Name)
			if !ok {
				continue
			}
			ensureSet(tableCols, scope)[in.Field] = struct{}{}
		}
	}

	// Some output fields come back with NO source column at all: count(*),
	// literal projections, unqualified aggregate args the engine fails to
	// resolve, scalar-subquery aliases. The engine only exposes their output
	// NAME (an alias like "c" or a synthetic "_0"), which is not a source
	// column. Rather than crediting that phantom name, recover the REAL source
	// columns that flow into the result by resolving the query's projections
	// structurally (recursing scalar subqueries, excluding filter positions).
	if sourceless {
		if err := creditFlowColumns(client, querySQL, cfg, tableCols); err != nil {
			return err
		}
	}
	return nil
}

// hasDirectTransformation reports whether OpenLineage identifies an input as
// contributing a value to the output. Inputs with only INDIRECT/FILTER
// transformations remain represented by their seeded physical tables but do
// not contribute source columns.
func hasDirectTransformation(transformations []polyglot.OpenLineageTransformation) bool {
	for _, transformation := range transformations {
		if strings.EqualFold(strings.TrimSpace(transformation.Type), "DIRECT") {
			return true
		}
	}
	return false
}

// creditFlowColumns resolves querySQL's projection flow structurally and folds
// the columns that reach the result into tableCols. It is used to recover the
// real source columns of output fields the engine returns with no source column
// (count(*), literals, unqualified aggregates, scalar-subquery aliases), keyed
// on the actual referenced column rather than the fabricated output name.
func creditFlowColumns(
	client *polyglot.Client,
	querySQL string,
	cfg lineageOptions,
	tableCols map[string]map[string]struct{},
) error {
	stmt, err := parseFirstStatement(client, querySQL, cfg.dialect)
	if err != nil {
		return err
	}
	inner := innerQuery(stmt)
	if inner == nil {
		return nil
	}
	q, err := decodeQueryMap(inner)
	if err != nil {
		return err
	}

	rr := &refResolver{metadata: cfg.metadata, result: map[string]map[string]struct{}{}, flowOnly: true}
	out := rr.resolveQuery(q, nil)

	// Record only the (table, column) refs that flow into the query's result.
	// The "*" sentinel (an unexpandable star) is not a concrete column and is
	// dropped, matching the engine's behavior for a wildcard without metadata.
	for _, name := range out.names {
		for _, ref := range out.byName[strings.ToLower(name)] {
			if ref.table == "" || ref.col == "" || ref.col == "*" {
				continue
			}
			scope, ok := resolveScopeTable(tableCols, ref.table)
			if !ok {
				continue
			}
			ensureSet(tableCols, scope)[ref.col] = struct{}{}
		}
	}
	return nil
}

// resolveScopeTable maps an OpenLineage input dataset name to the fully-qualified
// scope table key seeded from the query's base tables.
func resolveScopeTable(tableCols map[string]map[string]struct{}, inputName string) (string, bool) {
	if _, ok := tableCols[inputName]; ok {
		return inputName, true
	}
	var matches []string
	for scope := range tableCols {
		if tableRefMatches(inputName, scope) {
			matches = append(matches, scope)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

func tableRefMatches(a, b string) bool {
	return hasTableSuffix(a, b) || hasTableSuffix(b, a)
}

// hasTableSuffix reports whether the fully-qualified name `qualified` ends with
// the table reference `ref` on a name-segment boundary: either the two are
// equal, or `qualified` ends with ".ref". The dot-boundary check is what stops
// a query table `raw.orders` from spuriously matching an unrelated catalog key
// `hive.braw.orders` (schema "braw", not "raw"), which a plain strings.HasSuffix
// would accept.
func hasTableSuffix(qualified, ref string) bool {
	if ref == "" {
		return false
	}
	if qualified == ref {
		return true
	}
	return strings.HasSuffix(qualified, "."+ref)
}

// metadataForSources returns the subset of metadata for tables the query reads.
// Catalog entries for unrelated tables are excluded so they cannot influence
// OpenLineage resolution or the unresolved-column heuristic.
func metadataForSources(metadata map[string][]string, sources []string) map[string][]string {
	if len(metadata) == 0 || len(sources) == 0 {
		return metadata
	}
	out := make(map[string][]string, len(sources))
	for _, src := range sources {
		key := metadataKeyForTable(metadata, src)
		if cols, ok := metadata[key]; ok {
			out[key] = append([]string{}, cols...)
			continue
		}
		if cols, ok := lookupMetadataColumns(metadata, src); ok {
			out[key] = cols
			continue
		}
		out[src] = []string{}
	}
	return out
}

func metadataKeyForTable(metadata map[string][]string, tableRef string) string {
	if _, ok := metadata[tableRef]; ok {
		return tableRef
	}
	var found string
	var count int
	for key := range metadata {
		if hasTableSuffix(key, tableRef) {
			found = key
			count++
		}
	}
	if count == 1 {
		return found
	}
	return tableRef
}

// sourceTables returns the fully-qualified root physical tables that querySQL
// reads from. It uses the analyzer's base-table facts, which see through CTEs,
// subqueries and set operations. No schema is supplied: base tables are derived
// structurally from the FROM clauses, and passing a schema would make the
// analyzer fail on queries with ambiguous unqualified columns.
func sourceTables(client *polyglot.Client, querySQL, dialect string) ([]string, error) {
	opts := polyglot.AnalyzeQueryOptions{Dialect: dialect}
	analysis, err := client.AnalyzeQuery(querySQL, opts)
	if err != nil {
		return nil, fmt.Errorf("%w: analyze failed: %v", ErrInternal, err)
	}
	names := make([]string, 0, len(analysis.BaseTables))
	for _, bt := range analysis.BaseTables {
		if bt.Name != "" {
			names = append(names, bt.Name)
		}
	}
	return names, nil
}

// parseFirstStatement parses sql and returns the first statement node.
func parseFirstStatement(client *polyglot.Client, sql, dialect string) (map[string]any, error) {
	// All analysis APIs enforce the same byte budget before native parsing.
	// The native parser independently enforces its complexity/depth limits.
	if err := guardInput(sql); err != nil {
		return nil, err
	}
	raw, err := client.Parse(sql, dialect)
	if err != nil {
		return nil, classifyParseError(err)
	}
	var stmts []any
	if err := sonic.Unmarshal(raw, &stmts); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	stmts = dropNils(stmts)
	if len(stmts) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one statement, got %d", ErrUnsupported, len(stmts))
	}
	stmt, ok := stmts[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected statement shape", ErrUnsupported)
	}
	return stmt, nil
}

// generateStatement renders a single statement node back to SQL text.
func generateStatement(client *polyglot.Client, node map[string]any, dialect string) (string, error) {
	gen, err := client.Generate(mustMarshal([]any{node}), dialect)
	if err != nil || len(gen) == 0 {
		return "", fmt.Errorf("%w: generate failed: %v", ErrInternal, err)
	}
	return gen[0], nil
}

// innerQuery returns the query body to analyze for a statement. A bare
// SELECT/UNION is its own body; CREATE VIEW / CREATE TABLE AS / INSERT ...
// SELECT are unwrapped to the query they wrap. nil means the statement has no
// query at all (e.g. CREATE TABLE (...) / DROP).
func innerQuery(stmt map[string]any) map[string]any {
	if queryBody(stmt) != nil {
		return stmt
	}
	// A wrapping statement holds its query under a single payload object
	// (e.g. create_view -> query, create_table -> as_select, insert -> query).
	for _, payload := range stmt {
		m, ok := payload.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range innerQueryKeys {
			if q, ok := m[key].(map[string]any); ok && queryBody(q) != nil {
				return q
			}
		}
		for _, v := range m {
			if q, ok := v.(map[string]any); ok && queryBody(q) != nil {
				return q
			}
		}
	}
	return nil
}

// metadataToSchema converts the table->columns metadata into the Polyglot
// validation schema used to expand wildcards and resolve columns. A name with
// dots is split so the last segment is the table and the rest is the schema
// (e.g. "hive.raw.orders" -> schema "hive.raw", table "orders").
func metadataToSchema(metadata map[string][]string) *polyglot.ValidationSchema {
	if len(metadata) == 0 {
		return nil
	}
	names := make([]string, 0, len(metadata))
	for name := range metadata {
		names = append(names, name)
	}
	sort.Strings(names)

	schema := &polyglot.ValidationSchema{Tables: make([]polyglot.SchemaTable, 0, len(names))}
	for _, full := range names {
		schemaPart, table := splitLastDot(full)
		cols := metadata[full]
		columns := make([]polyglot.SchemaColumn, 0, len(cols))
		for _, c := range cols {
			// Polyglot 0.8 parses every supplied type while building its
			// resolver schema; an empty type causes the native parser to panic.
			// Metadata has only names, so use a portable placeholder type. It
			// affects neither wildcard expansion nor column lineage.
			columns = append(columns, polyglot.SchemaColumn{Name: c, Type: "VARCHAR"})
		}
		schema.Tables = append(schema.Tables, polyglot.SchemaTable{
			Schema:  schemaPart,
			Name:    table,
			Columns: columns,
		})
	}
	return schema
}

func splitLastDot(name string) (schema, table string) {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

func sortedResult(tableCols map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(tableCols))
	for table, cols := range tableCols {
		list := make([]string, 0, len(cols))
		for c := range cols {
			list = append(list, c)
		}
		sort.Strings(list)
		out[table] = list
	}
	return out
}

func ensureSet(m map[string]map[string]struct{}, key string) map[string]struct{} {
	set, ok := m[key]
	if !ok {
		set = map[string]struct{}{}
		m[key] = set
	}
	return set
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
