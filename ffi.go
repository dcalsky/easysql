// This file selects the native Polyglot SQL FFI shared library that matches the
// host platform. The path is an internal implementation detail: it is always
// the bundled artifact and cannot be overridden by the caller, so the loaded
// native code is guaranteed to match the pinned polyglot Go SDK.
//
// Prebuilt artifacts live in the repository's .ffi/ directory, one per platform,
// laid out as published by the polyglot-sql-ffi release:
//
//	.ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib
//	.ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so
//	...
//
// OpenBundledClient detects the current OS/architecture, finds the matching
// artifact and opens a polyglot.Client against it.

package easysql

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// ffiDirName is the name of the directory holding the per-platform artifacts.
const ffiDirName = ".ffi"

// versionCheckSkipEnv, when set to a non-empty value, disables the FFI/SDK
// version compatibility check performed by OpenBundledClient. It is an escape
// hatch for advanced users deliberately running a hand-built or otherwise
// non-standard native library; loading a mismatched library is unsupported and
// may crash or produce wrong results.
const versionCheckSkipEnv = "EASYSQL_SKIP_FFI_VERSION_CHECK"

// OpenBundledClient opens a polyglot.Client backed by the prebuilt FFI shared
// library bundled in the repository's .ffi/ directory, selected for the current
// operating system and architecture.
//
// Only the bundled artifact is ever loaded. The library path is fixed and not
// user-configurable, and there is no fallback search of arbitrary locations: if
// the host platform has no bundled artifact, opening fails (fail closed). This
// guarantees the loaded native code always matches the pinned polyglot Go SDK.
//
// The loaded library's version is verified against the pinned SDK (the version
// in go.mod); a mismatch is rejected fail closed, because the native FFI and the
// Go SDK share an ABI that is only guaranteed within the same release. Set
// EASYSQL_SKIP_FFI_VERSION_CHECK to bypass the check (unsupported).
//
// The caller owns the returned client's lifecycle and must Close it.
func OpenBundledClient() (*polyglot.Client, error) {
	path, err := BundledFFIPath()
	if err != nil {
		return nil, err
	}
	client, err := polyglot.Open(path)
	if err != nil {
		return nil, err
	}
	if err := verifyFFIVersion(client); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// verifyFFIVersion fails closed when the loaded native library's version does
// not match the pinned polyglot Go SDK version. polyglot.Version() is a
// compile-time constant baked into the SDK release named in go.mod, so it is the
// single source of truth: bumping the SDK there forces a matching .ffi artifact
// or this check trips. Set EASYSQL_SKIP_FFI_VERSION_CHECK to bypass (unsupported).
func verifyFFIVersion(client *polyglot.Client) error {
	if strings.TrimSpace(os.Getenv(versionCheckSkipEnv)) != "" {
		return nil
	}
	want := polyglot.Version()
	got, err := client.RuntimeVersion()
	if err != nil {
		return fmt.Errorf("easysql: cannot read native FFI version: %w", err)
	}
	if got != want {
		return fmt.Errorf(
			"easysql: FFI/SDK version mismatch: native library reports %q but the pinned polyglot SDK is %q; "+
				"update the bundled .ffi artifacts to match the SDK in go.mod (or set %s=1 to bypass, unsupported)",
			got, want, versionCheckSkipEnv,
		)
	}
	return nil
}

// BundledFFIPath returns the absolute path to the prebuilt Polyglot FFI shared
// library for the current OS/architecture inside the repository's .ffi/
// directory. It returns an error if the host platform has no bundled artifact
// or the .ffi/ directory cannot be located.
func BundledFFIPath() (string, error) {
	platform, err := ffiPlatformDir()
	if err != nil {
		return "", err
	}
	libName := ffiLibraryFileName()

	var tried []string
	for _, root := range ffiSearchRoots() {
		candidate := filepath.Join(root, ffiDirName, platform, libName)
		tried = append(tried, candidate)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"easysql: no bundled FFI library for %s/%s; looked for %s/%s/%s under %v",
		runtime.GOOS, runtime.GOARCH, ffiDirName, platform, libName, tried,
	)
}

// ffiPlatformDir maps the host GOOS/GOARCH to the .ffi/ subdirectory name used
// by the published artifacts (e.g. "polyglot-sql-ffi-macos-aarch64").
func ffiPlatformDir() (string, error) {
	var osName string
	switch runtime.GOOS {
	case "darwin":
		osName = "macos"
	case "linux":
		osName = "linux"
	case "windows":
		osName = "windows"
	default:
		return "", fmt.Errorf("easysql: no bundled FFI for OS %q", runtime.GOOS)
	}

	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", fmt.Errorf("easysql: no bundled FFI for architecture %q", runtime.GOARCH)
	}

	return fmt.Sprintf("polyglot-sql-ffi-%s-%s", osName, arch), nil
}

// ffiLibraryFileName returns the shared-library file name for the host OS,
// matching the polyglot SDK's own platform naming.
func ffiLibraryFileName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libpolyglot_sql_ffi.dylib"
	case "windows":
		return "polyglot_sql_ffi.dll"
	default:
		return "libpolyglot_sql_ffi.so"
	}
}

// ffiSearchRoots lists candidate directories that may contain the .ffi/ folder,
// walking up from both the working directory and this source file's directory so
// the lookup works regardless of where a test or binary is run from.
func ffiSearchRoots() []string {
	var roots []string
	seen := map[string]bool{}
	addAncestors := func(dir string) {
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		for {
			if !seen[dir] {
				seen[dir] = true
				roots = append(roots, dir)
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return
			}
			dir = parent
		}
	}

	if wd, err := os.Getwd(); err == nil {
		addAncestors(wd)
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		addAncestors(filepath.Dir(file))
	}
	return roots
}
