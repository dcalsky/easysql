package easysql

import (
	"reflect"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// These tests answer a concrete question: is innerQuery (the unwrap step in
// LineageSourceColumns) actually necessary, or could the raw statement be handed
// straight to the native engine?
//
// They probe the two engine calls LineageSourceColumns depends on and show that
// the engine rejects the wrapping statements (CREATE VIEW / CREATE TABLE AS /
// INSERT ... SELECT) but accepts the query body innerQuery extracts. They also
// pin down innerQuery's "no query" guard for statements like CREATE TABLE (...)
// and DROP.

// engineProbe runs the two native-engine calls LineageSourceColumns relies on —
// AnalyzeQuery (used by sourceTables) and OpenLineageColumnLineage (used by
// aggregateColumns) — against an arbitrary SQL string and reports whether each
// one succeeded.
func engineProbe(t *testing.T, sql string) (analyzeOK, lineageOK bool) {
	t.Helper()

	_, aerr := testClient.AnalyzeQuery(sql, polyglot.AnalyzeQueryOptions{Dialect: "trino"})

	olOpts := polyglot.OpenLineageOptions{
		Dialect:          "trino",
		Producer:         lineageProducer,
		DatasetNamespace: lineageNamespace,
		OutputDataset:    &polyglot.OpenLineageDatasetID{Namespace: lineageNamespace, Name: "result"},
	}
	if schema := metadataToSchema(trinoMetadata); schema != nil {
		olOpts.Schema = schema
	}
	_, lerr := testClient.OpenLineageColumnLineage(sql, olOpts)

	return aerr == nil, lerr == nil
}

// unwrapToInnerSQL reproduces the prefix of LineageSourceColumns that innerQuery
// powers: parse the first statement, unwrap it to its query body, and render
// that body back to SQL. ok is false when innerQuery reports "no query"
// (e.g. CREATE TABLE (...) / DROP).
func unwrapToInnerSQL(t *testing.T, sql string) (innerSQL string, ok bool) {
	t.Helper()

	stmt, err := parseFirstStatement(testClient, sql, "trino")
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	inner := innerQuery(stmt)
	if inner == nil {
		return "", false
	}
	innerSQL, err = generateStatement(testClient, inner, "trino")
	if err != nil {
		t.Fatalf("generate inner of %q: %v", sql, err)
	}
	return innerSQL, true
}

// TestInnerQueryUnwrapIsRequiredByEngine proves the unwrap step is load-bearing
// rather than redundant: for every wrapping statement the native engine rejects
// the raw statement on at least one of the two calls LineageSourceColumns makes,
// yet accepts the body innerQuery extracts on both. Remove innerQuery and feed
// the raw statement to the engine and these cases would error.
func TestInnerQueryUnwrapIsRequiredByEngine(t *testing.T) {
	wrapping := []struct {
		name string
		sql  string
	}{
		{"create_view", `CREATE VIEW hive.x.v AS SELECT o.amount FROM hive.raw.orders o`},
		{"create_table_as_select", `CREATE TABLE hive.x.t AS SELECT o.amount FROM hive.raw.orders o`},
		{"insert_select", `INSERT INTO hive.x.t SELECT o.amount FROM hive.raw.orders o`},
	}

	for _, tc := range wrapping {
		t.Run(tc.name, func(t *testing.T) {
			// The raw statement is rejected by at least one engine call. If a
			// future engine accepts it on both, this guard fails and signals
			// that innerQuery may have become redundant for this shape.
			fullAnalyzeOK, fullLineageOK := engineProbe(t, tc.sql)
			t.Logf("raw statement: analyzeOK=%v lineageOK=%v", fullAnalyzeOK, fullLineageOK)
			if fullAnalyzeOK && fullLineageOK {
				t.Fatalf("engine accepted the raw %s on both calls — innerQuery unwrapping would be redundant here", tc.name)
			}

			// The unwrapped body is accepted by both engine calls.
			innerSQL, ok := unwrapToInnerSQL(t, tc.sql)
			if !ok {
				t.Fatalf("innerQuery returned nil for query-bearing %s", tc.name)
			}
			t.Logf("unwrapped body: %s", innerSQL)
			innerAnalyzeOK, innerLineageOK := engineProbe(t, innerSQL)
			if !innerAnalyzeOK || !innerLineageOK {
				t.Fatalf("engine rejected unwrapped body %q: analyzeOK=%v lineageOK=%v; want both true",
					innerSQL, innerAnalyzeOK, innerLineageOK)
			}
		})
	}
}

// TestInnerQueryNonQueryStatementsReturnNil pins down the other half of
// innerQuery's job: a statement with no query at all returns nil, which lets
// LineageSourceColumns short-circuit to an empty result instead of surfacing the
// engine errors these statements would otherwise trigger.
func TestInnerQueryNonQueryStatementsReturnNil(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"create_table_columns", `CREATE TABLE hive.x.t (id BIGINT, name VARCHAR)`},
		{"drop_table", `DROP TABLE hive.x.t`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sanity: the engine cannot analyze these directly, so the nil
			// guard is what keeps LineageSourceColumns from erroring.
			if analyzeOK, lineageOK := engineProbe(t, tc.sql); analyzeOK || lineageOK {
				t.Fatalf("engine unexpectedly accepted %q: analyzeOK=%v lineageOK=%v", tc.sql, analyzeOK, lineageOK)
			}

			stmt, err := parseFirstStatement(testClient, tc.sql, "trino")
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sql, err)
			}
			if inner := innerQuery(stmt); inner != nil {
				t.Fatalf("innerQuery(%q) = non-nil; want nil for a statement with no query", tc.sql)
			}

			got, err := LineageSourceColumns(tc.sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("LineageSourceColumns(%q): %v", tc.sql, err)
			}
			if len(got) != 0 {
				t.Fatalf("LineageSourceColumns(%q) = %v; want empty result", tc.sql, got)
			}
		})
	}
}

