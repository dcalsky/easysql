package easysql

import (
	"reflect"
	"sort"
	"testing"
)

// refColsMetadata is the catalog used by ReferencedColumns tests that exercise
// metadata-driven resolution (unqualified columns, star expansion).
var refColsMetadata = map[string][]string{
	"hive.raw.users":  {"id", "name", "email"},
	"hive.raw.orders": {"oid", "uid", "amt", "status"},
	"c.s.other":       {"o1", "o2", "o3"},
}

func assertReferencedColumns(t *testing.T, name, sql string, expected map[string][]string, opts ...LineageOption) {
	t.Helper()
	opts = append([]LineageOption{WithLineageDialect("trino")}, opts...)
	actual, err := ReferencedColumns(sql, opts...)
	if err != nil {
		t.Fatalf("%s: ReferencedColumns: %v", name, err)
	}
	want := map[string][]string{}
	for tbl, cols := range expected {
		c := append([]string{}, cols...)
		sort.Strings(c)
		want[tbl] = c
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%s\nexpected: %v\nactual:   %v", name, want, actual)
	}
}

// --------------------------------------------------------------------------- //
// Core semantics: filter-position columns are INCLUDED (the whole point).
// --------------------------------------------------------------------------- //

func TestReferencedColumnsIncludesFilterColumns(t *testing.T) {
	assertReferencedColumns(t, "where_only_column_included",
		`SELECT a FROM t WHERE b > 1`,
		map[string][]string{"t": {"a", "b"}},
	)
}

// TestReferencedColumnsIsSupersetOfLineage pins the documented relationship:
// ReferencedColumns adds the filter-only columns that LineageSourceColumns omits.
func TestReferencedColumnsIsSupersetOfLineage(t *testing.T) {
	sql := `SELECT order_id FROM hive.raw.orders WHERE status = 'X'`

	lineage, err := LineageSourceColumns(sql, WithLineageDialect("trino"))
	if err != nil {
		t.Fatalf("LineageSourceColumns: %v", err)
	}
	if !reflect.DeepEqual(lineage, map[string][]string{"hive.raw.orders": {"order_id"}}) {
		t.Fatalf("lineage baseline changed: %v", lineage)
	}

	assertReferencedColumns(t, "superset_over_lineage",
		sql,
		map[string][]string{"hive.raw.orders": {"order_id", "status"}},
	)
}

