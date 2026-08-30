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
    PATH="${package_dir}/lib:${PATH}" "${binary}"
  else
    "${binary}"
  fi
)

missing_dir="${test_dir}/missing-runtime"
mkdir -p "${missing_dir}/bin" "${missing_dir}/lib"
cp "${binary}" "${missing_dir}/bin/"
cp "${package_dir}/lib/${library}" "${missing_dir}/lib/"
missing_binary="${missing_dir}/bin/$(basename "${binary}")"

(
  cd "${test_dir}"
  if [[ "${target_os}" == "windows" ]]; then
    PATH="${missing_dir}/lib:${PATH}" "${missing_binary}" --expect-runtime-error
  else
    "${missing_binary}" --expect-runtime-error
  fi
)
