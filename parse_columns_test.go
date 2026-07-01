package easysql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// viewColumnsMetadata is the catalog used by star-expansion tests ported from
// view_columns Python UTs (distinct from trinoMetadata in lineage_test.go).
var viewColumnsMetadata = map[string][]string{
	"hive.raw.users":  {"id", "name", "email"},
	"hive.raw.orders": {"oid", "uid", "amt"},
	"c.s.other":       {"o1", "o2", "o3"},
}

const realViewSQL = `
CREATE VIEW vdm_mda.weiava.target_hcp_cnt AS
SELECT
  hco_province_cn
, hco_tier_cn
, standard_department_nm
, is_core_department
, bu
, brand
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202503') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q1 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202504' AND '202506') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q2 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202507' AND '202509') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q3 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202510' AND '202512') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q4 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202503') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q1 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202506') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q2 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202509') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q3 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202512') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2025 Q4 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202603') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q1 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202604' AND '202606') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q2 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202607' AND '202609') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q3 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202610' AND '202612') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q4 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202603') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q1 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202606') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q2 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202609') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q3 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202612') THEN concat(hcp_etms_cd, hco_etms_cd) END)) "hcp_hco 2026 Q4 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202503') THEN hcp_etms_cd END)) "hcp 2025 Q1 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202504' AND '202506') THEN hcp_etms_cd END)) "hcp 2025 Q2 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202507' AND '202509') THEN hcp_etms_cd END)) "hcp 2025 Q3 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202510' AND '202512') THEN hcp_etms_cd END)) "hcp 2025 Q4 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202503') THEN hcp_etms_cd END)) "hcp 2025 Q1 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202506') THEN hcp_etms_cd END)) "hcp 2025 Q2 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202509') THEN hcp_etms_cd END)) "hcp 2025 Q3 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202501' AND '202512') THEN hcp_etms_cd END)) "hcp 2025 Q4 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202603') THEN hcp_etms_cd END)) "hcp 2026 Q1 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202604' AND '202606') THEN hcp_etms_cd END)) "hcp 2026 Q2 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202607' AND '202609') THEN hcp_etms_cd END)) "hcp 2026 Q3 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202610' AND '202612') THEN hcp_etms_cd END)) "hcp 2026 Q4 Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202603') THEN hcp_etms_cd END)) "hcp 2026 Q1 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202606') THEN hcp_etms_cd END)) "hcp 2026 Q2 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202609') THEN hcp_etms_cd END)) "hcp 2026 Q3 YTD Target"
, count(DISTINCT (CASE WHEN (yearmonth BETWEEN '202601' AND '202612') THEN hcp_etms_cd END)) "hcp 2026 Q4 YTD Target"
FROM
  iceberg.rda_launch_to_engage.target_hcp
WHERE (yearmonth >= '202501')
GROUP BY 1, 2, 3, 4, 5, 6
ORDER BY 1 ASC, 2 ASC, 3 ASC, 4 ASC, 5 ASC, 6 ASC
`

var realViewExpected = []string{
	"hco_province_cn",
	"hco_tier_cn",
	"standard_department_nm",
	"is_core_department",
	"bu",
	"brand",
	"hcp_hco 2025 Q1 Target",
	"hcp_hco 2025 Q2 Target",
	"hcp_hco 2025 Q3 Target",
	"hcp_hco 2025 Q4 Target",
	"hcp_hco 2025 Q1 YTD Target",
	"hcp_hco 2025 Q2 YTD Target",
	"hcp_hco 2025 Q3 YTD Target",
	"hcp_hco 2025 Q4 YTD Target",
	"hcp_hco 2026 Q1 Target",
	"hcp_hco 2026 Q2 Target",
	"hcp_hco 2026 Q3 Target",
	"hcp_hco 2026 Q4 Target",
	"hcp_hco 2026 Q1 YTD Target",
	"hcp_hco 2026 Q2 YTD Target",
	"hcp_hco 2026 Q3 YTD Target",
	"hcp_hco 2026 Q4 YTD Target",
	"hcp 2025 Q1 Target",
	"hcp 2025 Q2 Target",
	"hcp 2025 Q3 Target",
	"hcp 2025 Q4 Target",
	"hcp 2025 Q1 YTD Target",
	"hcp 2025 Q2 YTD Target",
	"hcp 2025 Q3 YTD Target",
	"hcp 2025 Q4 YTD Target",
	"hcp 2026 Q1 Target",
	"hcp 2026 Q2 Target",
	"hcp 2026 Q3 Target",
	"hcp 2026 Q4 Target",
	"hcp 2026 Q1 YTD Target",
	"hcp 2026 Q2 YTD Target",
	"hcp 2026 Q3 YTD Target",
	"hcp 2026 Q4 YTD Target",
}

