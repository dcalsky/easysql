package easysql

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// Exercise the published native artifact directly, independently of easysql's
// adapters. Expectations follow the v0.11.0 Go integration tests and SQL slots.
func TestNativeOutputColumnsContract(t *testing.T) {
	output, err := testClient.OutputColumns("SELECT 1, t.*, b FROM t", "trino")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(output)
	t.Logf("raw output: %s", raw)
	if output.OrdinalComplete || len(output.Columns) != 3 || output.Columns[0].Kind != polyglot.OutputColumnUnnamed || output.Columns[1].Kind != polyglot.OutputColumnWildcard || output.Columns[2].Name == nil || *output.Columns[2].Name != "b" {
		t.Fatalf("unexpected output: %s", raw)
	}
	schema := polyglot.ValidationSchema{Tables: []polyglot.SchemaTable{{Name: "t", Columns: []polyglot.SchemaColumn{{Name: "z", Type: "INT"}, {Name: "a", Type: "INT"}}}}}
	output, err = testClient.OutputColumnsWithSchema("SELECT * FROM t", schema, "trino")
	if err != nil {
		t.Fatal(err)
	}
	if !output.OrdinalComplete || len(output.Columns) != 2 || *output.Columns[0].Name != "z" || *output.Columns[1].Name != "a" {
		t.Fatalf("schema order: %+v", output)
	}
	schema.Tables[0].Columns = nil
	output, err = testClient.OutputColumnsWithSchema("SELECT * FROM t", schema, "trino")
	if err != nil {
		t.Fatal(err)
	}
	if output.OrdinalComplete || len(output.Columns) != 1 || output.Columns[0].Kind != polyglot.OutputColumnWildcard {
		t.Fatalf("empty native schema must remain open: %+v", output)
	}
}

func TestParseColumnsNativeProjectionOrder(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      []string
	}{
		{"cte_reordered_projection", "WITH c AS (SELECT a, z FROM t) SELECT * FROM c", []string{"a", "z"}},
		{"multiple_stars_and_expression", "SELECT t.*, 1, t.* FROM t", []string{"z", "a", "_col2", "z", "a"}},
		{"duplicate_computed_alias", "SELECT 1 AS same, a AS same FROM t", []string{"same", "same"}},
		{"explicit_synthetic_looking_alias", "SELECT a AS _col_0 FROM t", []string{"_col_0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseColumns(tc.sql, WithLineageDialect("trino"), WithLineageMetadata(map[string][]string{"t": {"z", "a"}}))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Name-aligned set operations were corrected after v0.9.0. Exercise the
// public API, including columns supplied only by the right branch.
func TestParseColumnsUnionByName(t *testing.T) {
	got, err := ParseColumns("SELECT 1 AS a UNION ALL BY NAME SELECT 2 AS b", WithLineageDialect("duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}

// ColumnUses is not a replacement for ReferencedColumns: it excludes
// projection references in inner queries that do not reach the final output.
func TestNativeColumnUsesCompatibilityBoundary(t *testing.T) {
	sql := "WITH c AS (SELECT id, amount, unused FROM orders) SELECT id FROM c WHERE amount > 0"
	a, err := testClient.AnalyzeQuery(sql, polyglot.AnalyzeQueryOptions{Dialect: "trino"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a.ColumnUses)
	t.Logf("raw column uses: %s", raw)
	if len(a.ColumnUses) != 1 || a.ColumnUses[0].Context != polyglot.ColumnUseFilter || len(a.ColumnUses[0].References) != 1 || a.ColumnUses[0].References[0].Column != "amount" || a.ColumnUses[0].References[0].Table == nil || *a.ColumnUses[0].References[0].Table != "orders" {
		t.Fatalf("unexpected non-projection facts: %s", raw)
	}
	assertReferencedColumns(t, "inner_projection_is_still_a_reference", sql, map[string][]string{"orders": {"id", "amount", "unused"}})
}

func TestNativeParserDepthAndEOFContract(t *testing.T) {
	// A unary chain contains no brackets and defeated the old Go heuristic.
	_, err := testClient.Parse("SELECT "+strings.Repeat("~ ", 4000)+"1", "mysql")
	var native *polyglot.Error
	if !errors.As(err, &native) || !strings.Contains(native.Message, "E_GUARD_PARSER_DEPTH_EXCEEDED") {
		t.Fatalf("native depth guard: %v", err)
	}
	if !errors.Is(classifyParseError(err), ErrUnsupported) {
		t.Fatalf("guard classification: %v", err)
	}
	for _, sql := range []string{"SELECT IF(IF(", "SELECT (", "SELECT ARRAY["} {
		if _, err := testClient.Parse(sql, "mysql"); err == nil {
			t.Fatalf("truncated SQL accepted: %s", sql)
		}
	}
	if _, err := testClient.Parse("SELECT 1", "mysql"); err != nil {
		t.Fatalf("client unusable after guard failure: %v", err)
	}
}

func TestParseColumnsEmptyMetadataScope(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{"SELECT 1, t.* FROM t", []string{"_col0", "*"}},
		{"SELECT 1, e.* FROM empty e", []string{"_col0"}},
		{"SELECT e.*, t.* FROM empty e CROSS JOIN t", []string{"*"}},
	} {
		got, err := ParseColumns(tc.sql, WithLineageDialect("trino"), WithLineageMetadata(map[string][]string{"empty": {}}))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v want %v", tc.sql, got, tc.want)
		}
	}
}