func TestReferencedColumnsAllFilterPositions(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "where",
			sql:      `SELECT a FROM t WHERE b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "group_by",
			sql:      `SELECT a, count(*) FROM t GROUP BY a, g`,
			expected: map[string][]string{"t": {"a", "g"}},
		},
		{
			name:     "having",
			sql:      `SELECT a FROM t GROUP BY a HAVING sum(h) > 1`,
			expected: map[string][]string{"t": {"a", "h"}},
		},
		{
			name:     "order_by",
			sql:      `SELECT a FROM t ORDER BY o`,
			expected: map[string][]string{"t": {"a", "o"}},
		},
		{
			name:     "window_partition_and_order",
			sql:      `SELECT a, row_number() OVER (PARTITION BY b ORDER BY c) rn FROM t`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// QUALIFY is not parsed by the Trino dialect, so this clause is covered under a
// dialect that supports it (Snowflake). Resolution is dialect-agnostic.
func TestReferencedColumnsQualifyClause(t *testing.T) {
	assertReferencedColumns(t, "qualify",
		`SELECT a FROM t QUALIFY row_number() OVER (PARTITION BY p ORDER BY q) = 1`,
		map[string][]string{"t": {"a", "p", "q"}},
		WithLineageDialect("snowflake"),
	)
}

// --------------------------------------------------------------------------- //
// JOINs: ON columns are included; resolution by alias and by metadata.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsJoinQualified(t *testing.T) {
	assertReferencedColumns(t, "join_qualified",
		`SELECT u.name
         FROM hive.raw.users u
         JOIN hive.raw.orders o ON u.id = o.uid
         WHERE o.status = 'X'
         GROUP BY u.name`,
		map[string][]string{
			"hive.raw.users":  {"id", "name"},
			"hive.raw.orders": {"status", "uid"},
		},
		WithLineageMetadata(refColsMetadata),
	)
}

func TestReferencedColumnsJoinUnqualifiedResolvedByMetadata(t *testing.T) {
	assertReferencedColumns(t, "join_unqualified_metadata",
		`SELECT name
         FROM hive.raw.users
         JOIN hive.raw.orders ON id = uid
         WHERE status = 'X'`,
		map[string][]string{
			"hive.raw.users":  {"id", "name"},
			"hive.raw.orders": {"status", "uid"},
		},
		WithLineageMetadata(refColsMetadata),
	)
}

func TestReferencedColumnsJoinUnqualifiedNoMetadataIsSuperset(t *testing.T) {
	// With no metadata an unqualified column cannot be disambiguated across the
	// two tables, so it is attributed to both (a safe superset).
	assertReferencedColumns(t, "join_unqualified_no_metadata",
		`SELECT name FROM a JOIN b ON a.k = b.k WHERE status = 'X'`,
		map[string][]string{
			"a": {"k", "name", "status"},
			"b": {"k", "name", "status"},
		},
	)
}

func TestReferencedColumnsSingleTableUnqualifiedNoMetadata(t *testing.T) {
	assertReferencedColumns(t, "single_table_unqualified",
		`SELECT a, b FROM t WHERE c > 1 GROUP BY a, b`,
		map[string][]string{"t": {"a", "b", "c"}},
	)
}

// --------------------------------------------------------------------------- //
// CTEs and derived subqueries are seen through to root physical tables.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsCTE(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "single_cte_with_filter",
			sql:      `WITH c AS (SELECT a, b FROM t) SELECT c.a FROM c WHERE c.b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "cte_internal_filter",
			sql:      `WITH c AS (SELECT a FROM t WHERE z > 1) SELECT a FROM c`,
			expected: map[string][]string{"t": {"a", "z"}},
		},
		{
			name:     "cte_column_aliases",
			sql:      `WITH c(x, y) AS (SELECT a, b FROM t) SELECT x FROM c WHERE y > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "multi_cte",
			sql:      `WITH x AS (SELECT a FROM t), y AS (SELECT b FROM r) SELECT x.a, y.b FROM x, y`,
			expected: map[string][]string{"t": {"a"}, "r": {"b"}},
		},
		{
			name:     "cte_referenced_twice",
			sql:      `WITH c AS (SELECT a, b FROM t) SELECT c1.a FROM c c1 JOIN c c2 ON c1.a = c2.b`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "nested_cte",
			sql:      `WITH a AS (SELECT x, y FROM t WHERE w > 0), b AS (SELECT x FROM a WHERE y > 0) SELECT x FROM b`,
			expected: map[string][]string{"t": {"w", "x", "y"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// --------------------------------------------------------------------------- //
// Subqueries: derived tables, scalar, IN/EXISTS/ANY, correlated.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsSubqueries(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "derived_table_in_from",
			sql:      `SELECT s.x FROM (SELECT a AS x, b FROM t WHERE c > 1) s WHERE s.x > 0`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
		},
		{
			name:     "scalar_subquery_in_select",
			sql:      `SELECT a, (SELECT max(x) FROM r) m FROM t`,
			expected: map[string][]string{"t": {"a"}, "r": {"x"}},
		},
		{
			name:     "in_subquery",
			sql:      `SELECT a FROM t WHERE b IN (SELECT k FROM r WHERE v > 1)`,
			expected: map[string][]string{"t": {"a", "b"}, "r": {"k", "v"}},
		},
		{
			name:     "not_in_subquery",
			sql:      `SELECT a FROM t WHERE b NOT IN (SELECT k FROM r)`,
			expected: map[string][]string{"t": {"a", "b"}, "r": {"k"}},
		},
		{
			name:     "exists_correlated",
			sql:      `SELECT a FROM t WHERE EXISTS (SELECT 1 FROM r WHERE r.k = t.a)`,
			expected: map[string][]string{"t": {"a"}, "r": {"k"}},
		},
		{
			name:     "scalar_correlated",
			sql:      `SELECT a FROM t WHERE b > (SELECT max(x) FROM r WHERE r.k = t.a)`,
			expected: map[string][]string{"t": {"a", "b"}, "r": {"k", "x"}},
		},
		{
			name:     "subquery_in_join",
			sql:      `SELECT s.x, o.amt FROM (SELECT a AS x FROM t WHERE c > 1) s JOIN o ON s.x = o.k`,
			expected: map[string][]string{"t": {"a", "c"}, "o": {"amt", "k"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// --------------------------------------------------------------------------- //
// Set operations: branches merged.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsSetOperations(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "union_all",
			sql:      `SELECT a FROM t WHERE b > 1 UNION ALL SELECT c FROM r WHERE d < 2`,
			expected: map[string][]string{"t": {"a", "b"}, "r": {"c", "d"}},
		},
		{
			name:     "intersect",
			sql:      `SELECT a FROM t WHERE x > 1 INTERSECT SELECT b FROM r WHERE y < 2`,
			expected: map[string][]string{"t": {"a", "x"}, "r": {"b", "y"}},
		},
		{
			name:     "except",
			sql:      `SELECT a FROM t EXCEPT SELECT b FROM r`,
			expected: map[string][]string{"t": {"a"}, "r": {"b"}},
		},
		{
			name:     "with_then_union",
			sql:      `WITH c AS (SELECT a, b FROM t) SELECT a FROM c WHERE b > 1 UNION ALL SELECT x FROM r WHERE y < 2`,
			expected: map[string][]string{"t": {"a", "b"}, "r": {"x", "y"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// --------------------------------------------------------------------------- //
// Wildcards.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsStar(t *testing.T) {
	t.Run("bare_star_no_metadata", func(t *testing.T) {
		assertReferencedColumns(t, "bare_star_no_metadata",
			`SELECT * FROM t WHERE id > 0`,
			map[string][]string{"t": {"*", "id"}},
		)
	})
	t.Run("bare_star_with_metadata", func(t *testing.T) {
		assertReferencedColumns(t, "bare_star_with_metadata",
			`SELECT * FROM hive.raw.users WHERE id > 0`,
			map[string][]string{"hive.raw.users": {"email", "id", "name"}},
			WithLineageMetadata(refColsMetadata),
		)
	})
	t.Run("qualified_star_with_metadata", func(t *testing.T) {
		assertReferencedColumns(t, "qualified_star_with_metadata",
			`SELECT u.* FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid`,
			map[string][]string{
				"hive.raw.users":  {"email", "id", "name"},
				"hive.raw.orders": {"uid"},
			},
			WithLineageMetadata(refColsMetadata),
		)
	})
	t.Run("bare_star_join_with_metadata", func(t *testing.T) {
		assertReferencedColumns(t, "bare_star_join_with_metadata",
			`SELECT * FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid`,
			map[string][]string{
				"hive.raw.users":  {"email", "id", "name"},
				"hive.raw.orders": {"amt", "oid", "status", "uid"},
			},
			WithLineageMetadata(refColsMetadata),
		)
	})
}

// --------------------------------------------------------------------------- //
// Qualified column reference forms.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsThreePartReference(t *testing.T) {
	assertReferencedColumns(t, "three_part",
		`SELECT hive.raw.users.id FROM hive.raw.users WHERE hive.raw.users.email IS NOT NULL`,
		map[string][]string{"hive.raw.users": {"email", "id"}},
		WithLineageMetadata(refColsMetadata),
	)
}

func TestReferencedColumnsAliasQualified(t *testing.T) {
	assertReferencedColumns(t, "alias_qualified",
		`SELECT o.amt FROM hive.raw.orders AS o WHERE o.status = 'X'`,
		map[string][]string{"hive.raw.orders": {"amt", "status"}},
		WithLineageMetadata(refColsMetadata),
	)
}

// --------------------------------------------------------------------------- //
// DDL wrappers are unwrapped to their inner query.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsDDLWrappers(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "create_view",
			sql:      `CREATE VIEW v AS SELECT a FROM t WHERE b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "create_table_as_select",
			sql:      `CREATE TABLE d AS SELECT a FROM t WHERE b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "insert_select",
			sql:      `INSERT INTO d SELECT a FROM t WHERE b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "create_view_with_cte_and_join",
			sql:      `CREATE VIEW v AS WITH c AS (SELECT a, b FROM t WHERE z > 0) SELECT c.a FROM c JOIN r ON c.b = r.k`,
			expected: map[string][]string{"t": {"a", "b", "z"}, "r": {"k"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// --------------------------------------------------------------------------- //
// Seeding and empty results.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsTableWithNoColumnsStillAppears(t *testing.T) {
	assertReferencedColumns(t, "select_constant",
		`SELECT 1 FROM t`,
		map[string][]string{"t": {}},
	)
}

func TestReferencedColumnsNonQueryStatementsAreEmpty(t *testing.T) {
	for _, sql := range []string{
		`DROP TABLE t`,
		`CREATE TABLE t (a INT, b INT)`,
	} {
		got, err := ReferencedColumns(sql, WithLineageDialect("trino"))
		if err != nil {
			t.Fatalf("ReferencedColumns(%q): %v", sql, err)
		}
		if len(got) != 0 {
			t.Fatalf("ReferencedColumns(%q) = %v; want empty", sql, got)
		}
	}
}

// --------------------------------------------------------------------------- //
// Expressions: columns inside functions / CASE / arithmetic / predicates.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsExpressions(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "case_expression",
			sql:      `SELECT CASE WHEN a > 1 THEN b ELSE c END FROM t`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
		},
		{
			name:     "function_args",
			sql:      `SELECT coalesce(a, b) FROM t`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "arithmetic",
			sql:      `SELECT a + b * c FROM t`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
		},
		{
			name:     "between",
			sql:      `SELECT a FROM t WHERE x BETWEEN y AND z`,
			expected: map[string][]string{"t": {"a", "x", "y", "z"}},
		},
		{
			name:     "in_list",
			sql:      `SELECT a FROM t WHERE x IN (y, z)`,
			expected: map[string][]string{"t": {"a", "x", "y", "z"}},
		},
		{
			name:     "group_by_expression",
			sql:      `SELECT a + b FROM t GROUP BY a + b`,
			expected: map[string][]string{"t": {"a", "b"}},
		},
		{
			name:     "window_frame",
			sql:      `SELECT sum(amt) OVER (PARTITION BY p ORDER BY o ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t`,
			expected: map[string][]string{"t": {"amt", "o", "p"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
		})
	}
}

