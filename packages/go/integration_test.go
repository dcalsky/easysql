package easysql

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

const nativeTestLibraryEnv = "EASYSQL_NATIVE_TEST_LIBRARY"
const requireNativeTestEnv = "EASYSQL_REQUIRE_NATIVE_TEST"

func nativeTestClient(t *testing.T) *Client {
	t.Helper()
	path := os.Getenv(nativeTestLibraryEnv)
	if path == "" {
		if os.Getenv(requireNativeTestEnv) != "" {
			t.Fatalf("%s is required", nativeTestLibraryEnv)
		}
		t.Skipf("set %s to run native integration tests", nativeTestLibraryEnv)
	}
	client, err := Open(path)
	if err != nil {
		t.Fatalf("open native library: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close native library: %v", err)
		}
	})
	return client
}

func TestNativeRawABIContract(t *testing.T) {
	client := nativeTestClient(t)
	raw, err := client.Execute([]byte("not-json"))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ABIVersion uint32 `json:"abiVersion"`
		Status     Status `json:"status"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("native response is not JSON: %v: %s", err, raw)
	}
	if response.ABIVersion != ABIVersion || response.Status != StatusInvalidArgument || response.Error == "" {
		t.Fatalf("unexpected native error response: %+v", response)
	}
}

func TestNativeTypedOperations(t *testing.T) {
	client := nativeTestClient(t)
	version, err := client.RuntimeVersion()
	if err != nil || version == "" {
		t.Fatalf("RuntimeVersion = %q, %v", version, err)
	}

	rewritten, err := client.ApplyRowFilter(
		"SELECT id FROM orders",
		"tenant_id = 7",
		ApplyRowFilterOptions{Dialect: "postgres"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rewritten, "WHERE") || !strings.Contains(rewritten, "tenant_id") {
		t.Fatalf("unexpected rewrite: %q", rewritten)
	}

	bound, err := client.BindCTEs(
		"SELECT id FROM recent",
		[]CTEBinding{{Name: "recent", Query: "SELECT id FROM orders"}},
		BindCTEsOptions{Dialect: "trino"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound, "WITH") || !strings.Contains(bound, "orders") {
		t.Fatalf("unexpected bound CTE query: %q", bound)
	}

	lineage, err := client.LineageSourceColumns(
		"SELECT id FROM orders",
		AnalysisOptions{Dialect: "trino", Metadata: map[string][]string{"orders": {"id"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if columns := lineage["orders"]; len(columns) != 1 || columns[0] != "id" {
		t.Fatalf("unexpected lineage: %v", lineage)
	}

	columns, err := client.ParseColumns(
		"SELECT id, amount AS total FROM orders",
		AnalysisOptions{Dialect: "trino"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 || columns[0] != "id" || columns[1] != "total" {
		t.Fatalf("columns = %v, want [id total]", columns)
	}

	references, err := client.ReferencedColumns(
		"SELECT id FROM orders WHERE status = 'PAID'",
		AnalysisOptions{Dialect: "trino"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if columns := references["orders"]; len(columns) != 2 || columns[0] != "id" || columns[1] != "status" {
		t.Fatalf("unexpected referenced columns: %v", references)
	}

	uses, err := client.ReferencedColumnUsages(
		"SELECT id FROM orders WHERE status = 'PAID'",
		AnalysisOptions{Dialect: "trino"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"id/SELECT": false, "status/WHERE": false}
	for _, use := range uses {
		key := use.Column + "/" + use.Clause
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing referenced column use %s in %v", key, uses)
		}
	}

	replaced, err := client.RewriteTableReferences(
		"SELECT id FROM sales.orders",
		[]TableRewrite{{
			MatchKey: "sales.orders",
			Inline:   &TableRef{Schema: "archive", Table: "orders"},
		}},
		RewriteTableReferencesOptions{Dialect: "trino"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replaced, "archive") || !strings.Contains(replaced, "orders") {
		t.Fatalf("unexpected table rewrite: %q", replaced)
	}

	if _, err := client.ParseColumns("SELECT (", AnalysisOptions{Dialect: "trino"}); !errors.Is(err, ErrParse) {
		t.Fatalf("parse error = %v, want ErrParse", err)
	}
}

func TestNativeConcurrentCalls(t *testing.T) {
	client := nativeTestClient(t)
	const workers = 12
	const calls = 20
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range calls {
				columns, err := client.ParseColumns("SELECT id FROM orders", AnalysisOptions{Dialect: "trino"})
				if err != nil {
					errors <- err
					return
				}
				if len(columns) != 1 || columns[0] != "id" {
					errors <- &unexpectedColumnsError{columns: columns}
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

type unexpectedColumnsError struct{ columns []string }

func (err *unexpectedColumnsError) Error() string {
	return "unexpected columns: " + strings.Join(err.columns, ",")
}
