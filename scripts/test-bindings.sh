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
    echo "unsupported binding test target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

package_dir="${EASYSQL_NATIVE_OUTPUT:-${repo_root}/dist/native/${platform}}"
library_path="${package_dir}/lib/${library}"
test_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${test_dir}"
}
trap cleanup EXIT

if find "${package_dir}/lib" "${package_dir}/include" "${package_dir}/bindings" \
  -type f -print | grep -Eqi '(^|[/\\])[^/\\]*polyglot'; then
  echo "internal engine name leaked into native package layout" >&2
  exit 1
fi

request='{"abiVersion":1,"operation":"applyRowFilter","args":{"sql":"SELECT id FROM orders","whereClause":"tenant_id = 7","dialect":"postgres"}}'

EASYSQL_LIBRARY_PATH="${library_path}" \
EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" \
EASYSQL_TEST_REQUEST="${request}" \
PYTHONDONTWRITEBYTECODE=1 \
PYTHONPATH="${package_dir}/bindings/python" \
python3 -c 'import json, os, easysql; response = easysql.execute(json.loads(os.environ["EASYSQL_TEST_REQUEST"])); assert response["status"] == 0 and "tenant_id = 7" in response["data"]'

mkdir -p "${test_dir}/javascript"
npm install --prefix "${test_dir}/javascript" --no-package-lock --no-save --silent 'koffi@^3.1.0'
EASYSQL_LIBRARY_PATH="${library_path}" \
EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" \
EASYSQL_TEST_REQUEST="${request}" \
EASYSQL_JS_BINDING="${package_dir}/bindings/javascript" \
NODE_PATH="${test_dir}/javascript/node_modules" \
node -e 'const api = require(process.env.EASYSQL_JS_BINDING); const response = api.execute(JSON.parse(process.env.EASYSQL_TEST_REQUEST)); if (response.status !== 0 || !response.data.includes("tenant_id = 7")) process.exit(1)'

if [[ "${target_os}" == "windows" ]]; then
  (cd "${package_dir}/bindings/go" && EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" PATH="${package_dir}/lib:${PATH}" go test ./...)
else
  (cd "${package_dir}/bindings/go" && EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" go test ./...)
fi

printf 'Python, JavaScript, and Go bindings OK: %s\n' "${package_dir}"