func assertParseColumns(t *testing.T, name, sql string, expected []string, opts ...LineageOption) {
	t.Helper()
	opts = append([]LineageOption{WithLineageDialect("trino")}, opts...)
	actual, err := ParseColumns(sql, opts...)
	if err != nil {
		t.Fatalf("%s: ParseColumns: %v", name, err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s\nexpected: %v\nactual:   %v", name, expected, actual)
	}
}

func assertParseColumnsError(t *testing.T, name, sql string, opts ...LineageOption) {
	t.Helper()
	opts = append([]LineageOption{WithLineageDialect("trino")}, opts...)
	_, err := ParseColumns(sql, opts...)
	if err == nil {
		t.Fatalf("%s: expected error, got nil", name)
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("%s: expected ErrUnsupported, got %v", name, err)
	}
}

func TestParseColumnsRealViewBareAndQuotedAliasColumns(t *testing.T) {
	assertParseColumns(t, "real_view_bare_and_quoted_alias_columns", realViewSQL, realViewExpected)
}

func TestParseColumnsRealViewWithCommentAndSecurityDefiner(t *testing.T) {
	sql := strings.Replace(realViewSQL, "AS\nSELECT", "SECURITY DEFINER AS\nSELECT", 1)
	assertParseColumns(t, "real_view_with_comment_and_security_definer", sql, realViewExpected)
}

func TestParseColumnsUnaliasedExpressionsBecomeColIndex(t *testing.T) {
	assertParseColumns(t, "unaliased_expressions_become_col_index",
		"CREATE VIEW c.s.v AS SELECT a, count(*), sum(x) FROM t GROUP BY 1",
		[]string{"a", "_col1", "_col2"},
	)
}

func TestParseColumnsExplicitViewColumnListOverridesSelectNames(t *testing.T) {
	assertParseColumns(t, "explicit_view_column_list_overrides_select_names",
		"CREATE VIEW c.s.v (x1, x2, x3) AS SELECT a, b, count(*) FROM t GROUP BY 1, 2",
		[]string{"x1", "x2", "x3"},
	)
}

func TestParseColumnsBareSelectWithoutCreateView(t *testing.T) {
	assertParseColumns(t, "bare_select_without_create_view",
		"SELECT a AS x, b FROM t",
		[]string{"x", "b"},
	)
}

func TestParseColumnsUnionTakesNamesFromLeftmostSelect(t *testing.T) {
	assertParseColumns(t, "union_takes_names_from_leftmost_select",
		"SELECT a, count(*) FROM t GROUP BY 1 UNION ALL SELECT c, d FROM r",
		[]string{"a", "_col1"},
	)
}

func TestParseColumnsCTEViewUsesFinalSelectNames(t *testing.T) {
	assertParseColumns(t, "cte_view_uses_final_select_names", `
        CREATE VIEW c.s.v AS
        WITH paid AS (SELECT order_id, amount FROM orders WHERE status = 'PAID')
        SELECT order_id AS oid, amount AS amt FROM paid
        `, []string{"oid", "amt"})
}

func TestParseColumnsStarWithoutMetadataReturnsStar(t *testing.T) {
	assertParseColumns(t, "star_without_metadata_returns_star",
		"SELECT * FROM hive.raw.users",
		[]string{"*"},
	)
}

func TestParseColumnsStarWithExplicitEmptyTableMetadataReturnsEmpty(t *testing.T) {
	assertParseColumns(t, "star_with_explicit_empty_table_metadata_returns_empty",
		"SELECT * FROM hive.raw.users",
		[]string{},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {}}),
	)
}