// --------------------------------------------------------------------------- //
// FROM-position constructs: USING, self-join, UNNEST, PIVOT, lateral view.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsFromConstructs(t *testing.T) {
	t.Run("using_columns_included", func(t *testing.T) {
		// USING (k) reads k from both joined tables; with no metadata the
		// unqualified projection column a is also attributed to both (superset).
		assertReferencedColumns(t, "using",
			`SELECT a FROM t JOIN r USING (k)`,
			map[string][]string{"t": {"a", "k"}, "r": {"a", "k"}},
		)
	})
	t.Run("self_join", func(t *testing.T) {
		assertReferencedColumns(t, "self_join",
			`SELECT a.x, b.y FROM t a JOIN t b ON a.id = b.id`,
			map[string][]string{"t": {"id", "x", "y"}},
		)
	})
	t.Run("unnest_reads_source_column", func(t *testing.T) {
		assertReferencedColumns(t, "unnest",
			`SELECT i FROM t, UNNEST(t.arr) AS x(i)`,
			map[string][]string{"t": {"arr", "i"}},
		)
	})
	t.Run("pivot_reads_inner_columns", func(t *testing.T) {
		assertReferencedColumns(t, "pivot",
			`SELECT * FROM (SELECT region, amt FROM s) PIVOT (sum(amt) FOR region IN ('A'))`,
			map[string][]string{"s": {"amt", "region"}},
		)
	})
	t.Run("lateral_view_explode", func(t *testing.T) {
		assertReferencedColumns(t, "lateral_view",
			`SELECT e FROM t LATERAL VIEW explode(t.arr) tbl AS e`,
			map[string][]string{"t": {"arr", "e"}},
			WithLineageDialect("spark"),
		)
	})
}

