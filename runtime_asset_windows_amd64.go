//go:build windows && amd64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-windows-x86_64/polyglot_sql_ffi.dll
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "easysql_engine.dll"
	bundledRuntimeSHA256   = "4ba724884739b0a0d789dd0c774a3e3a8e886d3a6fd4edaf39ea3c79654c772f"
)
