#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"

case "${target_os}/${target_arch}" in
  darwin/arm64)
    platform="macos-aarch64"
    ;;
  linux/amd64)
    platform="linux-x86_64"
    ;;
  windows/amd64)
    platform="windows-x86_64"
    ;;
  *)
    echo "unsupported native test target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

package_dir="${EASYSQL_NATIVE_OUTPUT:-${repo_root}/dist/native/${platform}}"
EASYSQL_NATIVE_OUTPUT="${package_dir}" bash "${repo_root}/scripts/build-native.sh"

compiler="$(go env CC)"
binary="${package_dir}/bin/easysql-native-smoke"
mkdir -p "${package_dir}/bin"
test_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${test_dir}"
  rm -f "${binary}"
  rmdir "${package_dir}/bin" 2>/dev/null || true
}
trap cleanup EXIT

case "${target_os}" in
  darwin)
    "${compiler}" "${repo_root}/native/tests/smoke.c" \
      -I"${package_dir}/include" -L"${package_dir}/lib" -leasysql \
      -Wl,-rpath,@loader_path/../lib -o "${binary}"
    ;;
  linux)
    "${compiler}" "${repo_root}/native/tests/smoke.c" \
      -I"${package_dir}/include" -L"${package_dir}/lib" -leasysql \
      '-Wl,-rpath,$ORIGIN/../lib' -o "${binary}"
    ;;
  windows)
    binary="${binary}.exe"
    "${compiler}" "${repo_root}/native/tests/smoke.c" \
      -I"${package_dir}/include" -L"${package_dir}/lib" -leasysql -o "${binary}"
    ;;
esac

(
  cd "${test_dir}"
  if [[ "${target_os}" == "windows" ]]; then
    EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" \
      PATH="${package_dir}/lib:${PATH}" "${binary}"
  else
    EASYSQL_ENGINE_CACHE_DIR="${test_dir}/engine-cache" "${binary}"
  fi
)

bash "${repo_root}/scripts/test-bindings.sh"
EASYSQL_NATIVE_OUTPUT="${package_dir}" bash "${repo_root}/scripts/test-go-sdk.sh"
