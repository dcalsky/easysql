//go:build !(darwin && arm64) && !(linux && amd64) && !(windows && amd64)

package easysql

var bundledRuntimeArtifact []byte

const (
	bundledRuntimeFileName = ""
	bundledRuntimeSHA256   = ""
)