// innerQueryFound parses sql under dialect, runs innerQuery, and reports whether
// a query body was extracted. When one is, it is also re-rendered to SQL so the
// caller can see exactly what would be analyzed.
func innerQueryFound(t *testing.T, dialect, sql string) (innerSQL string, found bool) {
	t.Helper()
	stmt, err := parseFirstStatement(testClient, sql, dialect)
	if err != nil {
		t.Fatalf("parse [%s] %q: %v", dialect, sql, err)
	}
	inner := innerQuery(stmt)
	if inner == nil {
		return "", false
	}
	if queryBody(inner) == nil {
		t.Fatalf("innerQuery [%s] %q returned a non-query node", dialect, sql)
	}
	innerSQL, err = generateStatement(testClient, inner, dialect)
	if err != nil {
		t.Fatalf("generate inner [%s] %q: %v", dialect, sql, err)
	}
	return innerSQL, true
}

// TestInnerQueryUnwrapCoverage documents the full breadth of statements that
// innerQuery's generic unwrap handles today — not just the three named in the
// docs (CREATE VIEW / CREATE TABLE AS / INSERT ... SELECT) but also
// materialized views, CTE-bearing CTAS/INSERT, INSERT OVERWRITE, CACHE TABLE,
// EXPLAIN, etc., across trino/postgres/spark. Each wraps the same body
// `SELECT o.amount FROM hive.raw.orders o`, so end-to-end lineage must resolve
// to exactly that one source column.
func TestInnerQueryUnwrapCoverage(t *testing.T) {
	want := map[string][]string{"hive.raw.orders": {"amount"}}

	cases := []struct {
		name    string
		dialect string
		sql     string
	}{
		{"create_view", "trino", `CREATE VIEW hive.x.v AS SELECT o.amount FROM hive.raw.orders o`},
		{"create_view_with_column_list", "trino", `CREATE VIEW hive.x.v (a) AS SELECT o.amount FROM hive.raw.orders o`},
		{"create_materialized_view", "postgresql", `CREATE MATERIALIZED VIEW mv AS SELECT o.amount FROM hive.raw.orders o`},
		{"create_table_as_select", "trino", `CREATE TABLE hive.x.t AS SELECT o.amount FROM hive.raw.orders o`},
		{"create_table_as_with_cte", "trino", `CREATE TABLE hive.x.t AS WITH c AS (SELECT o.amount FROM hive.raw.orders o) SELECT amount FROM c`},
		{"insert_select", "trino", `INSERT INTO hive.x.t SELECT o.amount FROM hive.raw.orders o`},
		{"insert_select_with_cte", "trino", `INSERT INTO hive.x.t WITH c AS (SELECT o.amount FROM hive.raw.orders o) SELECT amount FROM c`},
		{"insert_select_returning", "postgresql", `INSERT INTO t SELECT o.amount FROM hive.raw.orders o RETURNING *`},
		{"insert_overwrite", "spark", `INSERT OVERWRITE TABLE t SELECT o.amount FROM hive.raw.orders o`},
		{"cache_table_as_select", "spark", `CACHE TABLE c AS SELECT o.amount FROM hive.raw.orders o`},
		{"explain_select", "trino", `EXPLAIN SELECT o.amount FROM hive.raw.orders o`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			innerSQL, found := innerQueryFound(t, tc.dialect, tc.sql)
			if !found {
				t.Fatalf("innerQuery returned nil; expected to unwrap %q", tc.sql)
			}
			t.Logf("unwrapped body: %s", innerSQL)

			got, err := LineageSourceColumns(tc.sql,
				WithLineageDialect(tc.dialect), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("LineageSourceColumns [%s] %q: %v", tc.dialect, tc.sql, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s lineage = %v; want %v", tc.name, got, want)
			}
		})
	}
}

