package easysql

import (
	"errors"
	"testing"
)

func testInlineRewrite() TableRewrite {
	return TableRewrite{
		MatchKey: "myschema.mytable",
		Inline:   &TableRef{Catalog: "cat", Schema: "vsch", Table: "view1"},
	}
}

func testUnionRewrite() TableRewrite {
	return TableRewrite{
		MatchKey: "myschema.mytable",
		Union: &UnionRewrite{
			TableAlias: "mytable",
			Columns:    []string{"col1", "col2"},
			Branches: []TableRef{
				{Catalog: "cat", Schema: "vsch", Table: "view1"},
				{Catalog: "cat", Schema: "vsch", Table: "view2"},
			},
		},
	}
}

func rewriteTablesValid(t *testing.T, sql string, specs []TableRewrite, opts ...RewriteTablesOption) string {
	t.Helper()
	out, err := RewriteTableReferences(sql, specs, opts...)
	if err != nil {
		t.Fatalf("RewriteTableReferences(%q): %v", sql, err)
	}
	if _, err := testClient.ParseOne(out, "trino"); err != nil {
		t.Fatalf("rewritten SQL does not parse:\n in:  %s\n out: %s\n err: %v", sql, out, err)
	}
	return out
}

func TestRewriteTableReferencesInlineLiteralSafe(t *testing.T) {
	input := "SELECT col1 FROM myschema.mytable WHERE note = ' myschema.mytable ' AND txt = 'see vdm_rda.docs' AND spaced = 'a   b'"

	out := rewriteTablesValid(t, input, []TableRewrite{testInlineRewrite()}, WithRewriteDialect("trino"))

	const want = "SELECT col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable WHERE note = ' myschema.mytable ' AND txt = 'see vdm_rda.docs' AND spaced = 'a   b'"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesStripMatchCatalog(t *testing.T) {
	input := `SELECT col1 FROM "vdm_rda".myschema.mytable WHERE col1 = 'a'`

	out := rewriteTablesValid(t, input, []TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
		StripMatchCatalogs("vdm_rda"),
	)

	const want = "SELECT col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable WHERE col1 = 'a'"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionDerivedTableWithExistingWith(t *testing.T) {
	input := "WITH user_cte AS (SELECT 1 AS k) SELECT col1 FROM myschema.mytable WHERE col2 = 'a'"

	out := rewriteTablesValid(t, input, []TableRewrite{testUnionRewrite()}, WithRewriteDialect("trino"))

	const want = "WITH user_cte AS (SELECT 1 AS k) SELECT col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS mytable WHERE col2 = 'a'"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesRepeatedReferences(t *testing.T) {
	input := "SELECT a.col1, b.col1 FROM myschema.mytable AS a JOIN myschema.mytable AS b ON a.id = b.id"

	out := rewriteTablesValid(t, input, []TableRewrite{testInlineRewrite()}, WithRewriteDialect("trino"))

	const want = "SELECT a.col1, b.col1 FROM (SELECT * FROM cat.vsch.view1) AS a JOIN (SELECT * FROM cat.vsch.view1) AS b ON a.id = b.id"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesNoopPreservesInput(t *testing.T) {
	input := "WITH mytable AS (SELECT 1 AS col1) SELECT col1 FROM mytable"

	out, err := RewriteTableReferences(input, []TableRewrite{testInlineRewrite()}, WithRewriteDialect("trino"))
	if err != nil {
		t.Fatalf("RewriteTableReferences noop: %v", err)
	}
	if out != input {
		t.Fatalf("no-op rewrite must preserve input bytes:\n in:  %q\n out: %q", input, out)
	}
}

func TestRewriteTableReferencesRejectsMultipleStatements(t *testing.T) {
	_, err := RewriteTableReferences(
		"SELECT * FROM myschema.mytable; SELECT * FROM myschema.mytable",
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for multiple statements, got %v", err)
	}
}
