package easysql

import (
	"encoding/json"
	"errors"
	"testing"
)

type fakeRuntime struct {
	version  string
	execute  func([]byte) []byte
	closed   bool
	executed int
}

func (runtime *fakeRuntime) Version() (string, error) { return runtime.version, nil }

func (runtime *fakeRuntime) Execute(request []byte) ([]byte, error) {
	runtime.executed++
	return runtime.execute(request), nil
}

func (runtime *fakeRuntime) Close() error {
	runtime.closed = true
	return nil
}

func TestApplyRowFilterEncodesPublicOptions(t *testing.T) {
	runtime := &fakeRuntime{execute: func(raw []byte) []byte {
		var request struct {
			ABIVersion uint32 `json:"abiVersion"`
			Operation  string `json:"operation"`
			Args       struct {
				SQL          string   `json:"sql"`
				WhereClause  string   `json:"whereClause"`
				Dialect      string   `json:"dialect"`
				TableNames   []string `json:"tableNames"`
				TableRegexps []string `json:"tableRegexps"`
				DefaultDB    string   `json:"defaultDB"`
			} `json:"args"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ABIVersion != ABIVersion || request.Operation != "applyRowFilter" {
			t.Fatalf("unexpected request envelope: %+v", request)
		}
		if request.Args.SQL != "SELECT id FROM orders" || request.Args.WhereClause != "tenant_id = 7" {
			t.Fatalf("unexpected SQL arguments: %+v", request.Args)
		}
		if request.Args.Dialect != "postgres" || request.Args.DefaultDB != "warehouse" {
			t.Fatalf("unexpected scalar options: %+v", request.Args)
		}
		if len(request.Args.TableNames) != 1 || request.Args.TableNames[0] != "orders" {
			t.Fatalf("unexpected table names: %v", request.Args.TableNames)
		}
		if len(request.Args.TableRegexps) != 1 || request.Args.TableRegexps[0] != "^sales\\." {
			t.Fatalf("unexpected table regexps: %v", request.Args.TableRegexps)
		}
		return []byte(`{"abiVersion":1,"status":0,"data":"SELECT id FROM orders WHERE tenant_id = 7"}`)
	}}
	client := &Client{runtime: runtime}

	result, err := client.ApplyRowFilter(
		"SELECT id FROM orders",
		"tenant_id = 7",
		ApplyRowFilterOptions{
			Dialect:      "postgres",
			TableNames:   []string{"orders"},
			TableRegexps: []string{`^sales\.`},
			DefaultDB:    "warehouse",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != "SELECT id FROM orders WHERE tenant_id = 7" {
		t.Fatalf("result = %q", result)
	}
}

func TestTypedCallReturnsNativeStatusError(t *testing.T) {
	runtime := &fakeRuntime{execute: func([]byte) []byte {
		return []byte(`{"abiVersion":1,"status":2,"error":"parse failed"}`)
	}}
	client := &Client{runtime: runtime}

	_, err := client.ParseColumns("SELECT (")
	if !errors.Is(err, ErrParse) {
		t.Fatalf("error = %v, want ErrParse", err)
	}
	var nativeError *Error
	if !errors.As(err, &nativeError) || nativeError.Operation != "parseColumns" || nativeError.Message != "parse failed" {
		t.Fatalf("unexpected native error: %#v", nativeError)
	}
}

func TestClientCloseIsIdempotentAndStopsCalls(t *testing.T) {
	runtime := &fakeRuntime{version: "v0.10.5", execute: func([]byte) []byte { return nil }}
	client := &Client{runtime: runtime}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if !runtime.closed {
		t.Fatal("runtime was not closed")
	}
	if _, err := client.RuntimeVersion(); !errors.Is(err, ErrClosed) {
		t.Fatalf("RuntimeVersion error = %v, want ErrClosed", err)
	}
	if _, err := client.Execute(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Execute error = %v, want ErrClosed", err)
	}
}

func TestOptionsRejectMultipleValues(t *testing.T) {
	client := &Client{runtime: &fakeRuntime{execute: func([]byte) []byte { return nil }}}
	_, err := client.ParseColumns("SELECT 1", AnalysisOptions{}, AnalysisOptions{})
	if err == nil {
		t.Fatal("expected options error")
	}
}
