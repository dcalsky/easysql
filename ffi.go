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
// The package loads this artifact lazily on first use (or eagerly via Init),
// detecting the current OS/architecture and verifying its version, so callers
// have nothing to wire up.

package easysql

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// ffiDirName is the name of the directory holding the per-platform artifacts.
const ffiDirName = ".ffi"

// versionCheckSkipEnv, when set to a non-empty value, disables the FFI/SDK
// version compatibility check performed when the bundled client is opened. It is
// an escape hatch for advanced users deliberately running a hand-built or
// otherwise non-standard native library; loading a mismatched library is
// unsupported and may crash or produce wrong results.
const versionCheckSkipEnv = "EASYSQL_SKIP_FFI_VERSION_CHECK"

// integrityCheckSkipEnv disables the pre-load digest check for advanced users
// deliberately replacing a bundled artifact. This is unsupported: native code
// is loaded into the current process, so bypassing integrity verification must
// be an explicit opt-in distinct from the version check.
const integrityCheckSkipEnv = "EASYSQL_SKIP_FFI_INTEGRITY_CHECK"

// bundledFFISHA256 pins the exact native artifacts shipped with this module.
// Checking before polyglot.Open is load-bearing: a post-load RuntimeVersion
// check cannot protect against a malicious library because its initialization
// code has already executed by then.
var bundledFFISHA256 = map[string]string{
	"polyglot-sql-ffi-macos-aarch64":  "175a83c0cb8be125cece09ca3a8bc40c982b9dfc2870ce97c0fdfd7b643adacb",
	"polyglot-sql-ffi-linux-x86_64":   "357f00115ccd3b276a8395e7072a1909b7a73f70c6ea6e636979932c7ec2c1ef",
	"polyglot-sql-ffi-windows-x86_64": "dae7e6bbc0f6afb390ff91472107b47999a5c508b94ac1963250f73961008ee9",
}

// The process-wide SQL engine. It is opened lazily, exactly once, the first time
// any API needs it (or eagerly via Init), and then shared by every call. The
// native library is safe for concurrent use, so a single shared client suffices
// and there is nothing for callers to wire up or close.
var (
	clientOnce   sync.Once
	sharedClient *polyglot.Client
	sharedErr    error
)

// Init eagerly loads and version-checks the bundled native SQL engine.
//
// It is optional: every API (ApplyRowFilter, LineageSourceColumns, ParseColumns,
// ReferencedColumns, ReferencedColumnUsages, …) initializes the engine lazily
// on first use, so the package works out of the box with no setup. Init exists
// so a long-running service can fail fast at startup instead of on its first
// query. It is idempotent and safe for concurrent use; repeated calls return
// the same result.
//
// Only the bundled artifact for the host OS/architecture is ever loaded — the
// path is fixed and not user-configurable — and its version must match the
// pinned polyglot Go SDK or initialization fails closed.
func Init() error {
	_, err := defaultClient()
	return err
}

// defaultClient returns the shared engine, opening it once on first use.
func defaultClient() (*polyglot.Client, error) {
	clientOnce.Do(func() {
		sharedClient, sharedErr = openBundledClient()
	})
	return sharedClient, sharedErr
}

// openBundledClient opens a polyglot.Client backed by the prebuilt FFI shared
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
func openBundledClient() (*polyglot.Client, error) {
	path, err := bundledFFIPath()
	if err != nil {
		return nil, err
	}
	if err := verifyBundledFFIIntegrity(path); err != nil {
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

// verifyBundledFFIIntegrity authenticates the selected artifact before any
// native code is loaded. ffiSearchRoots includes the working-directory ancestry
// for deployed binaries whose build-time module cache is unavailable, so path
// location alone is not a sufficient trust boundary.
func verifyBundledFFIIntegrity(path string) error {
	if strings.TrimSpace(os.Getenv(integrityCheckSkipEnv)) != "" {
		return nil
	}
	platform, err := ffiPlatformDir()
	if err != nil {
		return err
	}
	want, ok := bundledFFISHA256[platform]
	if !ok || want == "" {
		return fmt.Errorf("easysql: no trusted FFI digest for platform %q", platform)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("easysql: cannot open bundled FFI for integrity check: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("easysql: cannot hash bundled FFI: %w", err)
	}
	got := fmt.Sprintf("%x", h.Sum(nil))
	if got != want {
		return fmt.Errorf(
			"easysql: bundled FFI integrity check failed for %q: SHA-256 %s, want %s; "+
				"restore the artifact shipped with this module (or set %s=1 to bypass, unsupported)",
			path, got, want, integrityCheckSkipEnv,
		)
	}
	return nil
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

// bundledFFIPath returns the absolute path to the prebuilt Polyglot FFI shared
// library for the current OS/architecture inside the repository's .ffi/
// directory. It returns an error if the host platform has no bundled artifact
// or the .ffi/ directory cannot be located.
func bundledFFIPath() (string, error) {
	platform, err := ffiPlatformDir()
	if err != nil {
		return "", err
	}
	libName := ffiLibraryFileName()

	var tried []string
	for _, root := range ffiSearchRoots() {
		candidate := filepath.Join(root, ffiDirName, platform, libName)
		tried = append(tried, candidate)
		// Never follow a same-named symlink into an arbitrary native library.
		// Integrity is checked separately before the regular file is loaded.
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
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

// ffiSearchRoots lists candidate directories that may contain the .ffi/ folder.
// An absolute build-time source location is preferred over the runtime working
// directory so an adjacent, module-owned artifact wins whenever it is present.
// Working-directory ancestors remain a compatibility fallback for deployed
// binaries whose build-time module cache is unavailable; their artifacts still
// have to pass the pre-load digest check.
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

	if _, file, _, ok := runtime.Caller(0); ok {
		if filepath.IsAbs(file) {
			addAncestors(filepath.Dir(file))
		}
	}
	if wd, err := os.Getwd(); err == nil {
		addAncestors(wd)
	}
	return roots
}
