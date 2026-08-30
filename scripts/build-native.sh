#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"

case "${target_os}/${target_arch}" in
  darwin/arm64)
    platform="macos-aarch64"
    easysql_library="libeasysql.dylib"
    ;;
  linux/amd64)
    platform="linux-x86_64"
    easysql_library="libeasysql.so"
    ;;
  windows/amd64)
    platform="windows-x86_64"
    easysql_library="easysql.dll"
    ;;
  *)
    echo "unsupported native target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

package_dir="${EASYSQL_NATIVE_OUTPUT:-${repo_root}/dist/native/${platform}}"
lib_dir="${package_dir}/lib"
include_dir="${package_dir}/include"
license_dir="${package_dir}/licenses"
bindings_dir="${package_dir}/bindings"

mkdir -p "${lib_dir}" "${include_dir}" "${license_dir}" "${bindings_dir}"
cp -f "${repo_root}/native/include/easysql.h" "${include_dir}/easysql.h"
rm -rf "${bindings_dir}/python" "${bindings_dir}/javascript" "${bindings_dir}/go"
cp -R "${repo_root}/bindings/python" "${bindings_dir}/python"
cp -R "${repo_root}/bindings/javascript" "${bindings_dir}/javascript"
cp -R "${repo_root}/bindings/go" "${bindings_dir}/go"

# Remove obsolete sidecar names from an output directory produced by an older
# build. The engine is embedded in the easysql library now.
rm -f \
  "${lib_dir}/libpolyglot_sql_ffi.dylib" \
  "${lib_dir}/libpolyglot_sql_ffi.so" \
  "${lib_dir}/polyglot_sql_ffi.dll"

native_version="${EASYSQL_NATIVE_VERSION:-}"
if [[ -z "${native_version}" ]]; then
  native_version="$(git -C "${repo_root}" describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
if [[ ! "${native_version}" =~ ^[0-9A-Za-z._+-]+$ ]]; then
  echo "invalid EASYSQL_NATIVE_VERSION: use only letters, digits, dot, underscore, plus, and hyphen" >&2
  exit 1
fi

cd "${repo_root}"

go build \
  -trimpath \
  -buildvcs=false \
  -buildmode=c-shared \
  -ldflags="-s -w -X github.com/dcalsky/easysql/internal/nativeffi.LibraryVersion=${native_version}" \
  -o "${lib_dir}/${easysql_library}" \
  ./cmd/easysqlffi

generated_header="${lib_dir}/${easysql_library%.*}.h"
if [[ -f "${generated_header}" ]]; then
  rm -f "${generated_header}"
fi

if [[ "${target_os}" == "darwin" ]]; then
  install_name_tool -id "@rpath/${easysql_library}" "${lib_dir}/${easysql_library}"
fi
if [[ "${target_os}" == "windows" ]]; then
  compiler="$(go env CC)"
  dlltool="$("${compiler}" -print-prog-name=dlltool)"
  if command -v cygpath >/dev/null 2>&1 && [[ "${dlltool}" =~ ^[A-Za-z]: ]]; then
    dlltool="$(cygpath -u "${dlltool}")"
  fi
  "${dlltool}" \
    -D "${easysql_library}" \
    -d "${repo_root}/native/easysql.def" \
    -l "${lib_dir}/libeasysql.dll.a"
fi

index="${license_dir}/INDEX.txt"
: > "${index}"
while IFS='|' read -r module version module_dir; do
  [[ -z "${module}" ]] && continue
  if [[ "${target_os}" == "windows" ]] && command -v cygpath >/dev/null 2>&1; then
    module_dir="$(cygpath -u "${module_dir}")"
  fi
  license="$(find "${module_dir}" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) | sort | head -n 1)"
  if [[ -z "${license}" ]]; then
    echo "missing license file for ${module}@${version}" >&2
    exit 1
  fi
  safe_name="$(printf '%s@%s' "${module}" "${version}" | tr '/:' '__')"
  cp -f "${license}" "${license_dir}/${safe_name}.txt"
  printf '%s@%s -> %s.txt\n' "${module}" "${version}" "${safe_name}" >> "${index}"
done < <(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' ./cmd/easysqlffi | sort -u)

printf 'built native package: %s\n' "${package_dir}"
