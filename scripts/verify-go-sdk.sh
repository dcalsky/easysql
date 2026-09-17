#!/usr/bin/env bash
# Verify the public Go SDK from a clean consumer module. In local mode the
# caller supplies a module replacement and a freshly built native library. In
# published mode the script downloads both the Go module and matching release
# archive by version.
set -euo pipefail

module="github.com/dcalsky/easysql/packages/go"
repository="dcalsky/easysql"
version="${EASYSQL_VERIFY_VERSION:-latest}"
replace="${EASYSQL_VERIFY_REPLACE:-}"
library_path="${EASYSQL_VERIFY_LIBRARY:-}"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"

case "${target_os}/${target_arch}" in
  darwin/arm64)
    platform="macos-aarch64"
    library="libeasysql.dylib"
    ;;
  linux/amd64)
    platform="linux-x86_64"
    library="libeasysql.so"
    ;;
  windows/amd64)
    platform="windows-x86_64"
    library="easysql.dll"
    ;;
  *)
    echo "unsupported Go SDK verification target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

work="$(mktemp -d)"
cleanup() {
  rm -rf "${work}"
}
trap cleanup EXIT
cd "${work}"

cat > main.go <<'EOF'
package main

import (
	"fmt"
	"log"
	"strings"

	easysql "github.com/dcalsky/easysql/packages/go"
)

func main() {
	client, err := easysql.OpenDefault()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	query, err := client.ApplyRowFilter(
		"SELECT id FROM orders",
		"tenant_id = 7",
		easysql.ApplyRowFilterOptions{Dialect: "postgres"},
	)
	if err != nil {
		log.Fatal(err)
	}
	if !strings.Contains(query, "WHERE") || !strings.Contains(query, "tenant_id") {
		log.Fatalf("unexpected rewrite: %q", query)
	}

	lineage, err := client.LineageSourceColumns(
		"SELECT id FROM orders",
		easysql.AnalysisOptions{
			Dialect:  "trino",
			Metadata: map[string][]string{"orders": {"id"}},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	if columns := lineage["orders"]; len(columns) != 1 || columns[0] != "id" {
		log.Fatalf("unexpected lineage: %v", lineage)
	}

	version, err := client.RuntimeVersion()
	if err != nil || version == "" {
		log.Fatalf("runtime version: %q, %v", version, err)
	}
	fmt.Println("Go SDK verification OK:", easysql.Version(), version, query)
}
EOF

go mod init easysql-sdk-verify >/dev/null
if [[ -n "${replace}" ]]; then
  if [[ -z "${library_path}" || ! -f "${library_path}" ]]; then
    echo "EASYSQL_VERIFY_LIBRARY must name the built native library in local mode" >&2
    exit 1
  fi
  replace="${replace//\\//}"
  go mod edit "-replace=${module}=${replace}"
  go mod edit "-require=${module}@v0.0.0"
else
  if [[ "${version}" == "latest" ]]; then
    version="$(gh release view --repo "${repository}" --json tagName --jq .tagName)"
  fi
  go get "${module}@${version}"
  archive="easysql-native-${platform}-${version}.tar.gz"
  gh release download "${version}" --repo "${repository}" --pattern "${archive}" --dir "${work}/download"
  tar -C "${work}" -xzf "${work}/download/${archive}"
  library_path="${work}/easysql-native-${platform}-${version}/lib/${library}"
fi

GOFLAGS=-mod=mod go mod tidy
EASYSQL_LIBRARY_PATH="${library_path}" CGO_ENABLED=0 go run .
