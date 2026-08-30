//go:build darwin && arm64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "libeasysql_engine.dylib"
	bundledRuntimeSHA256   = "175a83c0cb8be125cece09ca3a8bc40c982b9dfc2870ce97c0fdfd7b643adacb"
)
