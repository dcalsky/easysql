//go:build linux && amd64

package easysql

import _ "embed"

//go:embed .ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so
var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = "libeasysql_engine.so"
	bundledRuntimeSHA256   = "357f00115ccd3b276a8395e7072a1909b7a73f70c6ea6e636979932c7ec2c1ef"
)
