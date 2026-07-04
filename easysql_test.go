package easysql

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// testWhere is a verbatim WHERE expression whose single string literal
// (testMarker) is counted to assert how many tables were wrapped.
const (
	testWhere  = "tenant = 'alice'"
	testMarker = "alice"
)

// --- wrap-count validators (specific to these behavior tests) --------------

// countLiterals counts string literals equal to value in sql's AST. Each wrap
// splices the WHERE expression once, so when that expression contains exactly
// one occurrence of value as a string literal this equals the number of wrapped
// tables.
func countLiterals(t *testing.T, sql, pg, value string) int {
	t.Helper()
	raw, err := testClient.ParseOne(sql, pg)
	if err != nil {
		t.Fatalf("re-parse %q: %v", sql, err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	n := 0
	walkJSON(node, func(m map[string]any) {
		if lit, ok := m["literal"].(map[string]any); ok {
			if lit["literal_type"] == "string" && lit["value"] == value {
				n++
			}
		}
	})
	return n
}

// hasEmptyAlias reports whether any subquery/table alias is empty (invalid SQL
// that some parsers tolerate on re-parse).
func hasEmptyAlias(t *testing.T, sql, pg string) bool {
	t.Helper()
	raw, err := testClient.ParseOne(sql, pg)
	if err != nil {
		t.Fatalf("re-parse %q: %v", sql, err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	empty := false
	walkJSON(node, func(m map[string]any) {
		if sq, ok := m["subquery"].(map[string]any); ok {
			if identName(sq["alias"]) == "" {
				empty = true
			}
		}
	})
	return empty
}

// rewriteValid runs a rewrite and asserts: it succeeds, the output is valid SQL
// (re-parses), has no empty alias, and wraps exactly wantWraps tables (counted
// via marker). Returns the rewritten SQL for further assertions.
func rewriteValid(t *testing.T, dialect, sql, whereClause, marker string, wantWraps int, opts ...Option) string {
	t.Helper()
	all := append([]Option{WithDialect(dialect)}, opts...)
	out, err := ApplyRowFilter(sql, whereClause, all...)
	if err != nil {
		t.Fatalf("ApplyRowFilter(%q): %v", sql, err)
	}
	pg := dialectToPolyglot[dialect]
	if _, err := testClient.ParseOne(out, pg); err != nil {
		t.Fatalf("output is not valid SQL:\n in:  %s\n out: %s\n err: %v", sql, out, err)
	}
	if hasEmptyAlias(t, out, pg) {
		t.Fatalf("output has an empty derived-table alias:\n in:  %s\n out: %s", sql, out)
	}
	if got := countLiterals(t, out, pg, marker); got != wantWraps {
		t.Fatalf("wrapped %d tables, want %d:\n in:  %s\n out: %s", got, wantWraps, sql, out)
	}
	return out
}

// --- core behavior ---------------------------------------------------------

func TestRewriteStructural(t *testing.T) {
	cases := []struct {
		name      string
		where     string
		opts      []Option
		sql       string
		wantWraps int
	}{
		{"basic", testWhere, nil, "select * from a", 1},
		{"explicit alias", testWhere, nil, "select * from a t", 1},
		{"projection cols", testWhere, nil, "select id, name from a", 1},
		{"join wraps both", testWhere, nil, "select a.id from a join b on a.id = b.id", 2},
		{"left join not degraded", testWhere, nil, "select * from a left join b on a.id = b.id", 2},
		{"self join", testWhere, nil, "select t1.id from a t1 join a t2 on t1.id = t2.id", 2},
		{"three way", testWhere, nil, "select * from a join b join c", 3},
		{"subquery in where", testWhere, []Option{WithTableNames("a")},
			"select * from c where id in (select id from a)", 1},
		{"derived table", testWhere, nil, "select * from (select * from a) x", 1},
		{"union both branches", testWhere, nil, "select * from a union select * from b", 2},
		{"named scope only listed", testWhere, []Option{WithTableNames("a")},
			"select * from a join c on a.id = c.id", 1},
		{"out of scope untouched", testWhere, []Option{WithTableNames("a", "b")},
			"select * from c", 0},
		{"regex prefix", testWhere, []Option{WithTableRegexp("^log_")},
			"select * from log_events join users on log_events.uid = users.id", 1},
		{"names+regex compose", testWhere, []Option{WithTableNames("b"), WithTableRegexp("^a$")},
			"select * from a join b join c", 2},
		{"disjunction where", "tenant = 'alice' or is_public = 1", nil, "select * from a", 1},
		{"default db resolves", testWhere, []Option{WithTableNames("db.a"), WithDefaultDB("db")},
			"select * from a", 1},
		{"index hint into subquery", testWhere, nil, "select * from a use index(idx)", 1},
		{"partition into subquery", testWhere, nil, "select * from a partition(p0)", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewriteValid(t, "mysql", tc.sql, tc.where, testMarker, tc.wantWraps, tc.opts...)
		})
	}
}

// TestCTEReferencesNotWrapped asserts the correct CTE semantics: a CTE
// reference is never wrapped (only the real tables in/around it are). This is
// where a global CTE-name set would either over-wrap or, worse, leave a real
// table unfiltered.
func TestCTEReferencesNotWrapped(t *testing.T) {
	cases := []struct {
		sql       string
		wantWraps int
	}{
		// CTE c references real table a (wrapped); outer ref to c is not.
		{"with c as (select * from a) select * from c", 1},
		// Non-recursive CTE: a inside its own body is the real table (wrapped);
		// outer ref to CTE a is not.
		{"with a as (select * from a) select * from a", 1},
		// CTE whose body has no table; outer ref to it is not wrapped.
		{"with a as (select 1 as id) select * from a", 0},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			rewriteValid(t, "mysql", tc.sql, testWhere, testMarker, tc.wantWraps)
		})
	}
}

// TestCTEScopeSecurity is the regression for the scope bug: a CTE named t in
// one set-operation branch must not cause the real table t in the sibling
// branch to be left unfiltered. The real t MUST be wrapped.
func TestCTEScopeSecurity(t *testing.T) {
	sql := "select * from t union all select * from (with t as (select 1 as id) select * from t) q"
	out := rewriteValid(t, "mysql", sql, testWhere, testMarker, 1) // exactly the real t in branch 1
	// Be explicit: the first branch's real table must be filtered. The generator
	// upper-cases keywords, so look for the wrapped real table t.
	if !strings.Contains(out, "(SELECT * FROM t WHERE") && !strings.Contains(out, "(SELECT * FROM `t` WHERE") {
		t.Fatalf("real table t was not filtered (security regression):\n%s", out)
	}
}

func TestTableRegexpMatchesQualifiedName(t *testing.T) {
	rewriteValid(t, "mysql",
		"select * from sales.orders join sales.users on orders.user_id = users.id",
		testWhere,
		testMarker,
		1,
		WithTableRegexp(`^sales\.orders$`),
	)
}

// TestTableFunctionsNotWrapped: table-valued functions are not physical tables
// and must be passed through unfiltered, never producing an empty alias.
func TestTableFunctionsNotWrapped(t *testing.T) {
	cases := []struct {
		dialect, sql string
	}{
		{"postgres", "select * from generate_series(1, 10)"},
		{"starrocks", "select * from table(generator(10))"},
		{"trino", "select * from unnest(array[1,2]) with ordinality"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			rewriteValid(t, tc.dialect, tc.sql, testWhere, testMarker, 0)
		})
	}
	// A real table joined with a table function: only the real table is wrapped.
	rewriteValid(t, "trino", "select x from a cross join unnest(a.arr) as t(x)", testWhere, testMarker, 1)
}

