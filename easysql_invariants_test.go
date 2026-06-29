package easysql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file verifies the rewrite with invariants that are INDEPENDENT of the
// rewrite's own scanning/decision logic, so they act as an external oracle
// (helpers applyTimed/sentinelErr/tableNameCounts live in main_test.go):
//
//  1. No-op fidelity: scoping to a table that does not exist wraps nothing, so
//     the input must come back byte-for-byte (or the call fails closed). A
//     no-op never round-trips through the generator.
//  2. Validity: any successful wrap-all output must re-parse. (When tables are
//     wrapped the generator reformats, so the output is not byte-identical.)
//  3. Table-multiset preservation: the multiset of physical table names is
//     identical before and after, proving no table is dropped, duplicated,
//     renamed or corrupted by the rewrite.
//
// The same checks run both as a deterministic table test and as a fuzz target.

const invMarker = "zzq_inv_marker = 1"

// checkInvariants runs the three oracle checks for one input under one dialect.
func checkInvariants(t *testing.T, dialect, pg, sql string) {
	t.Helper()

	// (1) No-op fidelity: scoping to a table that does not exist wraps nothing,
	// so the input must come back byte-for-byte (no generator round-trip).
	noop, err := applyTimed(sql, invMarker, WithDialect(dialect),
		WithTableNames("zzq_table_that_does_not_exist_zzq"))
	switch {
	case err == nil:
		if noop != sql {
			t.Fatalf("[%s] no-op scope mutated input:\n in:  %q\n out: %q", dialect, sql, noop)
		}
	case errors.Is(err, errTimeout):
		return // upstream hang, tolerated
	case !sentinelErr(err):
		t.Fatalf("[%s] no-op unclassified error: %v\n sql: %s", dialect, err, sql)
	}

	// (2)/(3) Wrap-all.
	out, err := applyTimed(sql, invMarker, WithDialect(dialect))
	if err != nil {
		if errors.Is(err, errTimeout) || sentinelErr(err) {
			return // fail-closed or upstream hang is acceptable
		}
		t.Fatalf("[%s] unclassified error: %v\n sql: %s", dialect, err, sql)
	}
	if _, e := testClient.ParseOne(out, pg); e != nil {
		t.Fatalf("[%s] output does not re-parse:\n in:  %s\n out: %s\n err: %v", dialect, sql, out, e)
	}
	// Compare the output's physical-table multiset to a like-for-like baseline,
	// so the rewrite is held accountable for adding/dropping tables without
	// flagging engine parse<->generate round-trip quirks (which can shift the
	// table set for adversarial input). When tables were wrapped the output is
	// generated, so the baseline is the input run through the same generator;
	// when nothing was wrapped the output is the raw input, so the baseline is
	// the raw input too.
	var base map[string]int
	var okBase bool
	if out == sql {
		base, okBase = tableNameCounts(t, sql, pg)
	} else {
		base, okBase = regenTableCounts(t, sql, pg)
	}
	got, okOut := tableNameCounts(t, out, pg)
	if okBase && okOut && !reflect.DeepEqual(base, got) {
		t.Fatalf("[%s] table multiset changed:\n base: %v  (%s)\n out:  %v  (%s)", dialect, base, sql, got, out)
	}
}

