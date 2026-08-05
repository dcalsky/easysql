# Native runtime

easysql uses the [Polyglot SQL](https://github.com/tobilg/polyglot) shared
library through PureGo; it does not use cgo. `Init` loads the bundled library
for the current platform. Every public API initializes the same runtime lazily
when `Init` was not called first.

| GOOS / GOARCH | Library |
| --- | --- |
| `darwin` / `arm64` | `.ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib` |
| `linux` / `amd64` | `.ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so` |
| `windows` / `amd64` | `.ffi/polyglot-sql-ffi-windows-x86_64/polyglot_sql_ffi.dll` |

The loaded library version is checked against the pinned Polyglot Go SDK.
Unsupported platforms and version mismatches fail initialization.

`EASYSQL_SKIP_FFI_VERSION_CHECK=1` bypasses that check and is unsupported.
