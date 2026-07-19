package easysql

import "testing"

func TestRewriteTableReferencesStripsCatalogQualifiedColumnRefs(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT vdm_rda.myschema.mytable.col1 FROM vdm_rda.myschema.mytable`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
		StripMatchCatalogs("vdm_rda"),
	)

	const want = "SELECT mytable.col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionAliasKeepsQualifiedRefsResolvable(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "replacement"

	out := rewriteTablesValid(t,
		`SELECT myschema.mytable.col1 FROM myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT replacement.col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS replacement"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesPreservesPostgresDollarQuotedStrings(t *testing.T) {
	spec := TableRewrite{
		MatchKey: "myschema.mytable",
		Inline:   &TableRef{Schema: "vsch", Table: "view1"},
	}

	out := rewriteTablesValid(t,
		`SELECT col1 FROM myschema.mytable WHERE note = $$not -- a comment$$`,
		[]TableRewrite{spec},
		WithRewriteDialect("postgres"),
	)

	const want = "SELECT col1 FROM (SELECT * FROM vsch.view1) AS mytable WHERE note = 'not -- a comment'"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesDoesNotDuplicateAliasesForSameBareTableName(t *testing.T) {
	specs := []TableRewrite{
		{
			MatchKey: "s1.t",
			Inline:   &TableRef{Schema: "vsch", Table: "view1"},
		},
		{
			MatchKey: "s2.t",
			Inline:   &TableRef{Schema: "vsch", Table: "view2"},
		},
	}

	out := rewriteTablesValid(t,
		`SELECT s1.t.id, s2.t.id FROM s1.t JOIN s2.t ON s1.t.id = s2.t.id`,
		specs,
		WithRewriteDialect("trino"),
	)

	const want = "SELECT s1_t.id, s2_t.id FROM (SELECT * FROM vsch.view1) AS s1_t JOIN (SELECT * FROM vsch.view2) AS s2_t ON s1_t.id = s2_t.id"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesQuotesUnionColumnIdentifiers(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.Columns = []string{"order id"}

	out := rewriteTablesValid(t,
		`SELECT col1 FROM myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = `SELECT col1 FROM (SELECT "order id" FROM cat.vsch.view1 UNION DISTINCT SELECT "order id" FROM cat.vsch.view2) AS mytable`
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUsesDialectIdentifierQuotingForTargets(t *testing.T) {
	spec := TableRewrite{
		MatchKey: "myschema.mytable",
		Inline:   &TableRef{Schema: "vsch", Table: "view-1"},
	}

	out := rewriteTablesValid(t,
		`SELECT col1 FROM myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("mysql"),
	)

	const want = "SELECT col1 FROM (SELECT * FROM vsch.`view-1`) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesCTEDoesNotShadowQualifiedPhysicalTable(t *testing.T) {
	spec := TableRewrite{
		MatchKey: "myschema.mytable",
		Inline:   &TableRef{Schema: "vsch", Table: "view1"},
	}

	out := rewriteTablesValid(t,
		`WITH mytable AS (SELECT col1 FROM other_table)
         SELECT mytable.col1, myschema.mytable.col1
         FROM mytable
         JOIN myschema.mytable ON mytable.col1 = myschema.mytable.col1`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "WITH mytable AS (SELECT col1 FROM other_table) SELECT mytable.col1, myschema_mytable.col1 FROM mytable JOIN (SELECT * FROM vsch.view1) AS myschema_mytable ON mytable.col1 = myschema_mytable.col1"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

// TestRewriteTableReferencesUnusedCTEDoesNotBlockBareRebind is the companion of
// TestRewriteTableReferencesCTEDoesNotShadowQualifiedPhysicalTable for the case
// where the same-named CTE is DEFINED but never referenced in FROM. Per SQL
// scoping ("for a CTE to be visible it must contain the query"), an unused CTE
// binds no range variable, so bare `mytable.col1` originally resolves to the
// physical table's implicit alias `mytable`. After the physical table is
// disambiguated to myschema_mytable (its bare name is reserved to avoid a
// collision with the CTE name), the bare refs MUST follow the table to
// myschema_mytable — leaving them as `mytable.col1` would dangle (the CTE is not
// in FROM) and silently change which relation the query reads.
func TestRewriteTableReferencesUnusedCTEDoesNotBlockBareRebind(t *testing.T) {
	out := rewriteTablesValid(t,
		`WITH mytable AS (SELECT 999 AS col1)
         SELECT mytable.col1 FROM myschema.mytable WHERE mytable.col1 = 'a'`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "WITH mytable AS (SELECT 999 AS col1) SELECT myschema_mytable.col1 FROM (SELECT * FROM cat.vsch.view1) AS myschema_mytable WHERE myschema_mytable.col1 = 'a'"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesAmbiguousOuterBareQualifierDoesNotBreakInnerAlias(t *testing.T) {
	specs := []TableRewrite{
		{MatchKey: "s1.t", Inline: &TableRef{Schema: "vsch", Table: "view1"}},
		{MatchKey: "s2.t", Inline: &TableRef{Schema: "vsch", Table: "view2"}},
	}

	out := rewriteTablesValid(t,
		`SELECT (SELECT t.id FROM other_table t) AS inner_id, s1.t.id
         FROM s1.t JOIN s2.t ON s1.t.id = s2.t.id`,
		specs,
		WithRewriteDialect("trino"),
	)

	const want = "SELECT (SELECT t.id FROM other_table AS t) AS inner_id, s1_t.id FROM (SELECT * FROM vsch.view1) AS s1_t JOIN (SELECT * FROM vsch.view2) AS s2_t ON s1_t.id = s2_t.id"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesNestedAliasDoesNotCaptureOuterRebind(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT (SELECT mytable.id FROM other_table mytable) AS inner_id, myschema.mytable.id
         FROM myschema.mytable`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT (SELECT mytable.id FROM other_table AS mytable) AS inner_id, mytable.id FROM (SELECT * FROM cat.vsch.view1) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesProjectionAliasDoesNotRenameTableAlias(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT mytable.col1 AS x, 1 AS mytable FROM myschema.mytable`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT mytable.col1 AS x, 1 AS mytable FROM (SELECT * FROM cat.vsch.view1) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesNestedAliasDoesNotRenameOuterTableAlias(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT mytable.col1, (SELECT 1 FROM other_table mytable) AS inner_value FROM myschema.mytable`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT mytable.col1, (SELECT 1 FROM other_table AS mytable) AS inner_value FROM (SELECT * FROM cat.vsch.view1) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionAliasKeepsBareRefsResolvable(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "replacement"

	out := rewriteTablesValid(t,
		`SELECT mytable.col1 FROM myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT replacement.col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS replacement"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesQualifiedStarRebindsToDerivedAlias(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		opts []RewriteTablesOption
	}{
		{
			name: "schema qualified",
			sql:  `SELECT myschema.mytable.* FROM myschema.mytable`,
			opts: []RewriteTablesOption{WithRewriteDialect("trino")},
		},
		{
			name: "catalog qualified",
			sql:  `SELECT vdm_rda.myschema.mytable.* FROM vdm_rda.myschema.mytable`,
			opts: []RewriteTablesOption{
				WithRewriteDialect("trino"),
				StripMatchCatalogs("vdm_rda"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := rewriteTablesValid(t, tt.sql, []TableRewrite{testInlineRewrite()}, tt.opts...)
			const want = "SELECT mytable.* FROM (SELECT * FROM cat.vsch.view1) AS mytable"
			if out != want {
				t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
			}
		})
	}
}

func TestRewriteTableReferencesUnionAllBranchQualifiedColumnRefsRebind(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT myschema.mytable.col1 FROM myschema.mytable UNION ALL SELECT col1 FROM other_table`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT mytable.col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable UNION ALL SELECT col1 FROM other_table"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionAllBranchQualifiedStarRebinds(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT myschema.mytable.* FROM myschema.mytable UNION ALL SELECT col1 FROM other_table`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT mytable.* FROM (SELECT * FROM cat.vsch.view1) AS mytable UNION ALL SELECT col1 FROM other_table"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesExceptBranchWhereClauseRebinds(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT col1 FROM myschema.mytable WHERE myschema.mytable.col2 > 1 EXCEPT SELECT col1 FROM other_table`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable WHERE mytable.col2 > 1 EXCEPT SELECT col1 FROM other_table"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionAllRightBranchQualifiedColumnRefsRebind(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT col1 FROM other_table UNION ALL SELECT myschema.mytable.col1 FROM myschema.mytable`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT col1 FROM other_table UNION ALL SELECT mytable.col1 FROM (SELECT * FROM cat.vsch.view1) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionRenamedBareStarInSetOperationBranchRebinds(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "replacement"

	out := rewriteTablesValid(t,
		`SELECT mytable.* FROM myschema.mytable UNION ALL SELECT col1 FROM other_table`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT replacement.* FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS replacement UNION ALL SELECT col1 FROM other_table"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func testSameNameRewriteSpecs() []TableRewrite {
	return []TableRewrite{
		{MatchKey: "s1.t", Inline: &TableRef{Schema: "vsch", Table: "view1"}},
		{MatchKey: "s2.t", Inline: &TableRef{Schema: "vsch", Table: "view2"}},
	}
}

func TestRewriteTableReferencesBareRebindIsScopedToEachQuery(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "subquery in where",
			sql:  `SELECT t.a FROM s1.t WHERE t.b IN (SELECT t.c FROM s2.t)`,
			want: "SELECT s1_t.a FROM (SELECT * FROM vsch.view1) AS s1_t WHERE s1_t.b IN (SELECT s2_t.c FROM (SELECT * FROM vsch.view2) AS s2_t)",
		},
		{
			name: "subquery in projection",
			sql:  `SELECT (SELECT t.c FROM s2.t) AS v, t.a FROM s1.t`,
			want: "SELECT (SELECT s2_t.c FROM (SELECT * FROM vsch.view2) AS s2_t) AS v, s1_t.a FROM (SELECT * FROM vsch.view1) AS s1_t",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := rewriteTablesValid(t, tt.sql, testSameNameRewriteSpecs(), WithRewriteDialect("trino"))
			if out != tt.want {
				t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", tt.want, out)
			}
		})
	}
}

func TestRewriteTableReferencesQuotedDotStarIsOneIdentifier(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT "s1.t".*, s1.t.a FROM "s1.t", s1.t`,
		testSameNameRewriteSpecs(),
		WithRewriteDialect("trino"),
	)

	const want = `SELECT "s1.t".*, t.a FROM "s1.t", (SELECT * FROM vsch.view1) AS t`
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesRowFieldAccessFollowsRenamedTable(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "replacement"

	out := rewriteTablesValid(t,
		`SELECT mytable.col1.f FROM myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT replacement.col1.f FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS replacement"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionAliasAvoidsExistingDerivedAlias(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "replacement"

	out := rewriteTablesValid(t,
		`SELECT mytable.col1 FROM myschema.mytable, (SELECT 1 AS c) AS replacement`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT myschema_mytable.col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS myschema_mytable, (SELECT 1 AS c) AS replacement"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesPreservesAliasColumnList(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT a.x FROM myschema.mytable AS a (x)`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT a.x FROM (SELECT * FROM cat.vsch.view1) AS a(x)"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesPreservesTableSample(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT col1 FROM myschema.mytable TABLESAMPLE BERNOULLI (10)`,
		[]TableRewrite{testInlineRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT col1 FROM (SELECT * FROM cat.vsch.view1 TABLESAMPLE BERNOULLI (10)) AS mytable"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesQuotesReservedIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		spec TableRewrite
		want string
	}{
		{
			name: "inline target",
			spec: TableRewrite{
				MatchKey: "myschema.mytable",
				Inline:   &TableRef{Schema: "vsch", Table: "order"},
			},
			want: `SELECT col1 FROM (SELECT * FROM vsch."order") AS mytable`,
		},
		{
			name: "union column",
			spec: func() TableRewrite {
				spec := testUnionRewrite()
				spec.Union.Columns = []string{"order"}
				return spec
			}(),
			want: `SELECT col1 FROM (SELECT "order" FROM cat.vsch.view1 UNION DISTINCT SELECT "order" FROM cat.vsch.view2) AS mytable`,
		},
		{
			name: "union alias",
			spec: func() TableRewrite {
				spec := testUnionRewrite()
				spec.Union.TableAlias = "order"
				return spec
			}(),
			want: `SELECT col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS "order"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := rewriteTablesValid(t,
				`SELECT col1 FROM myschema.mytable`,
				[]TableRewrite{tt.spec},
				WithRewriteDialect("trino"),
			)
			if out != tt.want {
				t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", tt.want, out)
			}
		})
	}
}

func TestRewriteTableReferencesCaseDistinctPostgresTablesGetDistinctAliases(t *testing.T) {
	spec := TableRewrite{
		MatchKey: "myschema.mytable",
		Inline:   &TableRef{Schema: "vsch", Table: "view1"},
	}
	out := rewriteTablesValid(t,
		`SELECT myschema."MyTable".a, myschema.mytable.b FROM myschema."MyTable", myschema.mytable`,
		[]TableRewrite{spec},
		WithRewriteDialect("postgres"),
	)

	const want = `SELECT myschema_MyTable.a, mytable_2.b FROM (SELECT * FROM vsch.view1) AS myschema_MyTable, (SELECT * FROM vsch.view1) AS mytable_2`
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnnestAliasOccupiesRelationName(t *testing.T) {
	spec := testUnionRewrite()
	spec.Union.TableAlias = "t"

	out := rewriteTablesValid(t,
		`SELECT t.x, mytable.col1 FROM myschema.mytable, UNNEST(ARRAY[1]) AS t (x)`,
		[]TableRewrite{spec},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT t.x, myschema_mytable.col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS myschema_mytable, UNNEST(ARRAY[1]) AS t(x)"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}

func TestRewriteTableReferencesUnionKeepsExplicitAlias(t *testing.T) {
	out := rewriteTablesValid(t,
		`SELECT a.col1 FROM myschema.mytable AS a`,
		[]TableRewrite{testUnionRewrite()},
		WithRewriteDialect("trino"),
	)

	const want = "SELECT a.col1 FROM (SELECT col1, col2 FROM cat.vsch.view1 UNION DISTINCT SELECT col1, col2 FROM cat.vsch.view2) AS a"
	if out != want {
		t.Fatalf("unexpected rewrite:\nwant: %s\n got: %s", want, out)
	}
}
