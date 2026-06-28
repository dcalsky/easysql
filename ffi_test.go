package easysql

import (
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// TestBundledFFIVersionMatchesSDK guards the core invariant enforced when the
// bundled client is opened: the bundled native library's version must equal the
// pinned polyglot Go SDK version (the one in go.mod). If this fails, the .ffi/
// artifacts are out of sync with the SDK and must be re-vendored to match.
func TestBundledFFIVersionMatchesSDK(t *testing.T) {
	got, err := testClient.RuntimeVersion()
	if err != nil {
		t.Fatalf("RuntimeVersion: %v", err)
	}
	if want := polyglot.Version(); got != want {
		t.Fatalf("bundled FFI version %q != pinned SDK version %q; "+
			"update the .ffi artifacts to match go.mod", got, want)
	}
}
