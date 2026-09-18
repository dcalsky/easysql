#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
    echo "unsupported Go SDK test target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

package_dir="${EASYSQL_NATIVE_OUTPUT:-${repo_root}/dist/native/${platform}}"
library_path="${package_dir}/lib/${library}"
if [[ ! -f "${library_path}" ]]; then
  echo "missing native SDK library: ${library_path}" >&2
  exit 1
fi

test_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${test_dir}"
}
trap cleanup EXIT

(
  cd "${repo_root}/packages/go"
  EASYSQL_NATIVE_TEST_LIBRARY="${library_path}" \
  EASYSQL_REQUIRE_NATIVE_TEST=1 \
  EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" \
  CGO_ENABLED=0 \
  go test ./...
  EASYSQL_NATIVE_TEST_LIBRARY="${library_path}" \
  EASYSQL_REQUIRE_NATIVE_TEST=1 \
  EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" \
  go test -race ./...
)

EASYSQL_VERIFY_REPLACE="${repo_root}/packages/go" \
EASYSQL_VERIFY_LIBRARY="${library_path}" \
EASYSQL_ENGINE_CACHE_DIR="${test_dir}/consumer-engine-cache" \
bash "${repo_root}/scripts/verify-go-sdk.sh"

printf 'Go native SDK package and consumer OK: %s\n' "${library_path}"
