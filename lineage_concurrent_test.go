package easysql

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// unionLeafTemplates are non-trivial leaf SELECTs (joins + WHERE + functions +
// aliases) over the trinoMetadata tables. They all project three columns so any
// number of them can be combined with UNION ALL into a valid set operation, and
// together they reference every source table so the merged lineage is rich.
var unionLeafTemplates = []string{
	`SELECT u.user_name AS c1, o.amount AS c2, o.quantity AS c3
	 FROM hive.raw.users u
	 JOIN hive.raw.orders o ON u.user_id = o.user_id
	 WHERE o.status = 'PAID' AND u.email IS NOT NULL`,

	`SELECT o.order_id AS c1, p.paid_amount AS c2, o.user_id AS c3
	 FROM hive.raw.orders o
	 JOIN hive.raw.payments p ON o.order_id = p.order_id
	 WHERE p.paid_at >= TIMESTAMP '2024-01-01 00:00:00'`,

	`SELECT u.user_id AS c1, date_trunc('day', o.order_ts) AS c2, o.amount AS c3
	 FROM hive.raw.users u
	 JOIN hive.raw.orders o ON u.user_id = o.user_id
	 WHERE o.order_date >= DATE '2024-01-01'`,
}

// buildUnionSQL returns a UNION ALL of branches leaf SELECTs, cycling through
// unionLeafTemplates. With branches > 1 it forces the lineage drivers down the
// multi-leaf path (where the concurrent version parallelizes).
func buildUnionSQL(branches int) string {
	parts := make([]string, branches)
	for i := 0; i < branches; i++ {
		parts[i] = unionLeafTemplates[i%len(unionLeafTemplates)]
	}
	return strings.Join(parts, "\nUNION ALL\n")
}

// benchCases are the workloads exercised by both the correctness test and the
// benchmark: a single SELECT (the common path) plus UNION set operations with a
// growing number of leaves.
func benchCases() []struct {
	name string
	sql  string
} {
	cases := []struct {
		name string
		sql  string
	}{
		{"single_select", unionLeafTemplates[0]},
	}
	for _, n := range []int{2, 4, 8, 16, 32} {
		cases = append(cases, struct {
			name string
			sql  string
		}{fmt.Sprintf("union_%d", n), buildUnionSQL(n)})
	}
	return cases
}

// TestSourceTableColumnsConcurrentMatchesSerial asserts the concurrent driver
// is a faithful drop-in: for every workload it returns exactly what the serial
// baseline returns.
func TestSourceTableColumnsConcurrentMatchesSerial(t *testing.T) {
	for _, tc := range benchCases() {
		t.Run(tc.name, func(t *testing.T) {
			serial, err := SourceTableColumns(tc.sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("serial: %v", err)
			}
			concurrent, err := SourceTableColumnsConcurrent(tc.sql,
				WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata))
			if err != nil {
				t.Fatalf("concurrent: %v", err)
			}
			if !reflect.DeepEqual(serial, concurrent) {
				t.Fatalf("concurrent != serial\n serial:     %v\n concurrent: %v", serial, concurrent)
			}
		})
	}
}

// BenchmarkLineageSerialVsConcurrent compares the serial baseline against the
// concurrent driver across a single SELECT and UNIONs of increasing width. Run
// with, e.g.:
//
//	go test -run '^$' -bench BenchmarkLineageSerialVsConcurrent -benchmem .
func BenchmarkLineageSerialVsConcurrent(b *testing.B) {
	if testClient == nil {
		b.Skip("polyglot FFI library not available")
	}
	for _, tc := range benchCases() {
		b.Run("serial/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := SourceTableColumns(tc.sql,
					WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata)); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("concurrent/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := SourceTableColumnsConcurrent(tc.sql,
					WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