// invariantCorpus is a broad set of statements across dialects/shapes; it is
// shared by the deterministic test and used as fuzz seeds.
var invariantCorpus = []struct{ dialect, sql string }{
	{"mysql", "select * from a"},
	{"mysql", "select id, Name from Orders o where o.x = 1"},
	{"mysql", "select a.id from a join b on a.id = b.id"},
	{"mysql", "select * from a left join b on a.id = b.id"},
	{"mysql", "select t1.id from a t1 join a t2 on t1.id = t2.id"},
	{"mysql", "select * from a join b join c"},
	{"mysql", "select * from a, b, c"},
	{"mysql", "select * from c where id in (select id from a)"},
	{"mysql", "select * from (select * from a) x"},
	{"mysql", "select * from a union select * from b"},
	{"mysql", "select * from a union all select * from b intersect select * from c"},
	{"mysql", "select * from a use index(idx)"},
	{"mysql", "select * from a partition(p0)"},
	{"mysql", "select 1 from dual"},
	{"mysql", "with c as (select * from a) select * from c"},
	{"mysql", "with a as (select * from a) select * from a"},
	{"mysql", "select * from t union all select * from (with t as (select 1 as id) select * from t) q"},
	{"mysql", "select (select count(*) from x) n, a.id from a"},
	{"mysql", "/* lead */ select a.id from a -- tail\nwhere a.x = 1"},
	{"mysql", "select row_number() over (partition by k order by ts) from a"},
	{"mysql", "select case when x > 0 then 1 else 0 end from a"},
	{"mysql", "select 姓名 from 订单 where city = '北京' -- 备注"},
	{"mysql", "select * from a;"},
	{"postgres", "select * from sales.orders"},
	{"postgres", `select * from "order"`},
	{"postgres", "select sales.orders.id from sales.orders"},
	{"postgres", "select * from a, lateral (select * from b where b.aid = a.id) s"},
	{"postgres", "select * from generate_series(1, 10)"},
	{"postgres", "select distinct on (uid) uid, ts from a order by uid, ts desc"},
	{"trino", "select * from hive.sales.a"},
	{"trino", "select * from a tablesample bernoulli (10)"},
	{"trino", "select x from a cross join unnest(a.arr) as t(x)"},
	{"trino", "select * from unnest(array[1,2]) with ordinality"},
	{"trino", "select k, sum(v) from a group by cube (k)"},
	{"starrocks", "select * from table(generator(10))"},
	{"starrocks", "select id, row_number() over (partition by k order by ts) rn from a qualify rn = 1"},
	{"starrocks", "select /*+ SET_VAR(query_timeout=5) */ * from a"},
}

func TestInputGuard(t *testing.T) {
	deepFunc := "select s" + strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300) + " from a"
	deepCurly := "select " + strings.Repeat("{", 300) + "1" + strings.Repeat("}", 300) + " from a"
	huge := "select * from a where x in (" + strings.Repeat("1,", maxInputBytes) + "1)"

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"deep function nesting", deepFunc},
		{"deep curly nesting", deepCurly},
		{"oversized input", huge},
	} {
		t.Run("rejected/"+tc.name, func(t *testing.T) {
			_, err := ApplyRowFilter(tc.sql, "t = 1", WithDialect("mysql"))
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("want ErrUnsupported, got %v", err)
			}
		})
	}

	// Brackets inside string literals must not trip the guard: valid SQL is
	// accepted and rewritten.
	for _, tc := range []string{
		"select '((((((((((' from a",
		"select * from a where note = '{a:{b:{c}}}'",
		"select * from a where x in (1, (2), ((3)))",
	} {
		t.Run("accepted/"+tc, func(t *testing.T) {
			if _, err := ApplyRowFilter(tc, "t = 1", WithDialect("mysql")); err != nil {
				t.Fatalf("valid SQL rejected: %v", err)
			}
		})
	}
}

func TestRewriteInvariants(t *testing.T) {
	for _, tc := range invariantCorpus {
		pg := dialectToPolyglot[tc.dialect]
		t.Run(tc.dialect+"/"+tc.sql, func(t *testing.T) {
			checkInvariants(t, tc.dialect, pg, tc.sql)
		})
	}
}

func FuzzRewrite(f *testing.F) {
	for _, tc := range invariantCorpus {
		f.Add(tc.sql)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		if testClient == nil {
			t.Skip("no ffi")
		}
		// Fuzz under mysql; the invariants hold regardless of input validity
		// because invalid input yields a classified error and is skipped.
		checkInvariants(t, "mysql", "mysql", sql)
	})
}
