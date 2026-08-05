package easysql

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// trinoMetadata mirrors TRINO_METADATA in the Python test_sql_lineage.py: the
// column catalog used to expand wildcards and resolve ambiguous columns.
var trinoMetadata = map[string][]string{
	"hive.raw.users": {"user_id", "user_name", "email"},
	"hive.raw.orders": {
		"order_id",
		"user_id",
		"amount",
		"quantity",
		"order_ts",
		"order_date",
		"status",
	},
	"hive.raw.payments": {"order_id", "paid_amount", "paid_at"},
}

// assertLineageSourceColumns is the Go analogue of the Python helper of the same
// name: it runs the Trino lineage analysis and asserts the exact table->columns
// result (columns are compared order-insensitively, since the API sorts them).
func assertLineageSourceColumns(t *testing.T, name, sql string, expected map[string][]string) {
	t.Helper()
	actual, err := LineageSourceColumns(
		sql,
		WithLineageDialect("trino"),
		WithLineageMetadata(trinoMetadata),
	)
	if err != nil {
		t.Fatalf("%s: LineageSourceColumns: %v", name, err)
	}
	want := map[string][]string{}
	for table, cols := range expected {
		// Use a non-nil empty slice so reflect.DeepEqual matches the
		// implementation, which returns []string{} (never nil) for a source
		// table with no flowing columns.
		c := append([]string{}, cols...)
		sort.Strings(c)
		want[table] = c
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%s\nexpected: %v\nactual:   %v", name, want, actual)
	}
}

func TestLineageSourceColumnsRejectsMultipleStatements(t *testing.T) {
	_, err := LineageSourceColumns(
		`SELECT a FROM t; SELECT secret FROM restricted`,
		WithLineageDialect("trino"),
	)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("LineageSourceColumns error = %v, want ErrUnsupported", err)
	}
}

func TestTrinoCatalogSchemaTableWithWildcardAndWhere(t *testing.T) {
	assertLineageSourceColumns(t,
		"trino_catalog_schema_table_with_wildcard_and_where",
		`
        CREATE VIEW hive.analytics.v_user_orders AS
        SELECT u.user_name, o.*
        FROM hive.raw.users u
        JOIN hive.raw.orders o ON u.user_id = o.user_id
        WHERE o.status = 'PAID'
          AND u.email IS NOT NULL
        `,
		map[string][]string{
			"hive.raw.orders": {
				"amount",
				"order_date",
				"order_id",
				"order_ts",
				"quantity",
				"status",
				"user_id",
			},
			"hive.raw.users": {"user_name"},
		},
	)
}

func TestTrinoDuplicateColumnWithQualifiedSourcesAndWhere(t *testing.T) {
	assertLineageSourceColumns(t,
		"trino_duplicate_column_with_qualified_sources_and_where",
		`
        CREATE VIEW hive.analytics.v_user_ids AS
        SELECT
            u.user_id AS user_id_from_users,
            o.user_id AS user_id_from_orders
        FROM hive.raw.users u
        JOIN hive.raw.orders o ON u.user_id = o.user_id
        WHERE o.status IN ('PAID', 'SHIPPED')
          AND u.email LIKE '%@example.com'
        `,
		map[string][]string{
			"hive.raw.orders": {"user_id"},
			"hive.raw.users":  {"user_id"},
		},
	)
}

func TestTrinoAmbiguousBareDuplicateColumnWithWhere(t *testing.T) {
	assertLineageSourceColumns(t,
		"trino_ambiguous_bare_duplicate_column_with_where",
		`
        CREATE VIEW hive.analytics.v_ambiguous_user_id AS
        SELECT user_id
        FROM hive.raw.users u
        JOIN hive.raw.orders o ON u.user_id = o.user_id
        WHERE o.order_date >= DATE '2024-01-01'
        `,
		map[string][]string{
			"hive.raw.orders": {"user_id"},
			"hive.raw.users":  {"user_id"},
		},
	)
}

