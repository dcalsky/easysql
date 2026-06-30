package easysql

import (
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
