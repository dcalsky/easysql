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

## Parser limits

The bundled engine and Go SDK are pinned together at Polyglot **v0.11.0**.
All SQL entry points retain easysql's 1 MiB input byte budget. Parser recursion
and SQL nesting are now checked by the native engine (default logical parser
depth 1024; grouping nesting 512), replacing the former raw-text 64-bracket
heuristic. Brackets in strings, identifiers, and comments do not consume SQL
nesting. Unary/IF chains are also protected even without grouping brackets.
Input complexity failures are exposed as `ErrUnsupported`; ordinary syntax
errors are `ErrParse`. These limits use different units and are not equivalent
to the former 64-bracket threshold.

Release archives for all three supported platforms were checked against the
[v0.11.0 published checksums](https://github.com/tobilg/polyglot/releases/download/v0.11.0/checksums.sha256).
The per-library hashes remain pinned in `runtime_asset_*.go`.
