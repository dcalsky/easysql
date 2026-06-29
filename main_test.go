package easysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// This file holds the shared test infrastructure for the whole package: the
// engine handle, TestMain, generic AST walkers and assertion helpers reused by
// the rewrite, invariant and benchmark tests. Test cases live in the focused
// easysql_*_test.go / lineage_*_test.go files.

// testClient is the shared engine, also used directly by tests for AST
// assertions. It is nil when the FFI library is unavailable (tests skip).
var testClient *polyglot.Client

func TestMain(m *testing.M) {
	// Initialize (and capture) the shared engine the same way the public API
	// does; tests also use this client directly for AST assertions.
	c, err := defaultClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "skipping easysql tests: polyglot FFI library not available:", err)
		fmt.Fprintln(os.Stderr, "ensure the matching .ffi/ artifact for this platform is present to run them")
		os.Exit(0)
	}
	testClient = c
	os.Exit(m.Run())
}

// walkJSON visits every object in a decoded-JSON tree.
func walkJSON(node any, fn func(map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		fn(v)
		for _, c := range v {
			walkJSON(c, fn)
		}
	case []any:
		for _, c := range v {
			walkJSON(c, fn)
		}
	}
}

// tableNameCounts counts physical-table references by name in sql's AST. It uses
// the same structural discriminator as identName (a table node's "table".name is
// itself an identifier object, while a column qualifier's is a bare string) but
// is otherwise an independent traversal: no scope, no ordering, counts every
// table reference. Used as an external oracle that no table is dropped,
// duplicated, renamed or corrupted by a rewrite.
func tableNameCounts(t *testing.T, sql, pg string) (map[string]int, bool) {
	t.Helper()
	raw, err := testClient.ParseOne(sql, pg)
	if err != nil {
		return nil, false
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, false
	}
	counts := map[string]int{}
	walkJSON(node, func(m map[string]any) {
		if tb, ok := m["table"].(map[string]any); ok {
			if name := identName(tb["name"]); name != "" {
				counts[strings.ToLower(name)]++
			}
		}
	})
	return counts, true
}

// regenTableCounts returns the physical-table multiset of sql after a single
// parse+generate cycle. The rewrite emits its output through the generator, so
// this is the fair multiset baseline: it excludes inputs the engine cannot
// round-trip faithfully (adversarial fuzz garbage), whose quirks would otherwise
// show up only on one side of the comparison. ok is false if the cycle fails.
func regenTableCounts(t *testing.T, sql, pg string) (map[string]int, bool) {
	t.Helper()
	raw, err := testClient.Parse(sql, pg)
	if err != nil {
		return nil, false
	}
	gen, err := testClient.Generate(raw, pg)
	if err != nil || len(gen) != 1 {
		return nil, false
	}
	return tableNameCounts(t, gen[0], pg)
}

// sentinelErr reports whether err is one of the package's classified errors.
func sentinelErr(err error) bool {
	return errors.Is(err, ErrParse) || errors.Is(err, ErrUnsupported) || errors.Is(err, ErrInternal)
}

// errTimeout marks a call that did not return in time. The underlying polyglot
// Parse can hang on certain adversarial input (an upstream sqlglot bug); a Go
// timeout lets the harness keep running, though the native call leaks. Treated
// as a tolerated outcome rather than a rewrite bug.
var errTimeout = errors.New("apply timed out")

// applyTimed runs ApplyRowFilter under a wall-clock budget so a native hang in a
// dependency cannot wedge the test/fuzz run.
func applyTimed(sql, where string, opts ...Option) (string, error) {
	type res struct {
		s string
		e error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{"", errors.New("panic")}
			}
		}()
		s, e := ApplyRowFilter(sql, where, opts...)
		ch <- res{s, e}
	}()
	select {
	case r := <-ch:
		return r.s, r.e
	case <-time.After(3 * time.Second):
		return "", errTimeout
	}
}

// mapsEqual reports whether two string->int maps are equal.
func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
