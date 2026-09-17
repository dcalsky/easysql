//go:build linux && amd64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "libeasysql_engine.so"
	bundledRuntimeSHA256   = "52d5362b80964bd8f6497e861215bf02e8ede5ab8efed05bd59947a53d1a60ad"
)
