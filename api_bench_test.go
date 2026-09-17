package easysql

import "testing"

// BenchmarkAnalysis exercises public APIs without optional external fixtures.
// Every call is uncached; metadata is identical across versions.
func BenchmarkAnalysis(b *testing.B) {
	metadata := map[string][]string{"orders": {"id", "uid", "amount", "status"}, "users": {"id", "name", "region"}}
	cases := []struct{ name, sql string }{
		{"simple", "SELECT id, amount FROM orders WHERE status = 'paid'"},
		{"star", "SELECT o.*, u.name FROM orders o JOIN users u ON o.uid = u.id"},
		{"cte", "WITH c AS (SELECT uid, SUM(amount) AS total FROM orders WHERE status = 'paid' GROUP BY uid) SELECT u.name, c.total FROM c JOIN users u ON c.uid = u.id ORDER BY c.total"},
		{"union", "SELECT id, amount FROM orders UNION ALL SELECT id, amount FROM orders WHERE status = 'paid'"},
	}
	for _, api := range []struct {
		name string
		call func(string, ...LineageOption) error
	}{
		{"ParseColumns", func(s string, o ...LineageOption) error { _, e := ParseColumns(s, o...); return e }},
		{"ReferencedColumns", func(s string, o ...LineageOption) error { _, e := ReferencedColumns(s, o...); return e }},
		{"ReferencedColumnUsages", func(s string, o ...LineageOption) error { _, e := ReferencedColumnUsages(s, o...); return e }},
		{"LineageSourceColumns", func(s string, o ...LineageOption) error { _, e := LineageSourceColumns(s, o...); return e }},
	} {
		for _, tc := range cases {
			b.Run(api.name+"/"+tc.name, func(b *testing.B) {
				opts := []LineageOption{WithLineageDialect("trino"), WithLineageMetadata(metadata)}
				if err := api.call(tc.sql, opts...); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := api.call(tc.sql, opts...); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
