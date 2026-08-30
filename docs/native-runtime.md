# Native runtime

easysql embeds its platform-specific SQL engine. `Init` authenticates and loads
the embedded engine eagerly; every public API initializes the same process-wide
runtime lazily when `Init` was not called first.

Supported targets are macOS ARM64, Linux x86-64, and Windows x86-64. Unsupported
platforms and runtime-version mismatches fail initialization.

Before native code is loaded, its SHA-256 digest is checked against the artifact
pinned by this module version. The authenticated bytes are materialized into a
content-addressed user-private cache, so Go programs and native-library consumers
do not need a runtime sidecar or working-directory setup.

`EASYSQL_SKIP_FFI_VERSION_CHECK=1` bypasses that check and is unsupported.
`EASYSQL_SKIP_FFI_INTEGRITY_CHECK=1` separately bypasses the pre-load digest
check for deliberate custom builds and is also unsupported.

Controlled deployments may use `InitWithRuntimePath` to select an explicit
trusted engine artifact. The path is still subject to regular-file, SHA-256,
and version checks, and must be selected before any SQL API initializes the
process-wide client.
