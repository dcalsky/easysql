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

Build the native C ABI package and run a compiled C consumer against it:

```bash
bash scripts/test-native.sh
```

The test builds the shared library, verifies that it finds the sibling Polyglot
runtime independently of the working directory, verifies that a missing sibling
fails closed, exercises success and error responses, and repeatedly checks
allocation/free ownership.

The CI workflow runs Go tests, the `go get` consumer smoke test, and the native
C ABI build/consumer test on Linux, macOS, and Windows:
[`goget-verify.yml`](../.github/workflows/goget-verify.yml).
