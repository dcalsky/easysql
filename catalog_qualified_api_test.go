package easysql

import (
	"reflect"
	"testing"
)

const catalogQualifiedInteraction = "iceberg.rda_launch_to_engage.interaction"

func TestApplyRowFilterCatalogQualifiedRegexp(t *testing.T) {
	out := bughuntApply(t, "trino", "SELECT * FROM "+catalogQualifiedInteraction,
		WithTableRegexp(`^iceberg\.rda_launch_to_engage\.interaction$`))
	if got := bughuntWraps(t, out, "trino"); got != 1 {
		t.Fatalf("catalog-qualified regexp should wrap the exact table, wraps=%d: %s", got, out)
	}

	out = bughuntApply(t, "trino",
		"SELECT * FROM hive.rda_launch_to_engage.interaction",
		WithTableRegexp(`^iceberg\.rda_launch_to_engage\.interaction$`))
	if got := bughuntWraps(t, out, "trino"); got != 0 {
		t.Fatalf("catalog-qualified regexp must not leak across catalogs, wraps=%d: %s", got, out)
	}
}

func TestBindCTEsCatalogQualifiedTableNames(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`SELECT f.interaction_id, u.name
		 FROM filtered f
		 JOIN hive.rda_launch_to_engage.users u ON f.user_id = u.user_id`,
		[]CTEBinding{{
			Name: "filtered",
			Query: `SELECT interaction_id, user_id
				FROM iceberg.rda_launch_to_engage.interaction
				WHERE active = TRUE`,
		}},
	)

	const want = "WITH filtered AS (SELECT interaction_id, user_id FROM iceberg.rda_launch_to_engage.interaction WHERE active = TRUE) SELECT f.interaction_id, u.name FROM filtered AS f JOIN hive.rda_launch_to_engage.users AS u ON f.user_id = u.user_id"
	if out != want {
		t.Fatalf("catalog-qualified tables changed while binding CTEs:\nwant: %s\n got: %s", want, out)
	}
}

func TestLineageSourceColumnsCatalogQualifiedTableNames(t *testing.T) {
	got, err := LineageSourceColumns(
		"SELECT interaction_id FROM "+catalogQualifiedInteraction,
		WithLineageDialect("trino"),
	)
	if err != nil {
		t.Fatalf("LineageSourceColumns: %v", err)
	}
	want := map[string][]string{
		catalogQualifiedInteraction: {"interaction_id"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog-qualified lineage mismatch:\nwant: %v\n got: %v", want, got)
	}
}

func TestLineageSourceColumnsConcurrentCatalogQualifiedTableNames(t *testing.T) {
	sql := "SELECT interaction_id FROM " + catalogQualifiedInteraction +
		" UNION ALL SELECT interaction_id FROM hive.rda_launch_to_engage.interaction"
	got, err := LineageSourceColumnsConcurrent(sql, WithLineageDialect("trino"))
	if err != nil {
		t.Fatalf("LineageSourceColumnsConcurrent: %v", err)
	}
	want := map[string][]string{
		catalogQualifiedInteraction:             {"interaction_id"},
		"hive.rda_launch_to_engage.interaction": {"interaction_id"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent catalog-qualified lineage mismatch:\nwant: %v\n got: %v", want, got)
	}
}

func TestParseColumnsCatalogQualifiedTableNames(t *testing.T) {
	got, err := ParseColumns(
		"SELECT * FROM "+catalogQualifiedInteraction,
		WithLineageDialect("trino"),
		WithLineageMetadata(map[string][]string{
			catalogQualifiedInteraction:             {"interaction_id", "status"},
			"hive.rda_launch_to_engage.interaction": {"legacy_id"},
		}),
	)
	if err != nil {
		t.Fatalf("ParseColumns: %v", err)
	}
	want := []string{"interaction_id", "status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog-qualified star expanded from the wrong table:\nwant: %v\n got: %v", want, got)
	}
}

const catalogQualifiedReferencesSQL = `
	SELECT
		iceberg.rda_launch_to_engage.interaction.interaction_id,
		hive.rda_launch_to_engage.interaction.legacy_id
	FROM iceberg.rda_launch_to_engage.interaction
	JOIN hive.rda_launch_to_engage.interaction
	  ON iceberg.rda_launch_to_engage.interaction.interaction_id =
	     hive.rda_launch_to_engage.interaction.legacy_id
	WHERE iceberg.rda_launch_to_engage.interaction.status = 'active'
`

func TestReferencedColumnsCatalogQualifiedTableNames(t *testing.T) {
	assertReferencedColumns(t, "catalog_qualified_tables",
		catalogQualifiedReferencesSQL,
		map[string][]string{
			catalogQualifiedInteraction:             {"interaction_id", "status"},
			"hive.rda_launch_to_engage.interaction": {"legacy_id"},
		},
	)
}

func TestReferencedColumnUsagesCatalogQualifiedTableNames(t *testing.T) {
	assertReferencedColumnUsages(t, "catalog_qualified_tables",
		catalogQualifiedReferencesSQL,
		[]ColumnUse{
			{Table: catalogQualifiedInteraction, Column: "interaction_id", Clause: ColumnClauseSelect},
			{Table: catalogQualifiedInteraction, Column: "interaction_id", Clause: ColumnClauseJoinOn},
			{Table: catalogQualifiedInteraction, Column: "status", Clause: ColumnClauseWhere},
			{Table: "hive.rda_launch_to_engage.interaction", Column: "legacy_id", Clause: ColumnClauseSelect},
			{Table: "hive.rda_launch_to_engage.interaction", Column: "legacy_id", Clause: ColumnClauseJoinOn},
		},
	)
}

func TestRewriteTableReferencesCatalogQualifiedTableNames(t *testing.T) {
	specs := []TableRewrite{{
		MatchKey: "rda_launch_to_engage.interaction",
		Inline: &TableRef{
			Catalog: "lakehouse",
			Schema:  "filtered",
			Table:   "interaction",
		},
	}}
	out := rewriteTablesValid(t,
		"SELECT iceberg.rda_launch_to_engage.interaction.interaction_id FROM "+
			catalogQualifiedInteraction,
		specs,
		WithRewriteDialect("trino"),
		StripMatchCatalogs("iceberg"),
	)
	const want = "SELECT interaction.interaction_id FROM (SELECT * FROM lakehouse.filtered.interaction) AS interaction"
	if out != want {
		t.Fatalf("unexpected catalog-qualified rewrite:\nwant: %s\n got: %s", want, out)
	}

	const otherCatalog = "SELECT * FROM hive.rda_launch_to_engage.interaction"
	out = rewriteTablesValid(t, otherCatalog, specs,
		WithRewriteDialect("trino"),
		StripMatchCatalogs("iceberg"),
	)
	if out != otherCatalog {
		t.Fatalf("rewrite leaked to an unconfigured catalog:\nwant: %s\n got: %s", otherCatalog, out)
	}
}