// --------------------------------------------------------------------------- //
// 宁滥勿缺: unresolvable references must broadcast (never silently drop).
// --------------------------------------------------------------------------- //

func TestReferencedColumnsNeverDropsUnresolvable(t *testing.T) {
	t.Run("unknown_qualifier_broadcasts", func(t *testing.T) {
		// x is not a known alias; the column must still surface, attributed to
		// every physical source in scope rather than being dropped.
		assertReferencedColumns(t, "unknown_qualifier",
			`SELECT x.a FROM t`,
			map[string][]string{"t": {"a"}},
		)
	})
	t.Run("unknown_column_multi_table_no_metadata_broadcasts", func(t *testing.T) {
		assertReferencedColumns(t, "unknown_column_no_metadata",
			`SELECT name FROM a JOIN b ON a.k = b.k WHERE foo > 1`,
			map[string][]string{
				"a": {"foo", "k", "name"},
				"b": {"foo", "k", "name"},
			},
		)
	})
	t.Run("column_absent_from_incomplete_metadata_broadcasts", func(t *testing.T) {
		// Metadata is present but lists neither "foo"; rather than drop it, foo is
		// attributed to every physical source in scope.
		assertReferencedColumns(t, "incomplete_metadata",
			`SELECT u.id FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid WHERE foo > 1`,
			map[string][]string{
				"hive.raw.users":  {"foo", "id"},
				"hive.raw.orders": {"foo", "uid"},
			},
			WithLineageMetadata(refColsMetadata),
		)
	})
	t.Run("derived_star_passthrough_broadcasts_to_roots", func(t *testing.T) {
		// s.x cannot be found in the (unexpanded) SELECT * output, so it is
		// attributed to the subquery's root table t instead of dropped.
		assertReferencedColumns(t, "derived_star_passthrough",
			`SELECT s.x FROM (SELECT * FROM t) s`,
			map[string][]string{"t": {"*", "x"}},
		)
	})
}

