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
    echo "unsupported native release target: ${target_os}/${target_arch}" >&2
    exit 1
    ;;
esac

native_version="${EASYSQL_NATIVE_VERSION:-}"
if [[ -z "${native_version}" ]]; then
  native_version="$(git -C "${repo_root}" describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
if [[ ! "${native_version}" =~ ^[0-9A-Za-z._+-]+$ ]]; then
  echo "invalid EASYSQL_NATIVE_VERSION: use only letters, digits, dot, underscore, plus, and hyphen" >&2
  exit 1
fi

package_dir="${EASYSQL_NATIVE_OUTPUT:-${repo_root}/dist/native/${platform}}"
release_dir="${EASYSQL_RELEASE_OUTPUT:-${repo_root}/dist/release}"
package_name="easysql-native-${platform}-${native_version}"
archive="${release_dir}/${package_name}.tar.gz"

for directory in include lib; do
  if [[ ! -d "${package_dir}/${directory}" ]]; then
    echo "missing native SDK directory: ${package_dir}/${directory}" >&2
    exit 1
  fi
done

staging_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${staging_dir}"
}
trap cleanup EXIT

mkdir -p "${staging_dir}/${package_name}" "${release_dir}"
for directory in include lib; do
  cp -R "${package_dir}/${directory}" "${staging_dir}/${package_name}/${directory}"
done

tar -C "${staging_dir}" -czf "${archive}" "${package_name}"
printf 'created binary-only native SDK archive: %s\n' "${archive}"
