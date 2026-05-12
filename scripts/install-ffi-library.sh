#!/usr/bin/env bash
set -euo pipefail

library_path="${1:-}"
goos="${2:-$(go env GOOS)}"
goarch="${3:-$(go env GOARCH)}"

case "${goos}" in
  darwin)
    library_name="libmonty_ffi.dylib"
    ;;
  linux)
    library_name="libmonty_ffi.so"
    ;;
  *)
    echo "unsupported GOOS for embedded FFI library: ${goos}" >&2
    exit 1
    ;;
esac

case "${goarch}" in
  amd64 | arm64)
    ;;
  *)
    echo "unsupported GOARCH for embedded FFI library: ${goarch}" >&2
    exit 1
    ;;
esac

if [[ -z "${library_path}" ]]; then
  library_path="target/release/${library_name}"
fi

if [[ ! -f "${library_path}" ]]; then
  echo "FFI library not found: ${library_path}" >&2
  exit 1
fi

dest_dir="internal/ffi/lib/${goos}_${goarch}"
dest="${dest_dir}/${library_name}.gz"
mkdir -p "${dest_dir}"
gzip -n -9 -c "${library_path}" > "${dest}"
echo "installed ${dest}"
