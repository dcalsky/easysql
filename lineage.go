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
//     an ambiguous unqualified column is attributed to every candidate source
//     table.
//   - Source tables are resolved to their root physical tables (CTEs,
//     subqueries and set operations are seen through), and every source table
//     is listed even if no column flows from it.
//
// Unlike the Python version there is no process pool or LRU cache here: that
// infrastructure existed only to work around CPython's GIL for a pure-Python
// parser. The Polyglot client does the heavy lifting in native code and is safe
// for concurrent use, so callers can call SourceTableColumns directly (and add
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

// setOpBranchKeys are the AST keys holding the operands of a set operation.
var setOpBranchKeys = []string{"left", "right", "this", "expression"}

// lineageOptions configures a SourceTableColumns call.
type lineageOptions struct {
	dialect   string
	producer  string
	namespace string
	metadata  map[string][]string
}

// LineageOption configures SourceTableColumns.
type LineageOption func(*lineageOptions)

// WithLineageDialect selects the SQL dialect used to parse and analyze the
// statement (e.g. "trino", "hive", "spark", "postgres", "mysql"). It defaults
// to "trino".
func WithLineageDialect(dialect string) LineageOption {
	return func(o *lineageOptions) { o.dialect = dialect }
}

// WithLineageProducer sets the OpenLineage `_producer` URI recorded on the
// emitted lineage events. It is pure provenance metadata that identifies the
// program producing the lineage; it does not affect the source-column result
// SourceTableColumns returns. It defaults to lineageProducer.
func WithLineageProducer(producer string) LineageOption {
	return func(o *lineageOptions) { o.producer = producer }
}

// WithLineageNamespace sets the OpenLineage dataset namespace used for the
// input/output datasets of the emitted lineage events. Like the producer it is
// pure provenance metadata and does not affect the source-column result
// SourceTableColumns returns. It defaults to lineageNamespace.
func WithLineageNamespace(namespace string) LineageOption {
	return func(o *lineageOptions) { o.namespace = namespace }
}

// WithLineageMetadata supplies table metadata used to expand wildcards
// (SELECT *, t.*) and to resolve ambiguous unqualified columns. Keys are
// fully-qualified table names ("catalog.schema.table" or "schema.table") and
// values are their column lists. It corresponds to the Python `metadata`
// argument backed by DummyMetaDataProvider.
func WithLineageMetadata(metadata map[string][]string) LineageOption {
	return func(o *lineageOptions) { o.metadata = metadata }
}

// SourceTableColumns returns, for each physical source table referenced by sql,
// the sorted list of source columns that flow into the statement's result. It
// works for any statement that contains a query (SELECT, UNION, CREATE VIEW,
// CREATE TABLE AS, INSERT ... SELECT, ...).
//
// It is the Go equivalent of the Python `get_source_table_columns`. The result
// keys are the fully-qualified table names as resolved by the analyzer (e.g.
// "hive.raw.orders"); every source table appears in the map even when no column
// flows from it. The caller owns the polyglot client's lifecycle.
func SourceTableColumns(client *polyglot.Client, sql string, opts ...LineageOption) (map[string][]string, error) {
	req, err := prepareLineage(client, sql, opts...)
	if err != nil {
		return nil, err
	}

	// Trace each output column back to its source columns. Set operations
	// (UNION / INTERSECT / EXCEPT) are split into their leaf SELECTs and merged,
	// because the OpenLineage engine rejects set operations directly.
	for _, leaf := range req.leaves {
		// leafSelects only ever splits set operations; a single leaf therefore
		// is the whole inner query, whose SQL we already rendered above. Reuse
		// it to avoid a redundant Generate round-trip on the common path.
		leafSQL := req.innerSQL
		if len(req.leaves) > 1 {
			leafSQL, err = generateStatement(client, leaf, req.cfg.dialect)
			if err != nil {
				return nil, err
			}
		}
		if err := aggregateColumns(client, leafSQL, req.cfg, req.schema, req.tableCols); err != nil {
			return nil, err
		}
	}

	return sortedResult(req.tableCols), nil
}

// lineageRequest carries everything the per-leaf analysis loop needs. It is
// produced once by prepareLineage and then consumed by both the serial
// (SourceTableColumns) and the concurrent (SourceTableColumnsConcurrent)
// drivers, so the two differ only in how they iterate the leaves — making the
// serial-vs-concurrent benchmark an apples-to-apples comparison.
type lineageRequest struct {
	cfg       lineageOptions
	schema    *polyglot.ValidationSchema
	innerSQL  string
	leaves    []map[string]any
	tableCols map[string]map[string]struct{}
}

