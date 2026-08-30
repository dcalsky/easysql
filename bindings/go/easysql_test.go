package easysql

import (
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	request := map[string]any{
		"abiVersion": 1,
		"operation":  "applyRowFilter",
		"args": map[string]any{
			"sql":         "SELECT id FROM orders",
			"whereClause": "tenant_id = 7",
			"dialect":     "postgres",
		},
	}
	var response struct {
		Status int    `json:"status"`
		Data   string `json:"data"`
	}
	if err := ExecuteJSON(request, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != 0 || !strings.Contains(response.Data, "tenant_id = 7") {
		t.Fatalf("unexpected response: %+v", response)
	}
}