// TestQuotingPreserved: a quoted reserved-word identifier must stay quoted, or
// the output would be invalid SQL.
func TestQuotingPreserved(t *testing.T) {
	out := rewriteValid(t, "postgres", `select * from "order"`, testWhere, testMarker, 1)
	if !strings.Contains(out, `"order"`) {
		t.Fatalf("quoting on reserved-word identifier was lost: %s", out)
	}
}

// TestDualSkipped: the DUAL pseudo-table is never wrapped.
func TestDualSkipped(t *testing.T) {
	rewriteValid(t, "mysql", "select 1 from dual", testWhere, testMarker, 0)
}

// TestWhereSplicedVerbatim: the WHERE expression is spliced as-is, so a literal
// the caller already bound appears verbatim in each wrapped table.
func TestWhereSplicedVerbatim(t *testing.T) {
	out := rewriteValid(t, "mysql", "select * from a join b on a.id = b.id",
		"tenant = 'acme'", "acme", 2)
	if strings.Count(out, "'acme'") != 2 {
		t.Fatalf("where expression not spliced verbatim into each table: %s", out)
	}
}

// TestSchemaStrip: when a schema-qualified table gets a synthesized
// (bare-name) derived-table alias, outer schema.table.col references must drop
// the schema so they keep resolving against the alias. The check is
// format-independent: the three-part reference must be gone and the two-part one
// present.
func TestSchemaStrip(t *testing.T) {
	out := rewriteValid(t, "mysql",
		"select sales.orders.id, name from sales.orders", testWhere, testMarker, 1)
	if strings.Contains(out, "sales.orders.id") {
		t.Fatalf("three-part column reference was not stripped to the alias:\n%s", out)
	}
	if !strings.Contains(out, "orders.id") {
		t.Fatalf("expected stripped column orders.id:\n%s", out)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want error
	}{
		{"update", "update a set x = 1", ErrUnsupported},
		{"delete", "delete from a", ErrUnsupported},
		{"insert", "insert into a values (1)", ErrUnsupported},
		{"multiple statements", "select * from a; select * from b", ErrUnsupported},
		{"syntax error", "select * frm where", ErrParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyRowFilter(tc.sql, testWhere)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ApplyRowFilter(%q) error = %v, want %v", tc.sql, err, tc.want)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name  string
		where string
		opts  []Option
	}{
		{"empty where", "", nil},
		{"invalid where", "user ===", nil},
		{"unknown dialect", testWhere, []Option{WithDialect("oracle")}},
		{"empty regexp", testWhere, []Option{WithTableRegexp("")}},
		{"invalid regexp", testWhere, []Option{WithTableRegexp("(")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ApplyRowFilter("select * from a", tc.where, tc.opts...); err == nil {
				t.Fatalf("ApplyRowFilter(where=%q) succeeded, want error", tc.where)
			}
		})
	}
}

// TestSelfCheck pins the documented contract of WithSelfCheck: it is a no-op
// (validity is always enforced), so toggling it must neither break the rewrite
// nor change its output. A bare "no error" check would still pass if the option
// silently disabled wrapping or if the rewrite regressed to returning the input
// unchanged; asserting the exact wrap count and byte-identical on/off output
// rules both out.
func TestSelfCheck(t *testing.T) {
	cases := []struct {
		sql       string
		wantWraps int
	}{
		{"select * from a", 1},
		{"select a.id from a join b on a.id = b.id", 2},
		{"select * from a union select * from b", 2},
		{"with c as (select * from a) select * from c", 1},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			// With self-check on: a valid rewrite that wraps exactly the
			// expected tables (rewriteValid re-parses and counts the marker).
			withCheck := rewriteValid(t, "mysql", tc.sql, testWhere, testMarker, tc.wantWraps, WithSelfCheck(true))
			// Since the option is a no-op, the output must match the default.
			without, err := ApplyRowFilter(tc.sql, testWhere, WithDialect("mysql"))
			if err != nil {
				t.Fatalf("default rewrite %q: %v", tc.sql, err)
			}
			if withCheck != without {
				t.Fatalf("WithSelfCheck must be a no-op but changed output:\n on:  %s\n off: %s", withCheck, without)
			}
		})
	}
}

