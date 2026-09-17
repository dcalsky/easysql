//go:build darwin && arm64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "libeasysql_engine.dylib"
	bundledRuntimeSHA256   = "b7c303e7cf70ebdd1798c10e03f4873873ddd05df7d39c53d91b9f7bfa6bf040"
)
