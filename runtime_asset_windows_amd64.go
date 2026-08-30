//go:build windows && amd64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-windows-x86_64/polyglot_sql_ffi.dll
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "easysql_engine.dll"
	bundledRuntimeSHA256   = "dae7e6bbc0f6afb390ff91472107b47999a5c508b94ac1963250f73961008ee9"
)