// --------------------------------------------------------------------------- //
// DML mutations read columns in WHERE / SET / ON / WHEN — never empty.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsDML(t *testing.T) {
	cases := []struct {
		name     string
		dialect  string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "delete_where",
			dialect:  "trino",
			sql:      `DELETE FROM t WHERE a > 1`,
			expected: map[string][]string{"t": {"a"}},
		},
		{
			name:     "delete_using",
			dialect:  "postgres",
			sql:      `DELETE FROM t USING r WHERE t.id = r.id AND r.k > 1`,
			expected: map[string][]string{"t": {"id"}, "r": {"id", "k"}},
		},
		{
			name:     "update_set_and_where",
			dialect:  "trino",
			sql:      `UPDATE t SET x = y + 1 WHERE a > 1`,
			expected: map[string][]string{"t": {"a", "x", "y"}},
		},
		{
			name:     "update_from",
			dialect:  "postgres",
			sql:      `UPDATE t SET x = s.v FROM r s WHERE t.id = s.id`,
			expected: map[string][]string{"t": {"id", "x"}, "r": {"id", "v"}},
		},
		{
			name:     "merge",
			dialect:  "trino",
			sql:      `MERGE INTO t USING r ON t.id = r.id WHEN MATCHED THEN UPDATE SET x = r.v WHEN NOT MATCHED THEN INSERT (a) VALUES (r.b)`,
			expected: map[string][]string{"t": {"id"}, "r": {"b", "id", "v"}},
		},
		{
			name:     "merge_with_subquery_source",
			dialect:  "trino",
			sql:      `MERGE INTO t USING (SELECT id, v FROM r WHERE z > 0) s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = s.v`,
			expected: map[string][]string{"t": {"id"}, "r": {"id", "v", "z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected, WithLineageDialect(tc.dialect))
		})
	}
}