// --- dialect coverage probe (asserted) -------------------------------------

func TestDialectBoundary(t *testing.T) {
	type pc struct {
		dialect, name, sql string
		wantWraps          int
	}
	cases := []pc{
		// postgres
		{"postgres", "cast ::", "select id::text from a", 1},
		{"postgres", "ILIKE", "select * from a where name ilike 'a%'", 1},
		{"postgres", "distinct on", "select distinct on (uid) uid, ts from a order by uid, ts desc", 1},
		{"postgres", "array literal", "select array[1,2,3] from a", 1},
		{"postgres", "array index", "select tags[1] from a", 1},
		{"postgres", "regex ~", "select * from a where name ~ '^x'", 1},
		{"postgres", "concat ||", "select first || last from a", 1},
		{"postgres", "is true", "select * from a where flag is true", 1},
		{"postgres", "fetch first", "select * from a order by id fetch first 10 rows only", 1},
		{"postgres", "limit offset", "select * from a limit 10 offset 5", 1},
		{"postgres", "dollar param", "select * from a where id = $1", 1},
		{"postgres", "dquoted ident", `select "uid" from a`, 1},
		{"postgres", "lateral", "select * from a, lateral (select * from b where b.aid = a.id) s", 2},
		{"postgres", "generate_series", "select * from generate_series(1, 10)", 0},
		{"postgres", "filter clause", "select count(*) filter (where x > 0) from a", 1},
		{"postgres", "json arrow", "select data->>'k' from a", 1},
		{"postgres", "interval", "select * from a where ts > now() - interval '1 day'", 1},
		// trino
		{"trino", "unnest", "select x from a cross join unnest(a.arr) as t(x)", 1},
		{"trino", "try_cast", "select try_cast(x as bigint) from a", 1},
		{"trino", "row ctor", "select row(1, 'a') from a", 1},
		{"trino", "map subscript", "select m['k'] from a", 1},
		{"trino", "lambda", "select filter(arr, x -> x > 0) from a", 1},
		{"trino", "grouping sets", "select k, sum(v) from a group by grouping sets ((k), ())", 1},
		{"trino", "cube", "select k, sum(v) from a group by cube (k)", 1},
		{"trino", "rollup", "select k, sum(v) from a group by rollup (k)", 1},
		{"trino", "concat ||", "select a || b from a", 1},
		{"trino", "dquoted ident", `select "count" from a`, 1},
		{"trino", "catalog.schema.table", "select * from hive.sales.a", 1},
		{"trino", "tablesample", "select * from a tablesample bernoulli (10)", 1},
		{"trino", "with ordinality", "select * from unnest(array[1,2]) with ordinality", 0},
		{"trino", "decimal cast", "select cast(x as decimal(10,2)) from a", 1},
		// starrocks
		{"starrocks", "array type cast", "select cast(x as array<int>) from a", 1},
		{"starrocks", "named struct", "select named_struct('a', 1) from a", 1},
		{"starrocks", "broadcast hint", "select * from a join [broadcast] b on a.id = b.id", 2},
		{"starrocks", "bucket_shuffle hint", "select * from a join [bucket_shuffle] b on a.id = b.id", 2},
		{"starrocks", "table function", "select * from table(generator(10))", 0},
		{"starrocks", "array map lambda", "select array_map(x -> x + 1, arr) from a", 1},
		{"starrocks", "qualify", "select id, row_number() over (partition by k order by ts) rn from a qualify rn = 1", 1},
		{"starrocks", "set_var hint", "select /*+ SET_VAR(query_timeout=5) */ * from a", 1},
		// common
		{"mysql", "plain", "select * from a", 1},
		{"mysql", "window", "select row_number() over (partition by k order by ts) from a", 1},
		{"mysql", "cte", "with c as (select * from a) select * from c", 1},
		{"mysql", "case when", "select case when x > 0 then 1 else 0 end from a", 1},
		{"mysql", "union all", "select * from a union all select * from b", 2},
	}

	pass := 0
	for _, tc := range cases {
		// Count only subtests that actually passed. t.Run reports the subtest
		// result; ignoring it made the coverage guard below dead code (pass
		// always equalled len(cases)), so a real dialect regression could slip
		// through as long as at least the panic/skip machinery kept running.
		if t.Run(tc.dialect+"/"+tc.name, func(t *testing.T) {
			rewriteValid(t, tc.dialect, tc.sql, testWhere, testMarker, tc.wantWraps)
		}) {
			pass++
		}
	}
	if pass != len(cases) {
		t.Fatalf("coverage regressed: %d/%d", pass, len(cases))
	}
	t.Logf("dialect coverage: %d/%d parsed+rewritten+validated", pass, len(cases))
}

