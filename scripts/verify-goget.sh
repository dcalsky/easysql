#!/usr/bin/env bash
#
# verify-goget.sh — prove the "open the box and it just works" promise.
#
# A brand-new project that does nothing but `go get github.com/dcalsky/easysql`
# must be able to call the API, because the matching native FFI library ships
# with the module and loads automatically on first use. This script creates a
# throwaway consumer module, wires it up the way a real user would, and runs a
# rewrite. It exits non-zero if the engine fails to load or misbehaves.
#
# Modes:
#   * Published (default): test the module as published on the proxy.
#       EASYSQL_VERIFY_VERSION   version to test (default: latest)
#   * Local checkout: test an unpublished working tree via a replace directive
#     (used by CI on pull requests, before a release is tagged).
#       EASYSQL_VERIFY_REPLACE   path to the easysql checkout
#
set -euo pipefail

module="github.com/dcalsky/easysql"
version="${EASYSQL_VERIFY_VERSION:-latest}"
replace="${EASYSQL_VERIFY_REPLACE:-}"

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
cd "$work"

cat > main.go <<'EOF'
package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/dcalsky/easysql"
)

func main() {
	out, err := easysql.ApplyRowFilter(
		`select "uid" from a`,
		"user = 'alice'",
		easysql.WithDialect("postgres"),
	)
	if err != nil {
		log.Fatalf("ApplyRowFilter failed — the native engine did not load via `go get`: %v", err)
	}
	if !strings.Contains(out, "WHERE") {
		log.Fatalf("unexpected rewrite output, native engine misbehaving: %q", out)
	}
	fmt.Println("go-get verification OK:", out)
}
EOF

go mod init easysql-goget-verify >/dev/null

if [ -n "$replace" ]; then
	replace="${replace//\\//}" # normalize Windows backslashes for the go.mod path
	echo "verifying local checkout via replace: $replace"
	go mod edit "-replace=${module}=${replace}"
	go mod edit "-require=${module}@v0.0.0"
else
	echo "verifying published module: ${module}@${version}"
	go get "${module}@${version}"
fi

GOFLAGS=-mod=mod go mod tidy
go run .
