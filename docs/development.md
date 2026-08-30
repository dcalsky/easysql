# Development

Run the test suite:

```bash
go test ./...
go test -race ./...
```

Run the end-to-end consumer smoke test:

```bash
scripts/verify-goget.sh
EASYSQL_VERIFY_VERSION=v0.1.0 scripts/verify-goget.sh
```

For an unpublished checkout:

```bash
EASYSQL_VERIFY_REPLACE="$PWD" scripts/verify-goget.sh
```

Build the native SDK and run C, Python, JavaScript, and Go consumers against it:

```bash
bash scripts/test-native.sh
```

The test builds the single shared library with its embedded engine, exercises
success and error responses, repeatedly checks allocation/free ownership, and
runs all three supported language bindings. It also rejects internal engine
names in the public library, header, and binding layout.

The CI workflow runs Go tests, the `go get` consumer smoke test, and the native
C ABI build/consumer test on Linux, macOS, and Windows:
[`goget-verify.yml`](../.github/workflows/goget-verify.yml).
