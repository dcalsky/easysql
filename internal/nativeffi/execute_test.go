package nativeffi

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type decodedResponse struct {
	ABIVersion uint32          `json:"abiVersion"`
	Status     int32           `json:"status"`
	Data       json.RawMessage `json:"data"`
	Error      string          `json:"error"`
}

func executeRequest(t *testing.T, operation, args string) decodedResponse {
	t.Helper()
	request := fmt.Sprintf(`{"abiVersion":1,"operation":%q,"args":%s}`, operation, args)
	return decodeResponse(t, Execute([]byte(request)))
}

func decodeResponse(t *testing.T, raw []byte) decodedResponse {
	t.Helper()
	var response decodedResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, raw)
	}
	if response.ABIVersion != ABIVersion {
		t.Fatalf("abiVersion = %d, want %d", response.ABIVersion, ABIVersion)
	}
	return response
}

func requireSuccess(t *testing.T, response decodedResponse) json.RawMessage {
	t.Helper()
	if response.Status != StatusSuccess {
		t.Fatalf("status = %d, error = %q", response.Status, response.Error)
	}
	if len(response.Data) == 0 {
		t.Fatal("successful response has no data")
	}
	return response.Data
}

func TestExecuteAllOperations(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		args      string
		contains  []string
	}{
		{
			name:      "apply row filter with default dialect",
			operation: "applyRowFilter",
			args:      `{"sql":"SELECT id FROM orders","whereClause":"tenant_id = 7"}`,
			contains:  []string{"WHERE", "tenant_id"},
		},
		{
			name:      "bind CTEs",
			operation: "bindCTEs",
			args:      `{"consumerSQL":"SELECT id FROM recent","bindings":[{"name":"recent","query":"SELECT id FROM orders"}],"dialect":"trino"}`,
			contains:  []string{"WITH", "recent", "orders"},
		},
		{
			name:      "lineage source columns",
			operation: "lineageSourceColumns",
			args:      `{"sql":"SELECT id FROM orders","dialect":"trino","metadata":{"orders":["id"]}}`,
			contains:  []string{`"orders"`, `"id"`},
		},
		{
			name:      "parse columns",
			operation: "parseColumns",
			args:      `{"sql":"SELECT id, amount AS total FROM orders","dialect":"trino"}`,
			contains:  []string{`"id"`, `"total"`},
		},
		{
			name:      "referenced columns",
			operation: "referencedColumns",
			args:      `{"sql":"SELECT id FROM orders WHERE status = 'PAID'","dialect":"trino"}`,
			contains:  []string{`"orders"`, `"id"`, `"status"`},
		},
		{
			name:      "referenced column usages",
			operation: "referencedColumnUsages",
			args:      `{"sql":"SELECT id FROM orders WHERE status = 'PAID'","dialect":"trino"}`,
			contains:  []string{`"table":"orders"`, `"clause":"SELECT"`, `"clause":"WHERE"`},
		},
		{
			name:      "rewrite table references",
			operation: "rewriteTableReferences",
			args:      `{"sql":"SELECT id FROM sales.orders","dialect":"trino","specs":[{"matchKey":"sales.orders","inline":{"schema":"archive","table":"orders"}}]}`,
			contains:  []string{"archive", "orders"},
		},
		{
			name:      "rewrite table references with union",
			operation: "rewriteTableReferences",
			args:      `{"sql":"SELECT id FROM sales.orders","dialect":"trino","specs":[{"matchKey":"sales.orders","union":{"tableAlias":"orders","columns":["id"],"branches":[{"schema":"shard1","table":"orders"},{"schema":"shard2","table":"orders"}]}}]}`,
			contains:  []string{"UNION", "shard1", "shard2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := string(requireSuccess(t, executeRequest(t, test.operation, test.args)))
			for _, fragment := range test.contains {
				if !strings.Contains(data, fragment) {
					t.Fatalf("data %s does not contain %q", data, fragment)
				}
			}
		})
	}
}

func TestExecuteErrorContract(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		status int32
	}{
		{name: "empty", input: "", status: StatusInvalidArgument},
		{name: "malformed JSON", input: "not-json", status: StatusInvalidArgument},
		{name: "wrong ABI", input: `{"abiVersion":2,"operation":"parseColumns","args":{}}`, status: StatusInvalidArgument},
		{name: "missing operation", input: `{"abiVersion":1,"args":{}}`, status: StatusInvalidArgument},
		{name: "missing args", input: `{"abiVersion":1,"operation":"parseColumns"}`, status: StatusInvalidArgument},
		{name: "unknown operation", input: `{"abiVersion":1,"operation":"unknown","args":{}}`, status: StatusInvalidArgument},
		{name: "invalid args", input: `{"abiVersion":1,"operation":"parseColumns","args":{"sql":7}}`, status: StatusInvalidArgument},
		{name: "parse error", input: `{"abiVersion":1,"operation":"parseColumns","args":{"sql":"SELECT (","dialect":"trino"}}`, status: StatusParseError},
		{name: "unsupported", input: `{"abiVersion":1,"operation":"applyRowFilter","args":{"sql":"UPDATE t SET a = 1","whereClause":"a = 1","dialect":"mysql"}}`, status: StatusUnsupported},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := decodeResponse(t, Execute([]byte(test.input)))
			if response.Status != test.status {
				t.Fatalf("status = %d, want %d; error = %q", response.Status, test.status, response.Error)
			}
			if response.Error == "" {
				t.Fatal("error response has no message")
			}
		})
	}
}

func TestExecuteConcurrent(t *testing.T) {
	request := []byte(`{"abiVersion":1,"operation":"referencedColumns","args":{"sql":"SELECT id FROM orders WHERE status = 'PAID'","dialect":"trino"}}`)
	const goroutines = 32
	const callsPerGoroutine = 50

	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				var response decodedResponse
				if err := json.Unmarshal(Execute(request), &response); err != nil {
					errs <- err.Error()
					return
				}
				if response.Status != StatusSuccess {
					errs <- fmt.Sprintf("status %d: %s", response.Status, response.Error)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