// prepareLineage performs the dialect-agnostic setup shared by every lineage
// driver: parse the statement, unwrap it to its inner query, render that query
// back to SQL, build the validation schema, and seed every root physical source
// table with an empty column set (so a table that contributes no flowing column
// still appears in the result). The returned request's leaves are the leaf
// SELECTs to analyze; a statement with no query at all yields zero leaves and an
// empty (non-nil) result map.
func prepareLineage(client *polyglot.Client, sql string, opts ...LineageOption) (*lineageRequest, error) {
	if client == nil {
		return nil, errors.New("easysql: nil polyglot client")
	}

	cfg := lineageOptions{dialect: "trino", producer: lineageProducer, namespace: lineageNamespace}
	for _, o := range opts {
		o(&cfg)
	}
	if strings.TrimSpace(cfg.dialect) == "" {
		cfg.dialect = "trino"
	}
	if strings.TrimSpace(cfg.producer) == "" {
		cfg.producer = lineageProducer
	}
	if strings.TrimSpace(cfg.namespace) == "" {
		cfg.namespace = lineageNamespace
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
		// A statement with no query at all (e.g. CREATE TABLE (...) / DROP):
		// nothing to analyze, so there are no source tables or columns. The
		// drivers see zero leaves and return an empty result.
		return &lineageRequest{cfg: cfg, tableCols: map[string]map[string]struct{}{}}, nil
	}

	innerSQL, err := generateStatement(client, inner, cfg.dialect)
	if err != nil {
		return nil, err
	}

	schema := metadataToSchema(cfg.metadata)

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

	return &lineageRequest{
		cfg:       cfg,
		schema:    schema,
		innerSQL:  innerSQL,
		leaves:    leafSelects(inner),
		tableCols: tableCols,
	}, nil
}

// aggregateColumns runs OpenLineage column lineage for a single leaf SELECT and
// folds its resolved (and heuristically resolved) source columns into tableCols.
func aggregateColumns(
	client *polyglot.Client,
	leafSQL string,
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

	res, err := client.OpenLineageColumnLineage(leafSQL, olOpts)
	if err != nil {
		return fmt.Errorf("%w: column lineage failed: %v", ErrInternal, err)
	}

	// Fold each output field into tableCols in a single pass:
	//   - Resolved fields map to the source (table, column) pairs that produced
	//     them.
	//   - Unresolved fields are usually a bare column that is ambiguous across
	//     joined tables. sqllineage attributes such a column to every candidate
	//     source table, so do the same: treat the output field name as the
	//     column name and credit each source table that declares it in metadata.
	for name, field := range res.Facet.Fields {
		if len(field.InputFields) == 0 {
			for _, in := range res.Inputs {
				if in.Name != "" && containsString(cfg.metadata[in.Name], name) {
					ensureSet(tableCols, in.Name)[name] = struct{}{}
				}
			}
			continue
		}
		for _, in := range field.InputFields {
			if in.Name == "" || in.Field == "" {
				continue
			}
			ensureSet(tableCols, in.Name)[in.Field] = struct{}{}
		}
	}
	return nil
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
	raw, err := client.Parse(sql, dialect)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	var stmts []any
	if err := sonic.Unmarshal(raw, &stmts); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	stmts = dropNils(stmts)
	if len(stmts) == 0 {
		return nil, fmt.Errorf("%w: no statement to analyze", ErrUnsupported)
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

// leafSelects flattens a query body into its leaf SELECT nodes, descending
// through set operations (UNION / INTERSECT / EXCEPT). The OpenLineage engine
// rejects set operations directly, so each branch is analyzed on its own and the
// results are merged.
func leafSelects(node map[string]any) []map[string]any {
	for _, k := range setOpKeys {
		if so, ok := node[k].(map[string]any); ok {
			var leaves []map[string]any
			for _, side := range setOpBranchKeys {
				if branch, ok := so[side].(map[string]any); ok {
					leaves = append(leaves, leafSelects(branch)...)
				}
			}
			if len(leaves) > 0 {
				return leaves
			}
		}
	}
	return []map[string]any{node}
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
			columns = append(columns, polyglot.SchemaColumn{Name: c})
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
