package easysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

var testClient *polyglot.Client

// testWhere is a verbatim WHERE expression whose single string literal
// (testMarker) is counted to assert how many tables were wrapped.
const (
	testWhere  = "tenant = 'alice'"
	testMarker = "alice"
)

func TestMain(m *testing.M) {
	// Initialize (and capture) the shared engine the same way the public API
	// does; tests also use this client directly for AST assertions.
	c, err := defaultClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "skipping easysql tests: polyglot FFI library not available:", err)
		fmt.Fprintln(os.Stderr, "ensure the matching .ffi/ artifact for this platform is present to run them")
		os.Exit(0)
	}
	testClient = c
	os.Exit(m.Run())
}

// --- structural validators -------------------------------------------------

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

func walkJSON(node any, fn func(map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		fn(v)
		for _, c := range v {
			walkJSON(c, fn)
		}
	case []any:
		for _, c := range v {
			walkJSON(c, fn)
		}
	}
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
	// Be explicit: the first branch's real table must be filtered.
	if !strings.Contains(out, "FROM (SELECT * FROM t WHERE") && !strings.Contains(out, "FROM (SELECT * FROM `t` WHERE") {
		t.Fatalf("real table t was not filtered (security regression):\n%s", out)
	}
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

func TestSelfCheck(t *testing.T) {
	// With self-check on, a valid rewrite still succeeds.
	for _, sql := range []string{
		"select * from a",
		"select a.id from a join b on a.id = b.id",
		"select * from a union select * from b",
		"with c as (select * from a) select * from c",
	} {
		if _, err := ApplyRowFilter(sql, testWhere, WithSelfCheck(true)); err != nil {
			t.Fatalf("self-check rewrite %q: %v", sql, err)
		}
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
		t.Run(tc.dialect+"/"+tc.name, func(t *testing.T) {
			rewriteValid(t, tc.dialect, tc.sql, testWhere, testMarker, tc.wantWraps)
		})
		pass++
	}
	if pass != len(cases) {
		t.Fatalf("coverage regressed: %d/%d", pass, len(cases))
	}
	t.Logf("dialect coverage: %d/%d parsed+rewritten+validated", len(cases), len(cases))
}
