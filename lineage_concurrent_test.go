package easysql

import (
	"reflect"
	"testing"
)

// TestLineageSourceColumnsConcurrentMatchesSerial preserves the legacy API's
// compatibility guarantee while anchoring the set-operation result itself.
func TestLineageSourceColumnsConcurrentMatchesSerial(t *testing.T) {
	const sql = `SELECT u.user_id FROM hive.raw.users u
		UNION ALL
		SELECT o.order_id FROM hive.raw.orders o`
	opts := []LineageOption{WithLineageDialect("trino"), WithLineageMetadata(trinoMetadata)}

	serial, err := LineageSourceColumns(sql, opts...)
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	want := map[string][]string{
		"hive.raw.orders": {"order_id"},
		"hive.raw.users":  {"user_id"},
	}
	if !reflect.DeepEqual(serial, want) {
		t.Fatalf("serial lineage drifted:\n got:  %v\n want: %v", serial, want)
	}

	concurrent, err := LineageSourceColumnsConcurrent(sql, opts...)
	if err != nil {
		t.Fatalf("concurrent: %v", err)
	}
	if !reflect.DeepEqual(concurrent, serial) {
		t.Fatalf("concurrent != serial\n serial:     %v\n concurrent: %v", serial, concurrent)
	}
}