func TestTrinoExpressionAndFunctionColumnsWithWhere(t *testing.T) {
	assertLineageSourceColumns(t,
		"trino_expression_and_function_columns_with_where",
		`
        CREATE VIEW hive.analytics.v_order_metrics AS
        SELECT
            o.amount * o.quantity AS gross_amount,
            date_trunc('day', o.order_ts) AS order_day,
            u.user_name
        FROM hive.raw.users u
        JOIN hive.raw.orders o ON u.user_id = o.user_id
        WHERE o.status = 'PAID'
          AND o.order_date >= DATE '2024-01-01'
        `,
		map[string][]string{
			"hive.raw.orders": {"amount", "order_ts", "quantity"},
			"hive.raw.users":  {"user_name"},
		},
	)
}

func TestTrinoCteResolvesToRootSourceTablesWithWhere(t *testing.T) {
	assertLineageSourceColumns(t,
		"trino_cte_resolves_to_root_source_tables_with_where",
		`
        CREATE VIEW hive.analytics.v_paid_orders AS
        WITH paid_orders AS (
            SELECT
                o.order_id,
                o.user_id,
                p.paid_amount
            FROM hive.raw.orders o
            JOIN hive.raw.payments p ON o.order_id = p.order_id
            WHERE o.status = 'PAID'
              AND p.paid_at >= TIMESTAMP '2024-01-01 00:00:00'
        )
        SELECT
            po.order_id,
            po.user_id,
            po.paid_amount
        FROM paid_orders po
        WHERE po.paid_amount > 0
        `,
		map[string][]string{
			"hive.raw.orders":   {"order_id", "user_id"},
			"hive.raw.payments": {"paid_amount"},
		},
	)
}

