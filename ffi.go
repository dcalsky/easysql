// This file initializes the private native SQL engine embedded in the easysql
// library. The engine is materialized into a content-addressed, user-private
// cache on first use and is never part of the public distribution layout.

package easysql

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

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

// engineCacheDirEnv overrides the root used to materialize the authenticated
// embedded engine. It is primarily useful for hermetic containers and tests.
const engineCacheDirEnv = "EASYSQL_ENGINE_CACHE_DIR"

// The process-wide SQL engine. It is opened lazily, exactly once, the first time
// any API needs it (or eagerly via Init), and then shared by every call. The
// native library is safe for concurrent use, so a single shared client suffices
// and there is nothing for callers to wire up or close.
var (
	clientOnce   sync.Once
	sharedClient *polyglot.Client
	sharedErr    error
)

// Init eagerly loads and version-checks the embedded native SQL engine.
//
// It is optional: every API (ApplyRowFilter, LineageSourceColumns, ParseColumns,
// ReferencedColumns, ReferencedColumnUsages, …) initializes the engine lazily
// on first use, so the package works out of the box with no setup. Init exists
// so a long-running service can fail fast at startup instead of on its first
// query. It is idempotent and safe for concurrent use; repeated calls return
// the same result.
//
// Only the embedded artifact for the host OS/architecture is loaded.
func Init() error {
	_, err := defaultClient()
	return err
}

// InitWithRuntimePath eagerly initializes the shared SQL engine from path.
//
// This advanced entry point supports controlled deployments that provide their
// own trusted engine file. Most applications should use Init. The file must be
// regular and pass the same SHA-256 and version checks as the embedded engine.
//
// Initialization is process-wide and first-call-wins, just like Init. Callers
// must invoke this before any SQL API when they need to select an explicit
// runtime location. It is idempotent and safe for concurrent use.
func InitWithRuntimePath(path string) error {
	clientOnce.Do(func() {
		sharedClient, sharedErr = openClient(path)
	})
	return sharedErr
}

// defaultClient returns the shared engine, opening it once on first use.
func defaultClient() (*polyglot.Client, error) {
	clientOnce.Do(func() {
		sharedClient, sharedErr = openBundledClient()
	})
	return sharedClient, sharedErr
}

// openBundledClient opens a client backed by the embedded, authenticated engine.
func openBundledClient() (*polyglot.Client, error) {
	path, err := bundledFFIPath()
	if err != nil {
		return nil, err
	}
	return openClient(path)
}

// openClient authenticates and opens an explicitly located engine runtime.
func openClient(path string) (*polyglot.Client, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("easysql: native runtime path must not be empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("easysql: cannot resolve native runtime path: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("easysql: cannot inspect native runtime: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("easysql: native runtime must be a regular file: %q", path)
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

// verifyBundledFFIIntegrity authenticates the selected artifact before native
// code is loaded.
func verifyBundledFFIIntegrity(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("easysql: cannot inspect native engine: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("easysql: native engine must be a regular file: %q", path)
	}
	if strings.TrimSpace(os.Getenv(integrityCheckSkipEnv)) != "" {
		return nil
	}
	want := bundledRuntimeSHA256
	if want == "" {
		return fmt.Errorf("easysql: no trusted native engine for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("easysql: cannot open native engine for integrity check: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("easysql: cannot hash native engine: %w", err)
	}
	got := fmt.Sprintf("%x", h.Sum(nil))
	if got != want {
		return fmt.Errorf(
			"easysql: native engine integrity check failed for %q: SHA-256 %s, want %s; "+
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
			"easysql: native engine version mismatch: runtime reports %q but this build requires %q; "+
				"restore the engine shipped with this build (or set %s=1 to bypass, unsupported)",
			got, want, versionCheckSkipEnv,
		)
	}
	return nil
}

// bundledFFIPath materializes the embedded runtime into a content-addressed,
// user-private cache. Atomic hard-link installation makes concurrent startup safe.
func bundledFFIPath() (string, error) {
	if len(bundledRuntimeArtifact) == 0 || bundledRuntimeFileName == "" || bundledRuntimeSHA256 == "" {
		return "", fmt.Errorf("easysql: no embedded native engine for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(bundledRuntimeArtifact))
	if got != bundledRuntimeSHA256 {
		return "", fmt.Errorf("easysql: embedded native engine integrity check failed: SHA-256 %s, want %s", got, bundledRuntimeSHA256)
	}

	cacheRoot := strings.TrimSpace(os.Getenv(engineCacheDirEnv))
	if cacheRoot == "" {
		defaultCacheRoot, cacheErr := os.UserCacheDir()
		if cacheErr != nil || strings.TrimSpace(defaultCacheRoot) == "" {
			cacheRoot = os.TempDir()
		} else {
			cacheRoot = defaultCacheRoot
		}
	}
	dir := filepath.Join(cacheRoot, "easysql", "engine", bundledRuntimeSHA256[:16])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("easysql: cannot create native engine cache: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("easysql: native engine cache is not a regular directory: %q", dir)
	}
	path := filepath.Join(dir, bundledRuntimeFileName)
	if err := verifyBundledFFIIntegrity(path); err == nil {
		return path, nil
	}

	tmp, err := os.CreateTemp(dir, ".easysql-engine-*"+filepath.Ext(bundledRuntimeFileName))
	if err != nil {
		return "", fmt.Errorf("easysql: cannot create native engine cache file: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("easysql: cannot secure native engine cache file: %w", err)
	}
	if _, err := tmp.Write(bundledRuntimeArtifact); err != nil {
		return "", fmt.Errorf("easysql: cannot write native engine cache file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("easysql: cannot sync native engine cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("easysql: cannot close native engine cache file: %w", err)
	}

	// A hard link installs the completed file without replacing a cache entry
	// another process may already be loading. If the filesystem does not support
	// hard links, the authenticated temporary file remains a valid private cache
	// entry for this process.
	if err := os.Link(tmpPath, path); err == nil {
		if err := verifyBundledFFIIntegrity(path); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := verifyBundledFFIIntegrity(path); err == nil {
		return path, nil
	}
	if err := verifyBundledFFIIntegrity(tmpPath); err != nil {
		return "", err
	}
	keep = true
	return tmpPath, nil
}