// TestInnerQueryUnsupportedShapes pins down the statements innerQuery does NOT
// unwrap today, returning nil (and so making LineageSourceColumns yield an empty
// result rather than an engine error). Two groups:
//
//   - "correctly empty": statements with no source query that flows into a
//     result — INSERT ... VALUES, CREATE TABLE (...) / LIKE, and subqueries that
//     sit only in a filter position (DELETE ... WHERE IN, UPDATE ... SET = (..)).
//   - "known gaps": MERGE ... USING (SELECT) and UPDATE ... FROM (SELECT) embed
//     a source query that DOES flow into a write target, but it is nested too
//     deep for innerQuery's shallow scan, so lineage is silently empty. If
//     innerQuery is ever extended to cover these, move the case to
//     TestInnerQueryUnwrapCoverage.
func TestInnerQueryUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		sql     string
		gap     bool // true = a real gap (source flows into a write target)
	}{
		{"insert_values", "trino", `INSERT INTO hive.x.t VALUES (1, 'a')`, false},
		{"create_table_columns", "trino", `CREATE TABLE hive.x.t (id BIGINT, name VARCHAR)`, false},
		{"create_table_like", "trino", `CREATE TABLE hive.x.t (LIKE hive.raw.orders)`, false},
		{"delete_where_subquery", "trino", `DELETE FROM hive.x.t WHERE id IN (SELECT o.user_id FROM hive.raw.orders o)`, false},
		{"update_set_subquery", "trino", `UPDATE hive.x.t SET amount = (SELECT max(o.amount) FROM hive.raw.orders o) WHERE id = 1`, false},
		{"merge_using_select", "trino", `MERGE INTO hive.x.t USING (SELECT o.user_id, o.amount FROM hive.raw.orders o) s ON t.id = s.user_id WHEN MATCHED THEN UPDATE SET amount = s.amount`, true},
		{"update_from_select", "postgresql", `UPDATE t SET x = s.v FROM (SELECT o.user_id, o.amount AS v FROM hive.raw.orders o) s WHERE t.id = s.user_id`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, found := innerQueryFound(t, tc.dialect, tc.sql); found {
				t.Fatalf("innerQuery unexpectedly unwrapped %s; update the coverage tests", tc.name)
			}
			got, err := LineageSourceColumns(tc.sql,
				WithLineageDialect(tc.dialect), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("LineageSourceColumns [%s] %q: %v", tc.dialect, tc.sql, err)
			}
			if len(got) != 0 {
				t.Fatalf("%s: LineageSourceColumns = %v; want empty (current behavior)", tc.name, got)
			}
			if tc.gap {
				t.Logf("KNOWN GAP: %s reads hive.raw.orders but lineage is empty today", tc.name)
			}
		})
	}
}