// TestLineageAcrossStatementTypes asserts that lineage is analyzed for EVERY
// statement that contains a query — not just sink statements like CREATE VIEW.
// A bare SELECT, UNION, CTE, subquery, CREATE TABLE AS and INSERT ... SELECT all
// report their real source columns (with filter-only columns still excluded).
func TestLineageAcrossStatementTypes(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name: "plain_select_explicit_columns",
			sql:  `SELECT user_id, user_name FROM hive.raw.users`,
			expected: map[string][]string{
				"hive.raw.users": {"user_id", "user_name"},
			},
		},
		{
			name: "plain_select_wildcard_expands_via_metadata",
			sql:  `SELECT * FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {
					"amount", "order_date", "order_id", "order_ts",
					"quantity", "status", "user_id",
				},
			},
		},
		{
			name: "plain_select_where_only_column_excluded",
			sql:  `SELECT order_id FROM hive.raw.orders WHERE status = 'X'`,
			expected: map[string][]string{
				"hive.raw.orders": {"order_id"},
			},
		},
		{
			name: "plain_select_join_filter_only_side_is_empty",
			sql: `SELECT u.user_name
                  FROM hive.raw.users u
                  JOIN hive.raw.orders o ON u.user_id = o.user_id`,
			expected: map[string][]string{
				"hive.raw.orders": {},
				"hive.raw.users":  {"user_name"},
			},
		},
		{
			name: "union_merges_both_branches",
			sql: `SELECT user_id FROM hive.raw.users
                  UNION
                  SELECT user_id FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
				"hive.raw.users":  {"user_id"},
			},
		},
		{
			name: "plain_cte_resolves_to_root_table",
			sql: `WITH a AS (SELECT o.user_id, o.amount FROM hive.raw.orders o),
                       b AS (SELECT a.user_id FROM a)
                  SELECT b.user_id FROM b`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
			},
		},
		{
			name: "plain_subquery_resolves_to_root_table",
			sql:  `SELECT x.uid FROM (SELECT o.user_id AS uid FROM hive.raw.orders o) x`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
			},
		},
		{
			name: "plain_select_window_partition_and_order_flow",
			sql: `SELECT row_number() OVER (PARTITION BY o.user_id ORDER BY o.order_ts) AS rn
                  FROM hive.raw.orders o`,
			expected: map[string][]string{
				"hive.raw.orders": {"order_ts", "user_id"},
			},
		},
		{
			name: "create_table_as_select_with_where_excluded",
			sql: `CREATE TABLE hive.x.t AS
                  SELECT o.amount, o.user_id FROM hive.raw.orders o WHERE o.status = 'X'`,
			expected: map[string][]string{
				"hive.raw.orders": {"amount", "user_id"},
			},
		},
		{
			name: "insert_into_select_with_where_excluded",
			sql:  `INSERT INTO hive.x.t SELECT o.amount FROM hive.raw.orders o WHERE o.status = 'X'`,
			expected: map[string][]string{
				"hive.raw.orders": {"amount"},
			},
		},
		{
			name: "create_view_over_union",
			sql: `CREATE VIEW hive.x.v AS
                  SELECT u.user_id FROM hive.raw.users u
                  UNION
                  SELECT o.user_id FROM hive.raw.orders o`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
				"hive.raw.users":  {"user_id"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertLineageSourceColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// TestLineageSetOperationValueAndFilterSemantics checks the full-query
// OpenLineage behavior added in Polyglot 0.8. UNION branches both contribute
// values, while INTERSECT/EXCEPT right branches are membership filters only.
func TestLineageSetOperationValueAndFilterSemantics(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name: "union_values_from_both_branches",
			sql: `SELECT user_id FROM hive.raw.users
				UNION
				SELECT user_id FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
				"hive.raw.users":  {"user_id"},
			},
		},
		{
			name: "union_all_values_from_both_branches",
			sql: `SELECT user_id FROM hive.raw.users
				UNION ALL
				SELECT user_id FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {"user_id"},
				"hive.raw.users":  {"user_id"},
			},
		},
		{
			name: "except_right_branch_is_filter_only",
			sql: `SELECT user_id FROM hive.raw.users
				EXCEPT
				SELECT user_id FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {},
				"hive.raw.users":  {"user_id"},
			},
		},
		{
			name: "intersect_right_branch_is_filter_only",
			sql: `SELECT user_id FROM hive.raw.users
				INTERSECT
				SELECT user_id FROM hive.raw.orders`,
			expected: map[string][]string{
				"hive.raw.orders": {},
				"hive.raw.users":  {"user_id"},
			},
		},
		{
			// The one native input has both DIRECT and INDIRECT/FILTER
			// transformations. It must still be included as a value source.
			name: "shared_source_with_direct_and_filter_transformations_flows",
			sql: `SELECT user_id FROM hive.raw.users
				EXCEPT
				SELECT user_id FROM hive.raw.users`,
			expected: map[string][]string{
				"hive.raw.users": {"user_id"},
			},
		},
		{
			name: "parenthesized_nested_union_keeps_every_value_branch",
			sql: `SELECT user_id FROM hive.raw.users
				UNION
				(SELECT user_id FROM hive.raw.orders
				 UNION ALL
				 SELECT order_id FROM hive.raw.payments)`,
			expected: map[string][]string{
				"hive.raw.orders":   {"user_id"},
				"hive.raw.payments": {"order_id"},
				"hive.raw.users":    {"user_id"},
			},
		},
		{
			name: "root_cte_referenced_by_each_union_branch",
			sql: `WITH candidates AS (SELECT user_id FROM hive.raw.users)
				SELECT user_id FROM candidates
				UNION ALL
				SELECT user_id FROM candidates`,
			expected: map[string][]string{
				"hive.raw.users": {"user_id"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertLineageSourceColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// TestLineagePlainSelectMatchesEquivalentCreateView is a focused regression for
// the requirement that a bare SELECT is analyzed just like its CREATE VIEW
// wrapper: both must yield the same source columns.
func TestLineagePlainSelectMatchesEquivalentCreateView(t *testing.T) {
	const body = `SELECT u.user_name, o.amount
                  FROM hive.raw.users u
                  JOIN hive.raw.orders o ON u.user_id = o.user_id
                  WHERE o.status = 'PAID'`

	plain, err := LineageSourceColumns(body,
		WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
	if err != nil {
		t.Fatalf("plain select: %v", err)
	}
	view, err := LineageSourceColumns("CREATE VIEW hive.x.v AS "+body,
		WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
	if err != nil {
		t.Fatalf("create view: %v", err)
	}
	if !reflect.DeepEqual(plain, view) {
		t.Fatalf("plain SELECT and its CREATE VIEW disagree:\n plain: %v\n view:  %v", plain, view)
	}
	want := map[string][]string{
		"hive.raw.orders": {"amount"},
		"hive.raw.users":  {"user_name"},
	}
	if !reflect.DeepEqual(plain, want) {
		t.Fatalf("unexpected lineage:\n got:  %v\n want: %v", plain, want)
	}
}

func TestLineageSimpleSelectFromFoo(t *testing.T) {
	metadata := map[string][]string{
		"foo": {"a", "b"},
		"boo": {"b"},
	}
	actual, err := LineageSourceColumns(
		"SELECT a, b FROM foo",
		WithLineageDialect("trino"),
		WithLineageMetadata(metadata),
	)
	if err != nil {
		t.Fatalf("LineageSourceColumns: %v", err)
	}
	want := map[string][]string{
		"foo": {"a", "b"},
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("expected: %v\nactual:   %v", want, actual)
	}
}

// assertLineageWithMetadata runs lineage with a custom metadata catalog.
func assertLineageWithMetadata(t *testing.T, name, sql string, metadata map[string][]string, expected map[string][]string) {
	t.Helper()
	actual, err := LineageSourceColumns(
		sql,
		WithLineageDialect("trino"),
		WithLineageMetadata(metadata),
	)
	if err != nil {
		t.Fatalf("%s: LineageSourceColumns: %v", name, err)
	}
	want := map[string][]string{}
	for table, cols := range expected {
		c := append([]string{}, cols...)
		sort.Strings(c)
		want[table] = c
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%s\nexpected: %v\nactual:   %v", name, want, actual)
	}
}

// TestLineageEmptySourceMetadataIgnoresUnrelatedCatalogTables asserts that
// metadata entries for tables not referenced by the query must not appear in the
// lineage result, even when the queried table has an empty column list.
func TestLineageEmptySourceMetadataIgnoresUnrelatedCatalogTables(t *testing.T) {
	metadata := map[string][]string{
		"foo":         {},
		"other_table": {"a", "b", "c"},
	}
	assertLineageWithMetadata(t,
		"empty_source_metadata_ignores_unrelated_catalog",
		"SELECT a, b FROM foo",
		metadata,
		map[string][]string{
			"foo": {"a", "b"},
		},
	)
}

// TestLineageEmptySourceMetadataCreditsOnlyQueriedTable is a focused regression
// for the production case where a wide catalog is supplied but only one table is
// read and its schema is unknown (empty metadata).
func TestLineageEmptySourceMetadataCreditsOnlyQueriedTable(t *testing.T) {
	metadata := map[string][]string{
		"vdm_rda.launch_to_engage.event":            {},
		"vdm_rda.launch_to_engage.event_attendance": {"actual_end_time", "brand", "bu"},
		"vdm_rda_launch_to_engage.event1":           {"brand", "actual_a_hcp_count", "event_nm"},
	}
	assertLineageWithMetadata(t,
		"empty_event_metadata_credits_only_queried_table",
		`SELECT brand, actual_end_time, actual_a_hcp_count, event_osmp_cd
		 FROM vdm_rda.launch_to_engage.event`,
		metadata,
		map[string][]string{
			"vdm_rda.launch_to_engage.event": {
				"actual_a_hcp_count",
				"actual_end_time",
				"brand",
				"event_osmp_cd",
			},
		},
	)
}

// TestLineageResultKeysAreQuerySourceTablesOnly is a structural invariant: every
// table key in the lineage result must be a base table the query actually reads.
func TestLineageResultKeysAreQuerySourceTablesOnly(t *testing.T) {
	metadata := map[string][]string{
		"hive.raw.users":    {"user_id", "user_name"},
		"hive.raw.orders":   {"order_id", "user_id", "amount"},
		"hive.raw.payments": {"order_id", "paid_amount"},
	}
	sql := `SELECT user_id, amount FROM hive.raw.orders`
	got, err := LineageSourceColumns(sql,
		WithLineageDialect("trino"), WithLineageMetadata(metadata))
	if err != nil {
		t.Fatalf("LineageSourceColumns: %v", err)
	}
	for table := range got {
		if table != "hive.raw.orders" {
			t.Fatalf("unexpected table %q in lineage; want only hive.raw.orders (got %v)", table, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Audit probes for LineageSourceColumns and LineageSourceColumnsConcurrent.
//
// Documented semantics relied on (README "LineageSourceColumns" + doc comments
// in lineage.go):
//
//   d1. Only source columns whose VALUES flow into the result are reported;
//       filter-only columns (WHERE / JOIN ON / GROUP BY / ORDER BY / HAVING)
//       are excluded. Semantics align with sqllineage's
//       get_source_table_columns.
//   d2. Every root physical source table appears in the result, even with no
//       flowing columns.
//   d3. Metadata keys are fully-qualified table names; catalog entries for
//       tables NOT referenced by the query are ignored and cannot influence
//       the result.
//   d4. Works on any statement that contains a query — bare SELECT/UNION,
//       CREATE VIEW, CTAS, INSERT ... SELECT.
//   d5. WithLineageProducer / WithLineageNamespace are pure provenance
//       metadata and do not affect the result.
//   d6. LineageSourceColumnsConcurrent is a drop-in: options, semantics and
//       result are identical to the serial version.
// ---------------------------------------------------------------------------

// bughuntBoth runs the same call through the serial and the concurrent driver,
// asserts they agree (d6), and returns the shared result.
func bughuntBoth(t *testing.T, sql string, opts ...LineageOption) (map[string][]string, error) {
	t.Helper()
	serial, serr := LineageSourceColumns(sql, opts...)
	concurrent, cerr := LineageSourceColumnsConcurrent(sql, opts...)
	if (serr == nil) != (cerr == nil) {
		t.Fatalf("serial/concurrent error mismatch:\n serial err:     %v\n concurrent err: %v", serr, cerr)
	}
	if serr == nil && !reflect.DeepEqual(serial, concurrent) {
		t.Fatalf("concurrent != serial (doc: drop-in alternative)\n serial:     %v\n concurrent: %v", serial, concurrent)
	}
	return serial, serr
}

func bughuntWant(expected map[string][]string) map[string][]string {
	want := map[string][]string{}
	for table, cols := range expected {
		c := append([]string{}, cols...)
		sort.Strings(c)
		want[table] = c
	}
	return want
}

// TestBughuntFilterOnlyAndFlowSemantics pins d1/d2 corner cases.
func TestBughuntFilterOnlyAndFlowSemantics(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			// top-level ORDER BY is filter-position: excluded (d1).
			"order_by_only_column_excluded",
			`SELECT order_id FROM hive.raw.orders ORDER BY order_ts`,
			map[string][]string{"hive.raw.orders": {"order_id"}},
		},
		{
			// HAVING is filter-position: max(amount) excluded; the projected
			// group key and the projected aggregate's argument flow (d1).
			"having_only_column_excluded",
			`SELECT o.user_id, count(o.order_id) AS c FROM hive.raw.orders o
			 GROUP BY o.user_id HAVING max(o.amount) > 10`,
			map[string][]string{"hive.raw.orders": {"order_id", "user_id"}},
		},
		{
			// A column used in BOTH projection and WHERE still flows (d1).
			"column_in_projection_and_filter_flows",
			`SELECT status FROM hive.raw.orders WHERE status = 'X'`,
			map[string][]string{"hive.raw.orders": {"status"}},
		},
		{
			// CASE: condition and branch columns all feed the output value.
			"case_condition_and_branches_flow",
			`SELECT CASE WHEN status = 'X' THEN amount ELSE quantity END AS v FROM hive.raw.orders`,
			map[string][]string{"hive.raw.orders": {"amount", "quantity", "status"}},
		},
		{
			// Aggregate argument flows; GROUP BY-only expression excluded (d1).
			"aggregate_argument_flows",
			`SELECT sum(o.amount) AS total FROM hive.raw.orders o GROUP BY o.status`,
			map[string][]string{"hive.raw.orders": {"amount"}},
		},
		{
			// Window PARTITION BY / ORDER BY feed the window value: they flow.
			"window_partition_and_order_flow",
			`SELECT max(o.amount) OVER (ORDER BY o.order_ts) AS m FROM hive.raw.orders o`,
			map[string][]string{"hive.raw.orders": {"amount", "order_ts"}},
		},
		{
			// IN-subquery is filter-only: users contributes nothing but must
			// still appear as a source table (d1 + d2).
			"filter_only_subquery_table_listed_empty",
			`SELECT o.amount FROM hive.raw.orders o
			 WHERE o.user_id IN (SELECT u.user_id FROM hive.raw.users u)`,
			map[string][]string{"hive.raw.orders": {"amount"}, "hive.raw.users": {}},
		},
		{
			// Self-join collapses to one physical table; ON columns excluded.
			"self_join_single_physical_table",
			`SELECT a.user_name FROM hive.raw.users a JOIN hive.raw.users b ON a.user_id = b.user_id`,
			map[string][]string{"hive.raw.users": {"user_name"}},
		},
		{
			// CTE chain resolves to root tables; the join-only side of the CTE
			// is listed with no flowing columns (d2).
			"cte_chain_resolves_to_roots",
			`WITH a AS (SELECT o.user_id, o.amount FROM hive.raw.orders o WHERE o.status = 'X'),
			      b AS (SELECT a.user_id FROM a JOIN hive.raw.users u ON a.user_id = u.user_id)
			 SELECT b.user_id FROM b`,
			map[string][]string{"hive.raw.orders": {"user_id"}, "hive.raw.users": {}},
		},
		{
			// Nested subquery resolves through to the root table.
			"nested_subqueries_resolve",
			`SELECT y.uid FROM (SELECT x.uid FROM (SELECT o.user_id AS uid FROM hive.raw.orders o) x) y`,
			map[string][]string{"hive.raw.orders": {"user_id"}},
		},
		{
			// UNION inside a CTE splits and merges correctly.
			"union_inside_cte",
			`WITH c AS (SELECT user_id FROM hive.raw.users UNION SELECT user_id FROM hive.raw.orders)
			 SELECT user_id FROM c`,
			map[string][]string{"hive.raw.orders": {"user_id"}, "hive.raw.users": {"user_id"}},
		},
		{
			// Left-deep three-way UNION (no parens) merges all branches.
			"three_way_union_flat",
			`SELECT user_id FROM hive.raw.users
			 UNION SELECT user_id FROM hive.raw.orders
			 UNION SELECT order_id FROM hive.raw.payments`,
			map[string][]string{
				"hive.raw.orders":   {"user_id"},
				"hive.raw.payments": {"order_id"},
				"hive.raw.users":    {"user_id"},
			},
		},
		{
			// EXCEPT's right branch only filters left values: it remains in the
			// result as a physical source table but contributes no value column.
			"except_right_branch_is_filter_only",
			`SELECT user_id FROM hive.raw.users EXCEPT SELECT user_id FROM hive.raw.orders`,
			map[string][]string{"hive.raw.orders": {}, "hive.raw.users": {"user_id"}},
		},
		{
			// Qualified wildcard expands only its own table via metadata; the
			// other join side is listed empty (d2, d3).
			"qualified_star_expands_own_table_only",
			`SELECT u.* FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id`,
			map[string][]string{
				"hive.raw.orders": {},
				"hive.raw.users":  {"email", "user_id", "user_name"},
			},
		},
		{
			// Ambiguous unqualified column with metadata: attributed to every
			// in-scope table whose metadata declares it — here only users.
			"ambiguous_bare_column_metadata_resolves_owner",
			`SELECT email FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id`,
			map[string][]string{"hive.raw.orders": {}, "hive.raw.users": {"email"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bughuntBoth(t, tc.sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("LineageSourceColumns: %v", err)
			}
			if want := bughuntWant(tc.expected); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s\n sql:  %s\n want: %v\n got:  %v", tc.name, tc.sql, want, got)
			}
		})
	}
}

// TestBughuntStarWithoutMetadata pins d2/d3 for SELECT * when the queried
// table's schema is unknown: the wildcard cannot be expanded, so the table is
// listed with no flowing columns — and unrelated catalog entries must not
// change that.
func TestBughuntStarWithoutMetadata(t *testing.T) {
	noMD, err := bughuntBoth(t, `SELECT * FROM hive.raw.orders`, WithLineageDialect("trino"))
	if err != nil {
		t.Fatalf("no metadata: %v", err)
	}
	unrelatedMD, err := bughuntBoth(t, `SELECT * FROM hive.raw.orders`,
		WithLineageDialect("trino"),
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"user_id", "email"}}))
	if err != nil {
		t.Fatalf("unrelated metadata: %v", err)
	}
	want := map[string][]string{"hive.raw.orders": {}}
	if !reflect.DeepEqual(noMD, want) {
		t.Fatalf("SELECT * without metadata:\n want: %v\n got:  %v", want, noMD)
	}
	if !reflect.DeepEqual(unrelatedMD, noMD) {
		t.Fatalf("unrelated catalog entry changed the result (doc: ignored):\n without: %v\n with:    %v", noMD, unrelatedMD)
	}
}

// TestBughuntWrappingStatementsMatchBareSelect pins d4: CREATE VIEW / CTAS /
// INSERT ... SELECT are unwrapped and must match the bare SELECT.
func TestBughuntWrappingStatementsMatchBareSelect(t *testing.T) {
	const body = `SELECT u.user_name, o.amount
	              FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id
	              WHERE o.status = 'PAID'`
	opts := []LineageOption{WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata)}

	base, err := bughuntBoth(t, body, opts...)
	if err != nil {
		t.Fatalf("bare select: %v", err)
	}
	want := bughuntWant(map[string][]string{
		"hive.raw.orders": {"amount"},
		"hive.raw.users":  {"user_name"},
	})
	if !reflect.DeepEqual(base, want) {
		t.Fatalf("bare select:\n want: %v\n got:  %v", want, base)
	}
	for name, sql := range map[string]string{
		"create_view":   "CREATE VIEW hive.x.v AS " + body,
		"ctas":          "CREATE TABLE hive.x.t AS " + body,
		"insert_select": "INSERT INTO hive.x.t " + body,
	} {
		got, err := bughuntBoth(t, sql, opts...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("%s disagrees with bare select:\n bare: %v\n got:  %v", name, base, got)
		}
	}
}

// TestBughuntProducerNamespaceDoNotAffectResult pins d5, including a namespace
// deliberately chosen to collide with table-name fragments ("hive.raw",
// "orders") to try to confuse dataset-name matching.
func TestBughuntProducerNamespaceDoNotAffectResult(t *testing.T) {
	const sql = `SELECT u.user_name, o.amount
	             FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id`
	base, err := bughuntBoth(t, sql, WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	for _, extra := range [][]LineageOption{
		{WithLineageProducer("urn:bughunt:producer")},
		{WithLineageNamespace("hive.raw")},
		{WithLineageNamespace("orders")},
		{WithLineageProducer("x"), WithLineageNamespace("hive.raw.users")},
		{WithLineageProducer("   "), WithLineageNamespace("   ")}, // blank -> defaults
	} {
		opts := append([]LineageOption{WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata)}, extra...)
		got, err := bughuntBoth(t, sql, opts...)
		if err != nil {
			t.Fatalf("with %d extra opts: %v", len(extra), err)
		}
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("producer/namespace changed the result (doc: provenance only):\n base: %v\n got:  %v", base, got)
		}
	}
}

// TestBughuntDeterminism re-runs a query whose lineage depends on the
// unresolved-column heuristic (ambiguous bare column, no metadata) several
// times through both drivers: the result must be identical on every run.
func TestBughuntDeterminism(t *testing.T) {
	const sql = `SELECT email FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id`
	first, err := bughuntBoth(t, sql, WithLineageDialect("trino"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	for i := 0; i < 4; i++ {
		got, err := bughuntBoth(t, sql, WithLineageDialect("trino"))
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("nondeterministic result:\n run 0: %v\n run %d: %v", first, i, got)
		}
	}
}

// TestBughuntPhantomOutputNamesCreditedAsSourceColumns: output fields with NO
// source column at all — count(*), literal projections, unqualified aggregate
// args the engine fails to resolve, scalar subquery aliases — must not credit
// the OUTPUT name (alias like "c"/"one", or synthetic "_0") to source tables.
// Only real source columns that flow into the result are reported (d1).
func TestBughuntPhantomOutputNamesCreditedAsSourceColumns(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			"count_star_alias",
			`SELECT count(*) AS c FROM hive.raw.orders`,
			map[string][]string{"hive.raw.orders": {}},
		},
		{
			"literal_projection",
			`SELECT 1 AS one, 'a' AS lit FROM hive.raw.orders`,
			map[string][]string{"hive.raw.orders": {}},
		},
		{
			// the real source column `amount` is recovered and no synthetic
			// name is fabricated.
			"unqualified_sum_synthetic_name",
			`SELECT sum(amount) FROM hive.raw.orders GROUP BY status`,
			map[string][]string{"hive.raw.orders": {"amount"}},
		},
		{
			// each table is credited only with its own real source column.
			"correlated_scalar_subquery_alias",
			`SELECT o.order_id,
			        (SELECT max(p.paid_amount) FROM hive.raw.payments p WHERE p.order_id = o.order_id) AS mp
			 FROM hive.raw.orders o`,
			map[string][]string{
				"hive.raw.orders":   {"order_id"},
				"hive.raw.payments": {"paid_amount"},
			},
		},
		{
			// the literal branch's alias must not be credited to orders.
			"literal_union_branch",
			`SELECT 1 AS x UNION SELECT o.amount FROM hive.raw.orders o`,
			map[string][]string{"hive.raw.orders": {"amount"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bughuntBoth(t, tc.sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("LineageSourceColumns: %v", err)
			}
			if want := bughuntWant(tc.expected); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s\n sql:  %s\n want: %v\n got:  %v", tc.name, tc.sql, want, got)
			}
		})
	}
}

// TestBughuntUnrelatedMetadataSuffixLeak: metadata key matching must respect
// dot boundaries, so a query table `raw.orders` does NOT match the catalog key
// `hive.braw.orders` (a DIFFERENT table: schema "braw" != "raw"). Metadata for
// a table not referenced by the query is ignored (d3), so the result must equal
// the control run with a plainly unrelated catalog.
func TestBughuntUnrelatedMetadataSuffixLeak(t *testing.T) {
	const sql = `SELECT amount FROM raw.orders o JOIN raw.users u ON o.id = u.id`

	control, err := bughuntBoth(t, sql,
		WithLineageDialect("trino"),
		WithLineageMetadata(map[string][]string{"hive.zzz.unrelated": {"amount"}}))
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	leaked, err := bughuntBoth(t, sql,
		WithLineageDialect("trino"),
		WithLineageMetadata(map[string][]string{"hive.braw.orders": {"amount", "id"}}))
	if err != nil {
		t.Fatalf("leak probe: %v", err)
	}
	t.Logf("control (unrelated key ignored): %v", control)
	t.Logf("leaked  (hive.braw.orders):      %v", leaked)
	if !reflect.DeepEqual(leaked, control) {
		t.Fatalf("metadata for unreferenced table hive.braw.orders influenced the result:\n control: %v\n leaked:  %v", control, leaked)
	}
}

// TestBughuntParenthesizedSetOperationBranchFails: a valid UNION with
// parenthesized branches is a statement that contains a query (d4) and must
// produce the same merged result as its flat equivalent, not an error.
func TestBughuntParenthesizedSetOperationBranchFails(t *testing.T) {
	want := bughuntWant(map[string][]string{
		"hive.raw.orders":   {"user_id"},
		"hive.raw.payments": {"order_id"},
		"hive.raw.users":    {"user_id"},
	})

	// Sanity: the flat equivalent works and yields the expected merge.
	flat, err := bughuntBoth(t,
		`SELECT user_id FROM hive.raw.users
		 UNION SELECT user_id FROM hive.raw.orders
		 UNION SELECT order_id FROM hive.raw.payments`,
		WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
	if err != nil {
		t.Fatalf("flat union: %v", err)
	}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("flat union baseline:\n want: %v\n got:  %v", want, flat)
	}

	for name, sql := range map[string]string{
		"right_parenthesized": `SELECT user_id FROM hive.raw.users UNION (SELECT user_id FROM hive.raw.orders UNION SELECT order_id FROM hive.raw.payments)`,
		"left_parenthesized":  `(SELECT user_id FROM hive.raw.users UNION SELECT user_id FROM hive.raw.orders) UNION SELECT order_id FROM hive.raw.payments`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := bughuntBoth(t, sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			t.Logf("actual: result=%v err=%v", got, err)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s\n want: %v\n got:  %v", name, want, got)
			}
		})
	}
}
