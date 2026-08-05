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

The CI workflow runs the consumer smoke test on Linux, macOS, and Windows:
[`goget-verify.yml`](../.github/workflows/goget-verify.yml).