func TestParseColumnsQualifiedStarWithExplicitEmptyTableMetadataReturnsEmpty(t *testing.T) {
	assertParseColumns(t, "qualified_star_with_explicit_empty_table_metadata_returns_empty",
		"SELECT u.* FROM hive.raw.users u",
		[]string{},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {}}),
	)
}

func TestParseColumnsStarWithMetadataIsExpanded(t *testing.T) {
	assertParseColumns(t, "star_with_metadata_is_expanded",
		"SELECT u.name, o.* FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid",
		[]string{"name", "oid", "uid", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateViewSelectStarSingleTableWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_view_select_star_single_table_with_metadata",
		"CREATE VIEW hive.analytics.v AS SELECT * FROM hive.raw.users",
		[]string{"id", "name", "email"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsCreateViewSelectStarJoinWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_view_select_star_join_with_metadata",
		"CREATE VIEW hive.analytics.v AS SELECT * FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid",
		[]string{"id", "name", "email", "oid", "uid", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateViewQualifiedStarWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_view_qualified_star_with_metadata",
		"CREATE VIEW hive.analytics.v AS SELECT u.*, o.amt FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid",
		[]string{"id", "name", "email", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateViewSelectStarPartialMetadataFallsBackToStar(t *testing.T) {
	assertParseColumns(t, "create_view_select_star_partial_metadata_falls_back_to_star",
		"CREATE VIEW hive.analytics.v AS SELECT * FROM hive.raw.users u JOIN hive.raw.orders o ON u.id = o.uid",
		[]string{"*"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsCTASBareAndUnaliasedColumns(t *testing.T) {
	assertParseColumns(t, "ctas_bare_and_unaliased_columns",
		"CREATE TABLE c.s.t AS SELECT a, b, count(*) FROM r GROUP BY 1, 2",
		[]string{"a", "b", "_col2"},
	)
}

func TestParseColumnsCTASWithExplicitColumnList(t *testing.T) {
	assertParseColumns(t, "ctas_with_explicit_column_list",
		"CREATE TABLE c.s.t (x1, x2) AS SELECT a, b FROM r",
		[]string{"x1", "x2"},
	)
}

func TestParseColumnsCTASWithQuotedAlias(t *testing.T) {
	assertParseColumns(t, "ctas_with_quoted_alias",
		`CREATE TABLE c.s.t AS SELECT a, count(*) "Total Cnt" FROM r GROUP BY 1`,
		[]string{"a", "Total Cnt"},
	)
}

func TestParseColumnsCreateOrReplaceTableCTAS(t *testing.T) {
	assertParseColumns(t, "create_or_replace_table_ctas",
		"CREATE OR REPLACE TABLE c.s.t AS SELECT x AS y, z FROM r",
		[]string{"y", "z"},
	)
}

func TestParseColumnsCreateTableDDLColumnDefs(t *testing.T) {
	assertParseColumns(t, "create_table_ddl_column_defs",
		"CREATE TABLE c.s.t (id BIGINT, name VARCHAR, created_at TIMESTAMP)",
		[]string{"id", "name", "created_at"},
	)
}

func TestParseColumnsCreateTableDDLWithTableProperties(t *testing.T) {
	assertParseColumns(t, "create_table_ddl_with_table_properties",
		"CREATE TABLE c.s.t (a INTEGER, b VARCHAR) WITH (format = 'PARQUET', partitioning = ARRAY['a'])",
		[]string{"a", "b"},
	)
}

func TestParseColumnsCreateTableDDLWithColumnCommentsAndConstraints(t *testing.T) {
	assertParseColumns(t, "create_table_ddl_with_column_comments_and_constraints",
		"CREATE TABLE c.s.t (id BIGINT NOT NULL COMMENT 'pk', name VARCHAR COMMENT 'n')",
		[]string{"id", "name"},
	)
}

func TestParseColumnsCreateTableIfNotExistsDDL(t *testing.T) {
	assertParseColumns(t, "create_table_if_not_exists_ddl",
		"CREATE TABLE IF NOT EXISTS c.s.t (a INT, b INT)",
		[]string{"a", "b"},
	)
}

func TestParseColumnsCreateTableAsWithCTE(t *testing.T) {
	assertParseColumns(t, "create_table_as_with_cte",
		"CREATE TABLE c.s.t AS WITH c AS (SELECT a, b FROM r) SELECT a, count(*) FROM c GROUP BY 1",
		[]string{"a", "_col1"},
	)
}

func TestParseColumnsCreateTableLikeWithoutMetadataRaises(t *testing.T) {
	assertParseColumnsError(t, "create_table_like_without_metadata_raises",
		"CREATE TABLE c.s.t (LIKE c.s.other)",
	)
}

func TestParseColumnsCreateTableLikeWithMetadataExpands(t *testing.T) {
	assertParseColumns(t, "create_table_like_with_metadata_expands",
		"CREATE TABLE c.s.t (LIKE c.s.other)",
		[]string{"o1", "o2", "o3"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateTableLikeIncludingPropertiesWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_table_like_including_properties_with_metadata",
		"CREATE TABLE c.s.t (LIKE c.s.other INCLUDING PROPERTIES)",
		[]string{"o1", "o2", "o3"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateTableMixedColumnsAndLikeWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_table_mixed_columns_and_like_with_metadata",
		"CREATE TABLE c.s.t (a INT, LIKE c.s.other, b VARCHAR)",
		[]string{"a", "o1", "o2", "o3", "b"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateTableLikeMatchesByNameSuffix(t *testing.T) {
	assertParseColumns(t, "create_table_like_matches_by_name_suffix",
		"CREATE TABLE c.s.t (LIKE other)",
		[]string{"o1", "o2", "o3"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsCreateMaterializedView(t *testing.T) {
	assertParseColumns(t, "create_materialized_view",
		"CREATE MATERIALIZED VIEW c.s.mv AS SELECT a, sum(b) total FROM r GROUP BY 1",
		[]string{"a", "total"},
	)
}

func TestParseColumnsCreateOrReplaceView(t *testing.T) {
	assertParseColumns(t, "create_or_replace_view",
		"CREATE OR REPLACE VIEW c.s.v AS SELECT a, b FROM r",
		[]string{"a", "b"},
	)
}

func TestParseColumnsWithSingleCTE(t *testing.T) {
	assertParseColumns(t, "with_single_cte",
		"WITH cte AS (SELECT a, b FROM r) SELECT a AS x, b FROM cte",
		[]string{"x", "b"},
	)
}

func TestParseColumnsWithMultipleCTEs(t *testing.T) {
	assertParseColumns(t, "with_multiple_ctes",
		"WITH c1 AS (SELECT a FROM r), c2 AS (SELECT b FROM s) SELECT c1.a, c2.b FROM c1, c2",
		[]string{"a", "b"},
	)
}

func TestParseColumnsWithCTEColumnAliases(t *testing.T) {
	assertParseColumns(t, "with_cte_column_aliases",
		"WITH c(x, y) AS (SELECT a, b FROM r) SELECT x, y FROM c",
		[]string{"x", "y"},
	)
}

func TestParseColumnsWithThenUnion(t *testing.T) {
	assertParseColumns(t, "with_then_union",
		"WITH c AS (SELECT a, b FROM r) SELECT a, count(*) FROM c GROUP BY 1 UNION ALL SELECT x, y FROM s",
		[]string{"a", "_col1"},
	)
}

func TestParseColumnsCreateViewWithCTE(t *testing.T) {
	assertParseColumns(t, "create_view_with_cte",
		"CREATE VIEW c.s.v AS WITH c AS (SELECT a, b FROM r) SELECT a AS oid, b AS amt FROM c",
		[]string{"oid", "amt"},
	)
}

func TestParseColumnsWithStarExpandedWithMetadata(t *testing.T) {
	assertParseColumns(t, "with_star_expanded_with_metadata",
		"WITH c AS (SELECT * FROM hive.raw.users) SELECT * FROM c",
		[]string{"id", "name", "email"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsWithJoinStarWithMetadata(t *testing.T) {
	assertParseColumns(t, "with_join_star_with_metadata", `
        WITH c AS (
            SELECT * FROM hive.raw.users u
            JOIN hive.raw.orders o ON u.id = o.uid
        ) SELECT * FROM c`,
		[]string{"id", "name", "email", "oid", "uid", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsWithQualifiedStarInCTEWithMetadata(t *testing.T) {
	assertParseColumns(t, "with_qualified_star_in_cte_with_metadata", `
        WITH c AS (
            SELECT u.*, o.amt FROM hive.raw.users u
            JOIN hive.raw.orders o ON u.id = o.uid
        ) SELECT * FROM c`,
		[]string{"id", "name", "email", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsWithOuterQualifiedStarWithMetadata(t *testing.T) {
	assertParseColumns(t, "with_outer_qualified_star_with_metadata",
		"WITH c AS (SELECT * FROM hive.raw.users) SELECT c.* FROM c",
		[]string{"id", "name", "email"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsWithJoinStarPartialMetadataFallsBackToStar(t *testing.T) {
	assertParseColumns(t, "with_join_star_partial_metadata_falls_back_to_star", `
        WITH c AS (
            SELECT * FROM hive.raw.users u
            JOIN hive.raw.orders o ON u.id = o.uid
        ) SELECT * FROM c`,
		[]string{"*"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsCreateViewAsWithJoinStarWithMetadata(t *testing.T) {
	assertParseColumns(t, "create_view_as_with_join_star_with_metadata", `
        CREATE VIEW hive.analytics.v AS
        WITH c AS (
            SELECT * FROM hive.raw.users u
            JOIN hive.raw.orders o ON u.id = o.uid
        ) SELECT * FROM c`,
		[]string{"id", "name", "email", "oid", "uid", "amt"},
		WithLineageMetadata(viewColumnsMetadata),
	)
}

func TestParseColumnsInsertSelectUsesSelectParseColumns(t *testing.T) {
	assertParseColumns(t, "insert_select_uses_select_output_columns",
		"INSERT INTO c.s.t SELECT a, b FROM r",
		[]string{"a", "b"},
	)
}

func TestParseColumnsInsertWithTargetColumnList(t *testing.T) {
	assertParseColumns(t, "insert_with_target_column_list",
		"INSERT INTO c.s.t (col1, col2) SELECT a, b FROM r",
		[]string{"col1", "col2"},
	)
}

func TestParseColumnsInsertValuesWithColumnList(t *testing.T) {
	assertParseColumns(t, "insert_values_with_column_list",
		"INSERT INTO c.s.t (a, b) VALUES (1, 2), (3, 4)",
		[]string{"a", "b"},
	)
}

func TestParseColumnsInsertSelectStarWithMetadata(t *testing.T) {
	assertParseColumns(t, "insert_select_star_with_metadata",
		"INSERT INTO c.s.t SELECT * FROM hive.raw.users",
		[]string{"id", "name", "email"},
		WithLineageMetadata(map[string][]string{"hive.raw.users": {"id", "name", "email"}}),
	)
}

func TestParseColumnsInsertValuesWithoutColumnListRaises(t *testing.T) {
	assertParseColumnsError(t, "insert_values_without_column_list_raises",
		"INSERT INTO c.s.t VALUES (1, 2)",
	)
}

func TestParseColumnsDropReturnsNil(t *testing.T) {
	got, err := ParseColumns("DROP TABLE hive.x.t", WithLineageDialect("trino"))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %v; want nil", got)
	}
}