// --------------------------------------------------------------------------- //
// Deep nesting, recursive CTE, quoted identifiers, multi-statement.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsMiscEdges(t *testing.T) {
	cases := []struct {
		name     string
		dialect  string
		sql      string
		expected map[string][]string
	}{
		{
			name:     "deeply_nested_subqueries",
			dialect:  "trino",
			sql:      `SELECT z FROM (SELECT y AS z FROM (SELECT x AS y FROM t WHERE w > 0) i WHERE i.y > 0) o WHERE o.z > 0`,
			expected: map[string][]string{"t": {"w", "x"}},
		},
		{
			name:     "recursive_cte_terminates",
			dialect:  "trino",
			sql:      `WITH RECURSIVE c AS (SELECT a FROM t UNION ALL SELECT c.a FROM c WHERE c.a > 0) SELECT a FROM c`,
			expected: map[string][]string{"t": {"a"}},
		},
		{
			name:     "quoted_identifiers",
			dialect:  "trino",
			sql:      `SELECT "Order" FROM t WHERE "User" > 1`,
			expected: map[string][]string{"t": {"Order", "User"}},
		},
		{
			name:     "first_of_multiple_statements",
			dialect:  "trino",
			sql:      `SELECT a FROM t; SELECT b FROM r`,
			expected: map[string][]string{"t": {"a"}},
		},
		{
			name:     "insert_target_columns_excluded",
			dialect:  "trino",
			sql:      `INSERT INTO d (x, y) SELECT a, b FROM t WHERE c > 1`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected, WithLineageDialect(tc.dialect))
		})
	}
}

// --------------------------------------------------------------------------- //
// Differential invariant: ReferencedColumns is a superset of
// LineageSourceColumns over a shared corpus (same keys, column superset).
// --------------------------------------------------------------------------- //

func TestReferencedColumnsSupersetInvariant(t *testing.T) {
	corpus := []string{
		`SELECT user_id FROM hive.raw.orders WHERE status = 'X'`,
		`SELECT u.user_name FROM hive.raw.users u JOIN hive.raw.orders o ON u.user_id = o.user_id WHERE o.status = 'PAID'`,
		`CREATE VIEW v AS SELECT o.amount FROM hive.raw.orders o WHERE o.order_date >= DATE '2024-01-01'`,
		`WITH p AS (SELECT order_id, user_id FROM hive.raw.orders WHERE status = 'PAID') SELECT order_id FROM p WHERE user_id > 0`,
		`SELECT user_id FROM hive.raw.users UNION SELECT user_id FROM hive.raw.orders`,
		`CREATE TABLE d AS SELECT o.amount FROM hive.raw.orders o GROUP BY o.amount HAVING count(o.order_id) > 1`,
		`INSERT INTO d SELECT u.user_name FROM hive.raw.users u WHERE u.email IS NOT NULL ORDER BY u.user_id`,
		`SELECT amount FROM hive.raw.orders o WHERE o.user_id IN (SELECT user_id FROM hive.raw.users WHERE email LIKE '%@x.com')`,
		`SELECT * FROM hive.raw.orders WHERE status = 'PAID'`,
		`SELECT o.amount, u.user_name FROM hive.raw.orders o JOIN hive.raw.users u ON o.user_id = u.user_id WHERE u.email IS NOT NULL GROUP BY o.amount, u.user_name`,
	}
	for _, sql := range corpus {
		lineage, err := LineageSourceColumns(sql,
			WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
		if err != nil {
			t.Fatalf("LineageSourceColumns(%q): %v", sql, err)
		}
		referenced, err := ReferencedColumns(sql,
			WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
		if err != nil {
			t.Fatalf("ReferencedColumns(%q): %v", sql, err)
		}
		for tbl, cols := range lineage {
			refCols, ok := referenced[tbl]
			if !ok {
				t.Fatalf("%q: table %q in lineage but missing from referenced (%v)", sql, tbl, referenced)
			}
			refSet := map[string]struct{}{}
			for _, c := range refCols {
				refSet[c] = struct{}{}
			}
			for _, c := range cols {
				if _, ok := refSet[c]; !ok {
					t.Fatalf("%q: column %q.%q in lineage but not in referenced %v",
						sql, tbl, c, refCols)
				}
			}
		}
	}
}