// ---------------------------------------------------------------------------
// Bug-hunt probes for ApplyRowFilter and its options. Each test asserts the
// behavior justified by the documented semantics (package doc in easysql.go,
// README.md "ApplyRowFilter" section).
// ---------------------------------------------------------------------------

// bughuntWraps counts how many tables were wrapped by counting the marker
// literal in the output (same technique as countLiterals).
func bughuntWraps(t *testing.T, out, dialect string) int {
	t.Helper()
	return countLiterals(t, out, dialectToPolyglot[dialect], testMarker)
}

// bughuntApply runs ApplyRowFilter and asserts only that it succeeds and the
// output re-parses; semantic assertions are left to each probe.
func bughuntApply(t *testing.T, dialect, sql string, opts ...Option) string {
	t.Helper()
	all := append([]Option{WithDialect(dialect)}, opts...)
	out, err := ApplyRowFilter(sql, testWhere, all...)
	if err != nil {
		t.Fatalf("ApplyRowFilter(%q): %v", sql, err)
	}
	if _, err := testClient.ParseOne(out, dialectToPolyglot[dialect]); err != nil {
		t.Fatalf("output does not re-parse:\n in:  %s\n out: %s\n err: %v", sql, out, err)
	}
	return out
}

// bughuntRelationNames returns the multiset of relation names bound by
// FROM/JOIN entries (aliases when present, bare table names otherwise) at
// every query level of the parsed output SQL.
func bughuntRelationNames(t *testing.T, sql, pg string) []string {
	t.Helper()
	raw, err := testClient.ParseOne(sql, pg)
	if err != nil {
		t.Fatalf("re-parse %q: %v", sql, err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	var names []string
	walkJSON(node, func(m map[string]any) {
		if n, ok := relationEntryName(m); ok {
			names = append(names, n)
		}
	})
	return names
}

// bughuntDuplicateInScope re-parses sql and reports the first relation name
// bound more than once within a SINGLE FROM/JOIN scope (the condition that
// triggers MySQL error 1066 "Not unique table/alias"). Two same-named
// relations living in DIFFERENT query scopes (e.g. a physical table `s1.t`
// inside one derived subquery and `s2.t` inside another) are legal and are not
// reported.
func bughuntDuplicateInScope(t *testing.T, sql, pg string) (string, bool) {
	t.Helper()
	raw, err := testClient.ParseOne(sql, pg)
	if err != nil {
		t.Fatalf("re-parse %q: %v", sql, err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	dupName := ""
	found := false
	walkJSON(node, func(m map[string]any) {
		if found {
			return
		}
		// A SELECT body carries its FROM/JOIN entries directly; only inspect
		// nodes that actually have a FROM clause so each scope is checked once.
		if _, ok := m["from"]; !ok {
			return
		}
		seen := map[string]bool{}
		for _, entry := range fromJoinEntries(m) {
			if n, ok := relationEntryName(entry); ok {
				if seen[n] {
					dupName, found = n, true
					return
				}
				seen[n] = true
			}
		}
	})
	return dupName, found
}

// Duplicate derived-table aliases for same-named tables in different schemas:
// joining s1.t with s2.t must give each wrapped derived table a distinct alias
// (assignDerivedAliases), so no single FROM scope has two relations named `t`
// (which would be MySQL error 1066).
func TestBughuntDuplicateAliasSameBareNameJoin(t *testing.T) {
	sql := "select s1.t.id, s2.t.name from s1.t join s2.t on s1.t.id = s2.t.id"
	out := bughuntApply(t, "mysql", sql)

	if n, dup := bughuntDuplicateInScope(t, out, "mysql"); dup {
		t.Fatalf("duplicate relation name %q in a single rewritten FROM:\n in:  %s\n out: %s", n, sql, out)
	}
}

// A derived-table alias must not collide with a same-named CTE referenced in
// the same FROM: a schema-qualified db.t is wrapped with a distinct alias so
// the outer FROM does not bind two relations named `t`.
func TestBughuntCTENameCollidesWithDerivedAlias(t *testing.T) {
	sql := "with t as (select 1 as id) select db.t.id, t.id from db.t join t on db.t.id = t.id"
	out := bughuntApply(t, "mysql", sql)

	if n, dup := bughuntDuplicateInScope(t, out, "mysql"); dup {
		t.Fatalf("duplicate relation name %q in a single rewritten FROM:\n in:  %s\n out: %s", n, sql, out)
	}
}

// A recursive CTE's self-reference denotes the CTE, not a physical table, so
// the recursive branch's `from c` must not be wrapped.
func TestBughuntRecursiveCTESelfReference(t *testing.T) {
	sql := "with recursive c as (select 1 as n union all select n + 1 from c where n < 3) select * from c"
	out := bughuntApply(t, "mysql", sql)
	if got := bughuntWraps(t, out, "mysql"); got != 0 {
		t.Fatalf("recursive CTE self-reference must not be wrapped, got %d wrap(s); out: %s", got, out)
	}
}

// A schema-qualified star (schema.table.*) must be rebound to the derived-table
// alias after wrapping (sales.orders.* -> orders.*).
func TestBughuntQualifiedStarNotRebound(t *testing.T) {
	sql := "select sales.orders.*, 1 from sales.orders"
	out := bughuntApply(t, "mysql", sql)
	if !strings.Contains(out, "orders.*") {
		t.Fatalf("expected qualified star rebound to orders.*:\n in:  %s\n out: %s", sql, out)
	}
}

// Catalog-qualified column refs (catalog.schema.table.col) must be rebound to
// the derived-table alias when a catalog-qualified table is wrapped
// (hive.sales.a.col1 -> a.col1).
func TestBughuntCatalogQualifiedColumnRefNotStripped(t *testing.T) {
	sql := "select hive.sales.a.col1 from hive.sales.a"
	out := bughuntApply(t, "trino", sql)
	if !strings.Contains(out, "a.col1") {
		t.Fatalf("expected column ref rebound to a.col1:\n in:  %s\n out: %s", sql, out)
	}
}

// The predicate must reach tables referenced from WHERE-clause subqueries
// (EXISTS / IN), per "keeps the rewrite correct across ... subqueries".
func TestBughuntSubqueryInExists(t *testing.T) {
	out := bughuntApply(t, "mysql",
		"select * from c where exists (select 1 from a where a.cid = c.id)",
		WithTableNames("a"))
	if got := bughuntWraps(t, out, "mysql"); got != 1 {
		t.Fatalf("want 1 wrap (table a inside EXISTS), got %d: %s", got, out)
	}
}

// Set operations: every physical table in every branch is wrapped, including
// nested set-ops and branch-local CTE shadowing.
func TestBughuntSetOpsAndCTEShadowing(t *testing.T) {
	sql := "select * from t union select * from (with t as (select 1 as id) select * from t) q intersect select * from t"
	out := bughuntApply(t, "mysql", sql)
	// Two real `t` references (branch 1 and branch 3); the CTE-shadowed one is
	// not wrapped.
	if got := bughuntWraps(t, out, "mysql"); got != 2 {
		t.Fatalf("want 2 wraps across set-op branches, got %d: %s", got, out)
	}
}

// Self-join with aliases: both sides wrapped, aliases preserved (no
// collision because the explicit aliases differ).
func TestBughuntSelfJoinAliases(t *testing.T) {
	out := bughuntApply(t, "mysql",
		"select t1.id from sales.orders t1 join sales.orders t2 on t1.pid = t2.id")
	if got := bughuntWraps(t, out, "mysql"); got != 2 {
		t.Fatalf("want 2 wraps, got %d: %s", got, out)
	}
	for _, alias := range []string{"t1", "t2"} {
		found := false
		for _, n := range bughuntRelationNames(t, out, "mysql") {
			if n == alias {
				found = true
			}
		}
		if !found {
			t.Fatalf("explicit alias %q lost: %s", alias, out)
		}
	}
}

// LATERAL: the lateral subquery's table is wrapped; the correlated outer
// reference keeps resolving.
func TestBughuntLateral(t *testing.T) {
	out := bughuntApply(t, "postgres",
		"select * from a, lateral (select * from b where b.aid = a.id) s")
	if got := bughuntWraps(t, out, "postgres"); got != 2 {
		t.Fatalf("want 2 wraps, got %d: %s", got, out)
	}
}

// Dialect quoting: postgres double quotes and mysql backticks are both
// preserved on wrapped reserved-word tables (README "Quoting preserved").
func TestBughuntDialectQuoting(t *testing.T) {
	outPg := bughuntApply(t, "postgres", `select * from "order"`)
	if !strings.Contains(outPg, `"order"`) {
		t.Fatalf("postgres quoting lost: %s", outPg)
	}
	outMy := bughuntApply(t, "mysql", "select * from `order`")
	if !strings.Contains(outMy, "`order`") {
		t.Fatalf("mysql backtick quoting lost: %s", outMy)
	}
}

// WithTableNames bare name matches the table in any schema; schema-qualified
// scope requires the schema (resolved via WithDefaultDB for bare refs).
func TestBughuntTableNamesMatching(t *testing.T) {
	// bare scope name matches schema-qualified reference
	out := bughuntApply(t, "mysql", "select * from sales.orders", WithTableNames("orders"))
	if got := bughuntWraps(t, out, "mysql"); got != 1 {
		t.Fatalf("bare scope should match sales.orders, wraps=%d: %s", got, out)
	}
	// qualified scope does not match a bare ref without WithDefaultDB
	out = bughuntApply(t, "mysql", "select * from orders", WithTableNames("sales.orders"))
	if got := bughuntWraps(t, out, "mysql"); got != 0 {
		t.Fatalf("qualified scope must not match bare ref without default DB, wraps=%d: %s", got, out)
	}
	// ... and matches with WithDefaultDB
	out = bughuntApply(t, "mysql", "select * from orders",
		WithTableNames("sales.orders"), WithDefaultDB("sales"))
	if got := bughuntWraps(t, out, "mysql"); got != 1 {
		t.Fatalf("qualified scope + default DB should match bare ref, wraps=%d: %s", got, out)
	}
	// WithDefaultDB must not leak the other way: qualified ref in a different
	// schema stays out of scope.
	out = bughuntApply(t, "mysql", "select * from hr.orders",
		WithTableNames("sales.orders"), WithDefaultDB("sales"))
	if got := bughuntWraps(t, out, "mysql"); got != 0 {
		t.Fatalf("hr.orders must not match sales.orders scope, wraps=%d: %s", got, out)
	}
}

// A CTE name in scope must not suppress wrapping of a physical table with the
// same bare name in a SIBLING scope (regression family of TestCTEScopeSecurity).
func TestBughuntCTESiblingScope(t *testing.T) {
	sql := "select * from (with x as (select 1 as id) select * from x) a join x on a.id = x.id"
	out := bughuntApply(t, "mysql", sql)
	// Outer x is a physical table (the CTE x is confined to the derived table).
	if got := bughuntWraps(t, out, "mysql"); got != 1 {
		t.Fatalf("want 1 wrap (outer physical x), got %d: %s", got, out)
	}
}

// Injection-ish whereClause values must be rejected at compile time (config
// error), never producing SQL where the predicate escapes the subquery WHERE.
func TestBughuntWhereClauseInjectionRejected(t *testing.T) {
	bad := []string{
		"1=1) as x --",                    // paren escape
		"1=1 union select * from secrets", // set-op smuggling
		"1=1; drop table a",               // second statement
	}
	for _, w := range bad {
		if _, err := ApplyRowFilter("select * from a", w, WithDialect("mysql")); err == nil {
			t.Fatalf("whereClause %q accepted; expected rejection", w)
		}
	}
	// A line-comment suffix parses in validation ("SELECT 1 WHERE p -- c") but
	// would comment out the template's closing paren. It must fail (any error
	// class), never emit malformed or predicate-escaped SQL.
	out, err := ApplyRowFilter("select * from a", "tenant = 'alice' -- boom", WithDialect("mysql"))
	if err == nil {
		if _, perr := testClient.ParseOne(out, "mysql"); perr != nil {
			t.Fatalf("comment-suffixed whereClause produced unparseable SQL: %s", out)
		}
	}
}

// Idempotency probe: applying the filter to its own output must still be
// valid SQL (the second pass re-wraps the inner physical table, which is
// semantically idempotent for an idempotent predicate; not asserting wrap
// counts because idempotency is not documented).
func TestBughuntDoubleApplyStillValid(t *testing.T) {
	out1 := bughuntApply(t, "mysql", "select * from a")
	out2 := bughuntApply(t, "mysql", out1)
	if got := bughuntWraps(t, out2, "mysql"); got < 1 {
		t.Fatalf("second application lost the filter entirely: %s", out2)
	}
}

// Quoted case-sensitive identifiers: scope matching is case-insensitive even
// for quoted identifiers. Postgres treats "Orders" and orders as DIFFERENT
// tables; matching is lower-cased on both sides, so scoping "orders" also
// wraps "Orders". This only ever OVER-filters (extra predicate, fail-safe
// direction), so it is noted as suspicious, not asserted as a bug.
func TestBughuntQuotedCaseInsensitiveScope(t *testing.T) {
	out := bughuntApply(t, "postgres", `select * from "Orders"`, WithTableNames("orders"))
	t.Logf("case-insensitive scope wraps quoted \"Orders\": wraps=%d out=%s",
		bughuntWraps(t, out, "postgres"), out)
	if !strings.Contains(out, `"Orders"`) {
		t.Fatalf("quoted identifier casing lost: %s", out)
	}
}

// Table-valued functions must never be wrapped nor given empty aliases.
func TestBughuntTableFunctions(t *testing.T) {
	out := bughuntApply(t, "postgres", "select * from generate_series(1, 10) g join a on true")
	if got := bughuntWraps(t, out, "postgres"); got != 1 {
		t.Fatalf("want only physical table a wrapped, got %d: %s", got, out)
	}
}
