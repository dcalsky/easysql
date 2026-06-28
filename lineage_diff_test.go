package easysql

import (
	"encoding/json"
	"os"
	"testing"
)

func loadDiffQueries(tb testing.TB) (queries []struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}, metadata map[string][]string) {
	tb.Helper()
	dir := os.Getenv("LINEAGE_DUMP")
	if dir == "" {
		tb.Skip("set LINEAGE_DUMP=<dir with queries.json> to run")
	}
	metadata = map[string][]string{
		"hive.raw.users": {"user_id", "user_name", "email"},
		"hive.raw.orders": {
			"order_id", "user_id", "amount", "quantity",
			"order_ts", "order_date", "status",
		},
		"hive.raw.payments": {"order_id", "paid_amount", "paid_at"},
	}
	raw, err := os.ReadFile(dir + "/queries.json")
	if err != nil {
		tb.Fatal(err)
	}
	if err := json.Unmarshal(raw, &queries); err != nil {
		tb.Fatal(err)
	}
	return queries, metadata
}

// BenchmarkSourceTableColumns measures cold (uncached) per-call latency over the
// shared differential query set. Run with:
//
//	LINEAGE_DUMP=/tmp/lineage_cmp go test -run x -bench BenchmarkSourceTableColumns -benchtime 1400x
func BenchmarkSourceTableColumns(b *testing.B) {
	queries, metadata := loadDiffQueries(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		if _, err := SourceTableColumns(testClient, q.SQL,
			WithLineageDialect("trino"), WithLineageMetadata(metadata)); err != nil {
			b.Fatalf("%s: %v", q.Name, err)
		}
	}
}

// BenchmarkSourceTableColumnsParallel measures throughput under concurrency. The
// Polyglot client is safe for concurrent use, so this scales across cores with
// no extra machinery (the Python port needed a process pool to do the same).
func BenchmarkSourceTableColumnsParallel(b *testing.B) {
	queries, metadata := loadDiffQueries(b)
	b.ReportAllocs()
	b.ResetTimer()
	var i int
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q := queries[i%len(queries)]
			i++
			if _, err := SourceTableColumns(testClient, q.SQL,
				WithLineageDialect("trino"), WithLineageMetadata(metadata)); err != nil {
				b.Fatalf("%s: %v", q.Name, err)
			}
		}
	})
}

// TestLineageDumpForDiff is an off-by-default harness used to compare the Go
// implementation against the Python sqllineage reference on a shared query set.
// Run with: LINEAGE_DUMP=/tmp/lineage_cmp go test -run TestLineageDumpForDiff
func TestLineageDumpForDiff(t *testing.T) {
	dir := os.Getenv("LINEAGE_DUMP")
	if dir == "" {
		t.Skip("set LINEAGE_DUMP=<dir with queries.json> to run")
	}
	queries, metadata := loadDiffQueries(t)

	out := map[string]any{}
	for _, q := range queries {
		res, err := SourceTableColumns(testClient, q.SQL,
			WithLineageDialect("trino"), WithLineageMetadata(metadata))
		if err != nil {
			out[q.Name] = map[string]string{"__error__": err.Error()}
			continue
		}
		out[q.Name] = res
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(dir+"/go_results.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote go_results.json with %d entries", len(out))
}
