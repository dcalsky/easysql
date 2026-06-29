package easysql

import "testing"

// benchCorpus is a set of representative statements exercised by the rewrite
// corpus test and the benchmark.
var benchCorpus = []struct {
	name    string
	dialect string
	sql     string
}{
	{"simple", "mysql", "select * from orders"},
	{"two_join", "mysql", "select a.id, b.name from orders a join users b on a.uid = b.id"},
	{"three_join", "mysql",
		"select * from orders o join users u on o.uid = u.id join items i on i.oid = o.id where o.ts > 0"},
	{"left_join", "mysql", "select * from orders o left join users u on o.uid = u.id"},
	{"subquery_from", "mysql", "select * from (select * from orders) x join users u on x.uid = u.id"},
	{"cte", "mysql", "with c as (select * from orders) select * from c join users u on c.uid = u.id"},
	{"union", "mysql", "select * from orders union all select * from archived_orders"},
	{"scalar_subquery", "mysql",
		"select o.id, (select count(*) from events e where e.oid = o.id) n from orders o"},
	{"schema_qualified", "postgres",
		"select sales.orders.id, u.name from sales.orders, public.users u where sales.orders.uid = u.id"},
	{"analytical", "mysql",
		"select u.region, count(*) c, sum(o.amount) total " +
			"from orders o join users u on o.uid = u.id join items i on i.oid = o.id " +
			"where o.ts >= 100 and u.active = 1 group by u.region having sum(o.amount) > 0 order by total desc limit 10"},
}

const benchPredicate = "z_pt = 1"

// TestRewriteCorpus checks the rewrite over a representative corpus: every
// statement must succeed, re-parse, and preserve the multiset of physical tables
// (none dropped, duplicated, renamed or corrupted).
func TestRewriteCorpus(t *testing.T) {
	if testClient == nil {
		t.Skip("no ffi")
	}
	for _, tc := range benchCorpus {
		t.Run(tc.name, func(t *testing.T) {
			pg := dialectToPolyglot[tc.dialect]

			r, err := compile(testClient, benchPredicate, WithDialect(tc.dialect))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			out, err := r.rewrite(tc.sql)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			t.Logf("\n in:  %s\n out: %s", tc.sql, out)

			if _, e := testClient.ParseOne(out, pg); e != nil {
				t.Fatalf("output invalid: %v", e)
			}
			in, okIn := tableNameCounts(t, tc.sql, pg)
			got, okOut := tableNameCounts(t, out, pg)
			if okIn && okOut && !mapsEqual(in, got) {
				t.Fatalf("table multiset differs:\n in:  %v\n out: %v", in, got)
			}
		})
	}
}

// BenchmarkRewrite measures the steady-state per-call rewrite cost on each
// corpus statement. The rewriter is compiled once per case (outside the timed
// loop) and warmed so the subquery template and native caches are primed.
func BenchmarkRewrite(b *testing.B) {
	if testClient == nil {
		b.Skip("no ffi")
	}
	for _, tc := range benchCorpus {
		r, err := compile(testClient, benchPredicate, WithDialect(tc.dialect))
		if err != nil {
			b.Fatalf("compile: %v", err)
		}
		if _, err := r.rewrite(tc.sql); err != nil {
			b.Fatalf("warm: %v", err)
		}

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := r.rewrite(tc.sql); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
