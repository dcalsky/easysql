package easysql

import (
	"reflect"
	"testing"
)

func TestParseColumnsUnrelatedCatalogDoesNotExpandStarFromOtherTable(t *testing.T) {
	metadata := map[string][]string{
		"foo":         {},
		"other_table": {"a", "b", "c"},
	}
	got, err := ParseColumns("SELECT * FROM foo",
		WithLineageDialect("trino"), WithLineageMetadata(metadata))
	if err != nil {
		t.Fatal(err)
	}
	// foo is in metadata with an explicit empty column list — zero output columns.
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("got %v, want []", got)
	}
}

func TestParseColumnsStarWithoutTableMetadataFallsBackToStar(t *testing.T) {
	got, err := ParseColumns("SELECT * FROM foo",
		WithLineageDialect("trino"),
		WithLineageMetadata(map[string][]string{"other_table": {"a", "b"}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("got %v, want [*]", got)
	}
}

func TestReferencedColumnsUnrelatedCatalogNotCredited(t *testing.T) {
	metadata := map[string][]string{
		"foo":         {},
		"other_table": {"a", "b", "c"},
	}
	got, err := ReferencedColumns("SELECT a, b FROM foo",
		WithLineageDialect("trino"), WithLineageMetadata(metadata))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"foo": {"a", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
