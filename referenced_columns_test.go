package easysql

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
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

func assertReferencedColumnUsages(t *testing.T, name, sql string, expected []ColumnUse, opts ...LineageOption) {
	t.Helper()
	opts = append([]LineageOption{WithLineageDialect("trino")}, opts...)
	actual, err := ReferencedColumnUsages(sql, opts...)
	if err != nil {
		t.Fatalf("%s: ReferencedColumnUsages: %v", name, err)
	}
	want := append([]ColumnUse{}, expected...)
	sort.Slice(want, func(i, j int) bool {
		if want[i].Table != want[j].Table {
			return want[i].Table < want[j].Table
		}
		if want[i].Column != want[j].Column {
			return want[i].Column < want[j].Column
		}
		return want[i].Clause < want[j].Clause
	})
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("%s\nexpected usages: %v\nactual usages:   %v", name, want, actual)
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

func TestReferencedColumnsRejectsMultipleStatements(t *testing.T) {
	sql := `SELECT a FROM t; SELECT secret FROM restricted`
	_, err := ReferencedColumns(sql, WithLineageDialect("trino"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ReferencedColumns error = %v, want ErrUnsupported", err)
	}
	_, err = ReferencedColumnUsages(sql, WithLineageDialect("trino"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ReferencedColumnUsages error = %v, want ErrUnsupported", err)
	}
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
		usages   []ColumnUse
	}{
		{
			name:     "where",
			sql:      `SELECT a FROM t WHERE b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseWhere},
			},
		},
		{
			name:     "group_by",
			sql:      `SELECT a, count(*) FROM t GROUP BY a, g`,
			expected: map[string][]string{"t": {"a", "g"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "a", Clause: ColumnClauseGroupBy},
				{Table: "t", Column: "g", Clause: ColumnClauseGroupBy},
			},
		},
		{
			name:     "group_by_projection_alias",
			sql:      `SELECT a + b AS x FROM t GROUP BY x`,
			expected: map[string][]string{"t": {"a", "b"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "a", Clause: ColumnClauseGroupBy},
				{Table: "t", Column: "b", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseGroupBy},
			},
		},
		{
			name:     "group_by_ordinal",
			sql:      `SELECT a, b FROM t GROUP BY 1, b`,
			expected: map[string][]string{"t": {"a", "b"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "a", Clause: ColumnClauseGroupBy},
				{Table: "t", Column: "b", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseGroupBy},
			},
		},
		{
			name:     "having",
			sql:      `SELECT a FROM t GROUP BY a HAVING sum(h) > 1`,
			expected: map[string][]string{"t": {"a", "h"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "a", Clause: ColumnClauseGroupBy},
				{Table: "t", Column: "h", Clause: ColumnClauseHaving},
			},
		},
		{
			name:     "order_by",
			sql:      `SELECT a FROM t ORDER BY o`,
			expected: map[string][]string{"t": {"a", "o"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "o", Clause: ColumnClauseOrderBy},
			},
		},
		{
			name:     "order_by_projection_alias",
			sql:      `SELECT a AS x FROM t ORDER BY x`,
			expected: map[string][]string{"t": {"a"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "a", Clause: ColumnClauseOrderBy},
			},
		},
		{
			name:     "order_by_numeric_expression_is_not_ordinal",
			sql:      `SELECT a, b FROM t ORDER BY b + 1`,
			expected: map[string][]string{"t": {"a", "b"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseOrderBy},
			},
		},
		{
			name:     "window_partition_and_order",
			sql:      `SELECT a, row_number() OVER (PARTITION BY b ORDER BY c) rn FROM t`,
			expected: map[string][]string{"t": {"a", "b", "c"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseSelect},
				{Table: "t", Column: "c", Clause: ColumnClauseSelect},
			},
		},
		{
			name:     "named_window_clause",
			sql:      `SELECT sum(a) OVER w FROM t WINDOW w AS (PARTITION BY p ORDER BY o)`,
			expected: map[string][]string{"t": {"a", "o", "p"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "p", Clause: ColumnClauseWindow},
				{Table: "t", Column: "o", Clause: ColumnClauseWindow},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected)
			assertReferencedColumnUsages(t, tc.name, tc.sql, tc.usages)
		})
	}
}

// QUALIFY is not parsed by the Trino dialect, so this clause is covered under a
// dialect that supports it (Snowflake). Resolution is dialect-agnostic.
func TestReferencedColumnsQualifyClause(t *testing.T) {
	t.Run("inline_expression", func(t *testing.T) {
		sql := `SELECT a FROM t QUALIFY row_number() OVER (PARTITION BY p ORDER BY q) = 1`
		assertReferencedColumns(t, "qualify", sql,
			map[string][]string{"t": {"a", "p", "q"}},
			WithLineageDialect("snowflake"),
		)
		assertReferencedColumnUsages(t, "qualify", sql,
			[]ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "p", Clause: ColumnClauseQualify},
				{Table: "t", Column: "q", Clause: ColumnClauseQualify},
			},
			WithLineageDialect("snowflake"),
		)
	})
	t.Run("projection_alias", func(t *testing.T) {
		sql := `SELECT a, row_number() OVER (PARTITION BY p ORDER BY q) AS rn FROM t QUALIFY rn = 1`
		assertReferencedColumns(t, "qualify_alias", sql,
			map[string][]string{"t": {"a", "p", "q"}},
			WithLineageDialect("snowflake"),
		)
		assertReferencedColumnUsages(t, "qualify_alias", sql,
			[]ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "p", Clause: ColumnClauseSelect},
				{Table: "t", Column: "p", Clause: ColumnClauseQualify},
				{Table: "t", Column: "q", Clause: ColumnClauseSelect},
				{Table: "t", Column: "q", Clause: ColumnClauseQualify},
			},
			WithLineageDialect("snowflake"),
		)
	})
}

func TestReferencedColumnUsagesDialectClauses(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		sql     string
		column  string
		clause  ColumnClause
	}{
		{
			name:    "sort_by",
			dialect: "spark",
			sql:     `SELECT a FROM t SORT BY s`,
			column:  "s",
			clause:  ColumnClauseSortBy,
		},
		{
			name:    "distribute_by",
			dialect: "spark",
			sql:     `SELECT a FROM t DISTRIBUTE BY d`,
			column:  "d",
			clause:  ColumnClauseDistributeBy,
		},
		{
			name:    "cluster_by",
			dialect: "spark",
			sql:     `SELECT a FROM t CLUSTER BY c`,
			column:  "c",
			clause:  ColumnClauseClusterBy,
		},
		{
			name:    "connect_by",
			dialect: "oracle",
			sql:     `SELECT a FROM t CONNECT BY PRIOR id = parent_id`,
			column:  "id",
			clause:  ColumnClauseConnectBy,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expected := []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: tc.column, Clause: tc.clause},
			}
			if tc.name == "connect_by" {
				expected = append(expected,
					ColumnUse{Table: "t", Column: "parent_id", Clause: ColumnClauseConnectBy})
			}
			assertReferencedColumnUsages(t, tc.name, tc.sql, expected,
				WithLineageDialect(tc.dialect))
		})
	}
}

// --------------------------------------------------------------------------- //
// JOINs: ON columns are included; resolution by alias and by metadata.
// --------------------------------------------------------------------------- //

func TestReferencedColumnsJoinQualified(t *testing.T) {
	sql := `SELECT u.name
         FROM hive.raw.users u
         JOIN hive.raw.orders o ON u.id = o.uid
         WHERE o.status = 'X'
         GROUP BY u.name`
	assertReferencedColumns(t, "join_qualified", sql,
		map[string][]string{
			"hive.raw.users":  {"id", "name"},
			"hive.raw.orders": {"status", "uid"},
		},
		WithLineageMetadata(refColsMetadata),
	)
	assertReferencedColumnUsages(t, "join_qualified", sql,
		[]ColumnUse{
			{Table: "hive.raw.users", Column: "name", Clause: ColumnClauseSelect},
			{Table: "hive.raw.users", Column: "name", Clause: ColumnClauseGroupBy},
			{Table: "hive.raw.users", Column: "id", Clause: ColumnClauseJoinOn},
			{Table: "hive.raw.orders", Column: "uid", Clause: ColumnClauseJoinOn},
			{Table: "hive.raw.orders", Column: "status", Clause: ColumnClauseWhere},
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

func TestReferencedColumnsFullyQualifiedSameBareTableName(t *testing.T) {
	assertReferencedColumns(t, "fully_qualified_same_bare_table_name",
		`SELECT hive.raw.users.id, hive.stage.users.email
         FROM hive.raw.users
         JOIN hive.stage.users ON hive.raw.users.id = hive.stage.users.id`,
		map[string][]string{
			"hive.raw.users":   {"id"},
			"hive.stage.users": {"email", "id"},
		},
		WithLineageMetadata(map[string][]string{
			"hive.raw.users":   {"id", "name"},
			"hive.stage.users": {"id", "email"},
		}),
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
		usages   []ColumnUse
	}{
		{
			name:     "single_cte_with_filter",
			sql:      `WITH c AS (SELECT a, b FROM t) SELECT c.a FROM c WHERE c.b > 1`,
			expected: map[string][]string{"t": {"a", "b"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseSelect},
				{Table: "t", Column: "b", Clause: ColumnClauseWhere},
			},
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
			if tc.usages != nil {
				assertReferencedColumnUsages(t, tc.name, tc.sql, tc.usages)
			}
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
		usages   []ColumnUse
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
			name:     "scalar_subquery_projection_alias_in_order_by",
			sql:      `SELECT (SELECT max(x) FROM r) AS m FROM t ORDER BY m`,
			expected: map[string][]string{"t": {}, "r": {"x"}},
			usages: []ColumnUse{
				{Table: "r", Column: "x", Clause: ColumnClauseSelect},
				{Table: "r", Column: "x", Clause: ColumnClauseOrderBy},
			},
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
			if tc.usages != nil {
				assertReferencedColumnUsages(t, tc.name, tc.sql, tc.usages)
			}
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
		sql := `SELECT * FROM hive.raw.users WHERE id > 0`
		assertReferencedColumns(t, "bare_star_with_metadata", sql,
			map[string][]string{"hive.raw.users": {"email", "id", "name"}},
			WithLineageMetadata(refColsMetadata),
		)
		assertReferencedColumnUsages(t, "bare_star_with_metadata", sql,
			[]ColumnUse{
				{Table: "hive.raw.users", Column: "id", Clause: ColumnClauseSelect},
				{Table: "hive.raw.users", Column: "name", Clause: ColumnClauseSelect},
				{Table: "hive.raw.users", Column: "email", Clause: ColumnClauseSelect},
				{Table: "hive.raw.users", Column: "id", Clause: ColumnClauseWhere},
			},
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
	sql := `SELECT 1 FROM t`
	assertReferencedColumns(t, "select_constant", sql,
		map[string][]string{"t": {}},
	)
	assertReferencedColumnUsages(t, "select_constant", sql, []ColumnUse{})
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
		usages, err := ReferencedColumnUsages(sql, WithLineageDialect("trino"))
		if err != nil {
			t.Fatalf("ReferencedColumnUsages(%q): %v", sql, err)
		}
		if len(usages) != 0 {
			t.Fatalf("ReferencedColumnUsages(%q) = %v; want empty", sql, usages)
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
		sql := `SELECT a FROM t JOIN r USING (k)`
		assertReferencedColumns(t, "using", sql,
			map[string][]string{"t": {"a", "k"}, "r": {"a", "k"}},
		)
		assertReferencedColumnUsages(t, "using", sql, []ColumnUse{
			{Table: "t", Column: "a", Clause: ColumnClauseSelect},
			{Table: "r", Column: "a", Clause: ColumnClauseSelect},
			{Table: "t", Column: "k", Clause: ColumnClauseJoinUsing},
			{Table: "r", Column: "k", Clause: ColumnClauseJoinUsing},
		})
	})
	t.Run("self_join", func(t *testing.T) {
		assertReferencedColumns(t, "self_join",
			`SELECT a.x, b.y FROM t a JOIN t b ON a.id = b.id`,
			map[string][]string{"t": {"id", "x", "y"}},
		)
	})
	t.Run("unnest_reads_source_column", func(t *testing.T) {
		sql := `SELECT i FROM t, UNNEST(t.arr) AS x(i)`
		assertReferencedColumns(t, "unnest", sql,
			map[string][]string{"t": {"arr", "i"}},
		)
		assertReferencedColumnUsages(t, "unnest", sql, []ColumnUse{
			{Table: "t", Column: "arr", Clause: ColumnClauseFrom},
			{Table: "t", Column: "i", Clause: ColumnClauseSelect},
		})
	})
	t.Run("pivot_reads_inner_columns", func(t *testing.T) {
		assertReferencedColumns(t, "pivot",
			`SELECT * FROM (SELECT region, amt FROM s) PIVOT (sum(amt) FOR region IN ('A'))`,
			map[string][]string{"s": {"amt", "region"}},
		)
	})
	t.Run("lateral_view_explode", func(t *testing.T) {
		sql := `SELECT e FROM t LATERAL VIEW explode(t.arr) tbl AS e`
		assertReferencedColumns(t, "lateral_view", sql,
			map[string][]string{"t": {"arr", "e"}},
			WithLineageDialect("spark"),
		)
		assertReferencedColumnUsages(t, "lateral_view", sql,
			[]ColumnUse{
				{Table: "t", Column: "arr", Clause: ColumnClauseLateralView},
				{Table: "t", Column: "e", Clause: ColumnClauseSelect},
			},
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
		usages   []ColumnUse
	}{
		{
			name:     "delete_where",
			dialect:  "trino",
			sql:      `DELETE FROM t WHERE a > 1`,
			expected: map[string][]string{"t": {"a"}},
			usages: []ColumnUse{
				{Table: "t", Column: "a", Clause: ColumnClauseWhere},
			},
		},
		{
			name:     "delete_using",
			dialect:  "postgres",
			sql:      `DELETE FROM t USING r WHERE t.id = r.id AND r.k > 1`,
			expected: map[string][]string{"t": {"id"}, "r": {"id", "k"}},
			usages: []ColumnUse{
				{Table: "t", Column: "id", Clause: ColumnClauseWhere},
				{Table: "r", Column: "id", Clause: ColumnClauseWhere},
				{Table: "r", Column: "k", Clause: ColumnClauseWhere},
			},
		},
		{
			name:     "update_set_and_where",
			dialect:  "trino",
			sql:      `UPDATE t SET x = y + 1 WHERE a > 1`,
			expected: map[string][]string{"t": {"a", "x", "y"}},
			usages: []ColumnUse{
				{Table: "t", Column: "x", Clause: ColumnClauseUpdateSetTarget},
				{Table: "t", Column: "y", Clause: ColumnClauseUpdateSetValue},
				{Table: "t", Column: "a", Clause: ColumnClauseWhere},
			},
		},
		{
			name:     "update_from",
			dialect:  "postgres",
			sql:      `UPDATE t SET x = s.v FROM r s WHERE t.id = s.id`,
			expected: map[string][]string{"t": {"id", "x"}, "r": {"id", "v"}},
			usages: []ColumnUse{
				{Table: "t", Column: "x", Clause: ColumnClauseUpdateSetTarget},
				{Table: "r", Column: "v", Clause: ColumnClauseUpdateSetValue},
				{Table: "t", Column: "id", Clause: ColumnClauseWhere},
				{Table: "r", Column: "id", Clause: ColumnClauseWhere},
			},
		},
		{
			name:     "merge",
			dialect:  "trino",
			sql:      `MERGE INTO t USING r ON t.id = r.id WHEN MATCHED THEN UPDATE SET x = r.v WHEN NOT MATCHED THEN INSERT (a) VALUES (r.b)`,
			expected: map[string][]string{"t": {"id"}, "r": {"b", "id", "v"}},
			usages: []ColumnUse{
				{Table: "t", Column: "id", Clause: ColumnClauseMergeOn},
				{Table: "r", Column: "id", Clause: ColumnClauseMergeOn},
				{Table: "r", Column: "v", Clause: ColumnClauseMergeWhen},
				{Table: "r", Column: "b", Clause: ColumnClauseMergeWhen},
			},
		},
		{
			name:     "merge_with_subquery_source",
			dialect:  "trino",
			sql:      `MERGE INTO t USING (SELECT id, v FROM r WHERE z > 0) s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = s.v`,
			expected: map[string][]string{"t": {"id"}, "r": {"id", "v", "z"}},
			usages: []ColumnUse{
				{Table: "t", Column: "id", Clause: ColumnClauseMergeOn},
				{Table: "r", Column: "id", Clause: ColumnClauseSelect},
				{Table: "r", Column: "id", Clause: ColumnClauseMergeOn},
				{Table: "r", Column: "v", Clause: ColumnClauseSelect},
				{Table: "r", Column: "v", Clause: ColumnClauseMergeWhen},
				{Table: "r", Column: "z", Clause: ColumnClauseWhere},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertReferencedColumns(t, tc.name, tc.sql, tc.expected, WithLineageDialect(tc.dialect))
			assertReferencedColumnUsages(t, tc.name, tc.sql, tc.usages, WithLineageDialect(tc.dialect))
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
		// Every corpus query reads at least one physical table. Without these
		// guards the superset loop below would pass vacuously if either analyzer
		// regressed to returning an empty map (the loop body simply never runs),
		// so a totally broken lineage/referenced pass would look green.
		if len(lineage) == 0 {
			t.Fatalf("%q: LineageSourceColumns returned no source tables", sql)
		}
		if len(referenced) == 0 {
			t.Fatalf("%q: ReferencedColumns returned no source tables", sql)
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

// ---------------------------------------------------------------------------
// Audit probes for ReferencedColumns.
// ---------------------------------------------------------------------------

// rcHuntMeta is a small catalog for metadata-driven probes.
var rcHuntMeta = map[string][]string{
	"hive.raw.users":  {"id", "name", "email"},
	"hive.raw.orders": {"oid", "uid", "amt", "status"},
}

func rcHuntRun(t *testing.T, sql string, opts ...LineageOption) map[string][]string {
	t.Helper()
	opts = append([]LineageOption{WithLineageDialect("trino")}, opts...)
	got, err := ReferencedColumns(sql, opts...)
	if err != nil {
		t.Fatalf("ReferencedColumns(%q): %v", sql, err)
	}
	return got
}

func rcHuntWant(expected map[string][]string) map[string][]string {
	want := map[string][]string{}
	for tbl, cols := range expected {
		c := append([]string{}, cols...)
		sort.Strings(c)
		want[tbl] = c
	}
	return want
}

// Property test: ReferencedColumns ⊇ LineageSourceColumns per table, over a
// corpus of SELECT queries (documented: "returns a superset of
// LineageSourceColumns's result"). Synthesized positional names (_0, _1, …)
// must never be emitted as source columns by LineageSourceColumns.
func TestBughuntSupersetProperty(t *testing.T) {
	corpus := []string{
		`SELECT id FROM hive.raw.users WHERE email IS NOT NULL`,
		`SELECT u.name, o.amt FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid`,
		`SELECT name FROM hive.raw.users ORDER BY id`,
		`SELECT status, sum(amt) FROM hive.raw.orders GROUP BY status HAVING count(oid) > 1`,
		`WITH c AS (SELECT id, name FROM hive.raw.users WHERE email LIKE '%x') SELECT name FROM c WHERE id > 0`,
		`SELECT * FROM hive.raw.orders WHERE status = 'PAID'`,
		`SELECT id FROM hive.raw.users UNION ALL SELECT uid FROM hive.raw.orders`,
		`SELECT s.n FROM (SELECT name AS n, id FROM hive.raw.users) s WHERE s.id > 1`,
		`SELECT amt FROM hive.raw.orders o WHERE o.uid IN (SELECT id FROM hive.raw.users WHERE name = 'a')`,
		`SELECT id, row_number() OVER (PARTITION BY name ORDER BY email) FROM hive.raw.users`,
		`SELECT a.id FROM hive.raw.users a JOIN hive.raw.users b ON a.email = b.name`,
	}
	synthetic := regexp.MustCompile(`^_\d+$`)
	var violations, syntheticViolations []string
	for _, sql := range corpus {
		lineage, err := LineageSourceColumns(sql, WithLineageDialect("trino"), WithLineageMetadata(rcHuntMeta))
		if err != nil {
			t.Fatalf("LineageSourceColumns(%q): %v", sql, err)
		}
		referenced, err := ReferencedColumns(sql, WithLineageDialect("trino"), WithLineageMetadata(rcHuntMeta))
		if err != nil {
			t.Fatalf("ReferencedColumns(%q): %v", sql, err)
		}
		for tbl, cols := range lineage {
			refCols, ok := referenced[tbl]
			if !ok {
				violations = append(violations, fmt.Sprintf("%q: table %q in lineage but missing from referenced (%v)", sql, tbl, referenced))
				continue
			}
			set := map[string]struct{}{}
			for _, c := range refCols {
				set[c] = struct{}{}
			}
			for _, c := range cols {
				if _, ok := set[c]; !ok {
					msg := fmt.Sprintf("%q: %s.%s in lineage but not in referenced %v", sql, tbl, c, refCols)
					if synthetic.MatchString(c) {
						syntheticViolations = append(syntheticViolations, msg)
					} else {
						violations = append(violations, msg)
					}
				}
			}
		}
	}
	for _, v := range violations {
		t.Error(v)
	}
	for _, v := range syntheticViolations {
		t.Errorf("superset invariant violated by a synthesized positional name (LineageSourceColumns must not emit _0/_1/… as source columns): %s", v)
	}
}

// Self-referencing non-recursive CTE: in standard SQL (and Trino, without
// RECURSIVE) the inner `t` in `WITH t AS (SELECT a FROM t)` is the PHYSICAL
// table t — a non-recursive CTE cannot reference itself.
func TestBughuntCTEShadowSelfReference(t *testing.T) {
	got := rcHuntRun(t, `WITH t AS (SELECT a FROM t) SELECT a FROM t`)
	want := rcHuntWant(map[string][]string{"t": {"a"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("self-referencing non-recursive CTE: inner FROM t is the physical table; got %v want %v", got, want)
	}
}

// A CTE defined AFTER its use site: in standard SQL a CTE body may only see
// earlier siblings, so `b` inside the first CTE is the physical table b.
func TestBughuntCTEForwardSiblingReference(t *testing.T) {
	got := rcHuntRun(t, `WITH a AS (SELECT x FROM b), b AS (SELECT y FROM t) SELECT x FROM a`)
	// Physical table b must at least be present (fail-open: never drop a
	// physical table appearing in a FROM). We only assert presence of key "b".
	if _, ok := got["b"]; !ok {
		t.Errorf("forward-sibling CTE reference must resolve to the physical table b (present in result); got %v", got)
	}
}

// Qualified star with an unknown qualifier must broadcast, not drop: it surfaces
// at minimum as the "*" sentinel on the only physical table in scope.
func TestBughuntUnknownQualifierStarDropped(t *testing.T) {
	got := rcHuntRun(t, `SELECT x.* FROM t`)
	if cols, ok := got["t"]; !ok || len(cols) == 0 {
		t.Errorf("SELECT x.* with an unknown qualifier was dropped (t has no columns) instead of being broadcast as the \"*\" sentinel; got %v", got)
	}
}

// Correlated qualified star: t.* inside a subquery where t is only visible in
// the OUTER scope. expandStar must walk parent scopes and fail open.
func TestBughuntCorrelatedQualifiedStar(t *testing.T) {
	got := rcHuntRun(t, `SELECT a FROM t WHERE EXISTS (SELECT t.* FROM r WHERE r.k = t.a)`)
	cols := got["t"]
	found := false
	for _, c := range cols {
		if c == "*" {
			found = true
		}
	}
	if !found {
		t.Errorf("correlated t.* inside a subquery was dropped — expandStar must walk parent scopes and fail open; got %v", got)
	}
}

// USING reads the column from BOTH joined tables (documented: USING columns are
// counted; fail-open: never drop).
func TestBughuntUsingBothSides(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT u.name FROM hive.raw.users u JOIN hive.raw.orders o USING (id)`,
		WithLineageMetadata(map[string][]string{
			"hive.raw.users":  {"id", "name"},
			"hive.raw.orders": {"id", "amt"},
		}))
	want := rcHuntWant(map[string][]string{
		"hive.raw.users":  {"id", "name"},
		"hive.raw.orders": {"id"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("USING with column in both metadata entries:\n got %v\nwant %v", got, want)
	}
}

// Unqualified column listed in TWO tables' metadata: attributed to both
// (documented: "attributed to every in-scope source that declares it").
func TestBughuntUnqualifiedInTwoTablesMetadata(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT k FROM a JOIN b ON a.x = b.y`,
		WithLineageMetadata(map[string][]string{
			"a": {"k", "x"},
			"b": {"k", "y"},
		}))
	want := rcHuntWant(map[string][]string{
		"a": {"k", "x"},
		"b": {"k", "y"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unqualified column in two metadata tables:\n got %v\nwant %v", got, want)
	}
}

// Correlated subquery: unqualified column in the inner scope whose metadata
// home is the OUTER table. Fail-open says it must not be dropped; metadata
// attribution says it belongs to the declaring table.
func TestBughuntCorrelatedUnqualifiedMetadata(t *testing.T) {
	// "name" is declared only by hive.raw.users (outer). Inner scope's only
	// source is orders.
	got := rcHuntRun(t,
		`SELECT u.id FROM hive.raw.users u WHERE EXISTS (SELECT 1 FROM hive.raw.orders o WHERE o.uid = u.id AND name = 'x')`,
		WithLineageMetadata(rcHuntMeta))
	// Not dropped is the hard requirement; attribution to users is what the
	// metadata rule implies. Accept either users-only or broadcast including it.
	all := map[string]bool{}
	for tbl, cols := range got {
		for _, c := range cols {
			if c == "name" {
				all[tbl] = true
			}
		}
	}
	if len(all) == 0 {
		t.Errorf("correlated unqualified column 'name' dropped entirely: %v", got)
	} else if !all["hive.raw.users"] {
		// Out of scope (undocumented): metadata attribution across correlation.
		// Still fail-open (not a drop), so only note it rather than fail.
		t.Logf("NOTE: 'name' declared only by outer hive.raw.users but attributed to %v (metadata attribution not applied across correlation; still fail-open)", got)
	}
}

// Set operation with trailing ORDER BY must not error or drop tables.
func TestBughuntSetOpOrderBy(t *testing.T) {
	sql := `SELECT a FROM t UNION ALL SELECT b FROM r ORDER BY a`
	got := rcHuntRun(t, sql)
	want := rcHuntWant(map[string][]string{"t": {"a"}, "r": {"b"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("set-op ORDER BY:\n got %v\nwant %v", got, want)
	}
	assertReferencedColumnUsages(t, "set_op_order_by", sql, []ColumnUse{
		{Table: "t", Column: "a", Clause: ColumnClauseSelect},
		{Table: "t", Column: "a", Clause: ColumnClauseOrderBy},
		{Table: "r", Column: "b", Clause: ColumnClauseSelect},
		{Table: "r", Column: "b", Clause: ColumnClauseOrderBy},
	})
}

// Nested set-ops.
func TestBughuntNestedSetOps(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT a FROM t WHERE p > 0 UNION SELECT b FROM r EXCEPT SELECT c FROM s WHERE q < 1`)
	want := rcHuntWant(map[string][]string{
		"t": {"a", "p"}, "r": {"b"}, "s": {"c", "q"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nested set ops:\n got %v\nwant %v", got, want)
	}
}

// QUALIFY + window ORDER BY inside OVER (snowflake).
func TestBughuntQualifyWindow(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT a FROM t QUALIFY row_number() OVER (PARTITION BY p ORDER BY q DESC) = 1`,
		WithLineageDialect("snowflake"))
	want := rcHuntWant(map[string][]string{"t": {"a", "p", "q"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("qualify window:\n got %v\nwant %v", got, want)
	}
}

// CTE referenced twice under different aliases, each touching different columns.
func TestBughuntCTETwiceDifferentColumns(t *testing.T) {
	got := rcHuntRun(t,
		`WITH c AS (SELECT a, b, d FROM t) SELECT c1.a FROM c c1 JOIN c c2 ON c1.b = c2.d`)
	want := rcHuntWant(map[string][]string{"t": {"a", "b", "d"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cte twice:\n got %v\nwant %v", got, want)
	}
}

// CTE shadowing a physical table name, used from the main query only.
func TestBughuntCTEShadowsPhysicalName(t *testing.T) {
	got := rcHuntRun(t,
		`WITH orders AS (SELECT uid FROM hive.raw.orders WHERE status = 'X') SELECT uid FROM orders`)
	want := rcHuntWant(map[string][]string{"hive.raw.orders": {"status", "uid"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cte shadow: bare CTE name must not surface as a physical table:\n got %v\nwant %v", got, want)
	}
}

// Self-join: each alias touches different columns; both merge into one table.
func TestBughuntSelfJoinDistinctColumns(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT l.a, r.b FROM t l JOIN t r ON l.k1 = r.k2 WHERE l.w > 0 ORDER BY r.o`)
	want := rcHuntWant(map[string][]string{"t": {"a", "b", "k1", "k2", "o", "w"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("self join:\n got %v\nwant %v", got, want)
	}
}

// UPDATE ... SET with a table-qualified target column (mysql syntax) must
// resolve to the bare column on the target table, not record it verbatim.
func TestBughuntUpdateQualifiedSetTarget(t *testing.T) {
	got := rcHuntRun(t, `UPDATE t SET t.x = t.y + 1 WHERE t.a > 1`, WithLineageDialect("mysql"))
	want := rcHuntWant(map[string][]string{"t": {"a", "x", "y"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("table-qualified SET target must resolve to column \"x\" on t, not verbatim \"t.x\";\n got %v\nwant %v", got, want)
	}
}

// DELETE with a subquery in WHERE.
func TestBughuntDeleteWithSubquery(t *testing.T) {
	got := rcHuntRun(t, `DELETE FROM t WHERE id IN (SELECT uid FROM r WHERE flag = 1)`)
	want := rcHuntWant(map[string][]string{"t": {"id"}, "r": {"flag", "uid"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("delete subquery:\n got %v\nwant %v", got, want)
	}
}

// MERGE with search conditions on WHEN clauses.
func TestBughuntMergeWhenCondition(t *testing.T) {
	got := rcHuntRun(t,
		`MERGE INTO t USING r ON t.id = r.id WHEN MATCHED AND r.flag = 1 THEN DELETE`)
	want := rcHuntWant(map[string][]string{"t": {"id"}, "r": {"flag", "id"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge when condition:\n got %v\nwant %v", got, want)
	}
}

func TestBughuntOrderByOrdinal(t *testing.T) {
	sql := `SELECT a, b FROM t ORDER BY 1, b`
	got := rcHuntRun(t, sql)
	want := rcHuntWant(map[string][]string{"t": {"a", "b"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order by ordinal:\n got %v\nwant %v", got, want)
	}
	assertReferencedColumnUsages(t, "order_by_ordinal", sql, []ColumnUse{
		{Table: "t", Column: "a", Clause: ColumnClauseSelect},
		{Table: "t", Column: "a", Clause: ColumnClauseOrderBy},
		{Table: "t", Column: "b", Clause: ColumnClauseSelect},
		{Table: "t", Column: "b", Clause: ColumnClauseOrderBy},
	})
}

func TestBughuntHavingAggregate(t *testing.T) {
	got := rcHuntRun(t, `SELECT g FROM t GROUP BY g HAVING max(h) - min(h2) > 3`)
	want := rcHuntWant(map[string][]string{"t": {"g", "h", "h2"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("having aggregate:\n got %v\nwant %v", got, want)
	}
}

// SELECT * with metadata for only one of two joined tables: the covered table
// expands, the uncovered one keeps the "*" sentinel.
func TestBughuntStarPartialMetadata(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT * FROM hive.raw.users u JOIN unknown_tbl x ON u.id = x.k`,
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name"}}))
	want := rcHuntWant(map[string][]string{
		"hive.raw.users": {"id", "name"},
		"unknown_tbl":    {"*", "k"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("star partial metadata:\n got %v\nwant %v", got, want)
	}
}

// Output hygiene: deduped and sorted per table.
func TestBughuntDedupAndSorted(t *testing.T) {
	sql := `SELECT b, a, b FROM t WHERE a > 1 AND b < 2 ORDER BY a`
	got := rcHuntRun(t, sql)
	want := rcHuntWant(map[string][]string{"t": {"a", "b"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedup/sort:\n got %v\nwant %v", got, want)
	}
	for tbl, cols := range got {
		if !sort.StringsAreSorted(cols) {
			t.Errorf("columns for %s not sorted: %v", tbl, cols)
		}
	}
	assertReferencedColumnUsages(t, "usage_dedup_and_sort", sql, []ColumnUse{
		{Table: "t", Column: "b", Clause: ColumnClauseSelect},
		{Table: "t", Column: "a", Clause: ColumnClauseSelect},
		{Table: "t", Column: "b", Clause: ColumnClauseWhere},
		{Table: "t", Column: "a", Clause: ColumnClauseWhere},
		{Table: "t", Column: "a", Clause: ColumnClauseOrderBy},
	})
}

// Three-part qualified column references cat.sch.tbl.col.
func TestBughuntThreePartQualified(t *testing.T) {
	got := rcHuntRun(t,
		`SELECT c.s.t1.a FROM c.s.t1 JOIN c.s.t2 ON c.s.t1.k = c.s.t2.k WHERE c.s.t2.f > 0`)
	want := rcHuntWant(map[string][]string{
		"c.s.t1": {"a", "k"},
		"c.s.t2": {"f", "k"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("three-part qualified:\n got %v\nwant %v", got, want)
	}
}
