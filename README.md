# easysql (Go + Polyglot)

easysql is a Go SQL rewriter built on the
[Polyglot SQL](https://github.com/tobilg/polyglot) engine — a multi-dialect,
sqlglot-compatible parser exposed to Go over an FFI library — giving it broad
dialect coverage (PostgreSQL, Trino/Presto, StarRocks, MySQL, …).

It rewrites `SELECT` statements to enforce row-level access policies: given a
boolean predicate, every reference to an in-scope **physical** table becomes a
filtered derived table.

```sql
-- input
SELECT * FROM a
-- output (where clause "user = 'alice'")
SELECT * FROM (SELECT * FROM a WHERE user = 'alice') AS a
```

Wrapping each table in its own pre-filtered subquery (instead of appending a
single top-level `WHERE`) keeps the rewrite correct across `JOIN` / `LEFT JOIN`
(no outer-join degradation), CTEs, subqueries and `UNION`. Only `SELECT` and set
operations are accepted; anything else is rejected fail-closed.

## Native library setup

The Go SDK calls a `polyglot-sql-ffi` shared library via PureGo (no cgo). The
per-platform prebuilt artifacts (matching SDK version `v0.5.10`) are vendored in
the repository's [`.ffi/`](.ffi) directory, one per platform:

```
.ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib
.ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so
.ffi/polyglot-sql-ffi-windows-x86_64/polyglot_sql_ffi.dll
```

There is **nothing to wire up**: the engine is loaded automatically the first
time you call any API. The package detects the host OS/architecture (via
`runtime.GOOS` / `runtime.GOARCH`), picks the matching `.ffi/` artifact and opens
it once for the process. The library path is **fixed and not configurable**:
only the bundled artifact is loaded, there is no environment override and no
fallback search of arbitrary locations. If the host platform has no bundled
artifact, the first call returns an error (fail closed) rather than loading some
other library. This keeps the loaded native code locked to the version this
module was built against.

The loaded library's version is also verified against the pinned polyglot Go SDK
(the version in `go.mod`) and fails closed on a mismatch — the native FFI and the
SDK share an ABI that is only guaranteed within the same release. The expected
version is `polyglot.Version()`, so it is impossible to silently drift: bump the
SDK in `go.mod` and the bundled `.ffi/` artifacts must be re-vendored to the same
version or it errors out (the `TestBundledFFIVersionMatchesSDK` test guards this
too). To bypass the version check for a deliberately hand-built library, set
`EASYSQL_SKIP_FFI_VERSION_CHECK=1` (unsupported).

Initialization is lazy by default. A long-running service that prefers to fail
fast at startup can call `easysql.Init()` once — it eagerly loads and
version-checks the engine, is idempotent, and is safe for concurrent use.


| GOOS / GOARCH       | `.ffi/` directory                 | Library file                |
| ------------------- | --------------------------------- | --------------------------- |
| `darwin` / `arm64`  | `polyglot-sql-ffi-macos-aarch64`  | `libpolyglot_sql_ffi.dylib` |
| `linux` / `amd64`   | `polyglot-sql-ffi-linux-x86_64`   | `libpolyglot_sql_ffi.so`    |
| `windows` / `amd64` | `polyglot-sql-ffi-windows-x86_64` | `polyglot_sql_ffi.dll`      |




## Usage

```go
// No setup: the bundled engine loads on first use.
out, err := easysql.ApplyRowFilter(`select "uid" from a`, "user = 'alice'", easysql.WithDialect("postgres"))
if err != nil { log.Fatal(err) }
// SELECT "uid" FROM (SELECT * FROM "a" WHERE user = 'alice') AS "a"
```

`ApplyRowFilter` is a one-shot, stateless call — there is no client to open or
constructor to keep around, mirroring `SourceTableColumns` below. It is safe for
concurrent use. Call `easysql.Init()` at startup if you want to validate the
native engine eagerly instead of on first use.



### Options


| Option                     | Description                                                                     |
| -------------------------- | ------------------------------------------------------------------------------- |
| `WithDialect(d)`           | `mysql` (default), `starrocks`, `postgres`, `trino`.                            |
| `WithTableNames(names...)` | Restrict the WHERE clause to these tables (bare or `schema.table`).             |
| `WithTableRegexp(pats...)` | Restrict to tables matching any Go regexp. Composes with `WithTableNames`.      |
| `WithDefaultDB(db)`        | Schema used to resolve unqualified names against a qualified scope.             |
| `WithSelfCheck(b)`         | Re-parse the output and fail closed (`ErrInternal`) if invalid. Off by default. |


The `whereClause` is a boolean SQL expression spliced **verbatim** as an AST
node (not string-concatenated into the query), so binding and escaping any
values inside it is the caller's responsibility. Errors classify via `errors.Is`
against `ErrParse`, `ErrUnsupported`, `ErrInternal`; configuration mistakes
(empty `whereClause`, unknown dialect, bad table regexp, …) return a plain
error.

## Column-level lineage

For every root physical source table, `SourceTableColumns` returns the
**source columns that flow into the statement's result** — columns appearing
only in filter positions (`WHERE`, `JOIN ... ON`, `GROUP BY`, `ORDER BY`, …) are
excluded. It mirrors the semantics of sqllineage's `get_source_table_columns`.

It analyzes **any** statement that contains a query: a bare `SELECT`/`UNION`,
as well as `CREATE VIEW`, `CREATE TABLE AS SELECT` and `INSERT ... SELECT`
(which are unwrapped to their inner query first). Every source table is listed,
even when no column flows from it.

```go
cols, err := easysql.SourceTableColumns(`
    CREATE VIEW hive.analytics.v_paid_orders AS
    WITH paid_orders AS (
        SELECT o.order_id, o.user_id, p.paid_amount
        FROM hive.raw.orders o
        JOIN hive.raw.payments p ON o.order_id = p.order_id
        WHERE o.status = 'PAID'
    )
    SELECT po.order_id, po.user_id, po.paid_amount
    FROM paid_orders po
    WHERE po.paid_amount > 0`,
    easysql.WithLineageDialect("trino"),
    easysql.WithLineageMetadata(map[string][]string{
        "hive.raw.orders":   {"order_id", "user_id", "amount", "status"},
        "hive.raw.payments": {"order_id", "paid_amount", "paid_at"},
    }),
)
// map[string][]string{
//   "hive.raw.orders":   {"order_id", "user_id"},
//   "hive.raw.payments": {"paid_amount"},
// }
```

It combines two Polyglot facilities: `AnalyzeQuery` base-table facts give the
root physical source tables (seeing through CTEs, subqueries and set
operations), and the OpenLineage column-lineage engine gives the per-column
flow. On top of that:

- **Wrappers are unwrapped.** `CREATE VIEW`, `CREATE TABLE AS` and
`INSERT ... SELECT` are reduced to their inner query before column analysis.
- **Set operations are split.** `UNION`/`INTERSECT`/`EXCEPT` branches are
analyzed individually and merged (the OpenLineage entrypoint rejects set
operations directly).
- **Ambiguous bare columns resolve to all candidates.** A bare column that the
engine cannot attribute (e.g. `SELECT user_id` from a join where both tables
have `user_id`) is credited to every source table that declares it in the
metadata, exactly as sqllineage does.

A differential harness (`lineage_diff_test.go` + the Python `sqllineage`
runner) cross-checks both implementations over a shared query set. On statements
with a write target (`CREATE VIEW` / `CREATE TABLE AS` / `INSERT ... SELECT`)
the two produce **identical** results. They differ by design on a bare
`SELECT`/`UNION`: `sqllineage` only traces columns *into a target* so it reports
empty column lists there, whereas easysql analyzes those statements too and
returns their source columns.

### Options


| Option                     | Description                                                                                                                             |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `WithLineageDialect(d)`    | Dialect used to parse/analyze (default `trino`). Accepts any Polyglot dialect, e.g. `trino`, `hive`, `spark`, `postgres`, `mysql`.      |
| `WithLineageMetadata(m)`   | `table -> columns` catalog (`"catalog.schema.table"` keys). Used to expand wildcards (`SELECT *`, `t.*`) and resolve ambiguous columns. |
| `WithLineageProducer(uri)` | OpenLineage `_producer` provenance URI recorded on emitted events. Pure metadata; does not affect the result. Defaults to the repo URL. |
| `WithLineageNamespace(ns)` | OpenLineage dataset namespace for emitted events. Pure provenance metadata; does not affect the result. Defaults to `easysql`.          |


There is no process pool or LRU cache: Polyglot runs in native code and the
client is concurrency-safe, so callers can invoke `SourceTableColumns` directly
and layer their own caching if needed. `SourceTableColumnsConcurrent` is a
drop-in variant with identical options, semantics and result that analyzes the
leaf `SELECT`s of a set operation (`UNION` / `INTERSECT` / `EXCEPT`) in parallel;
a single-`SELECT` statement takes the same serial path, so there is no goroutine
overhead on the common case.

## Design notes

The transform runs directly on Polyglot's JSON AST. Each `ApplyRowFilter` call parses
the WHERE expression and a wrapper skeleton **once**, then deep-clones and
splices them per table, so the number of FFI calls does not grow with the table
count.

This implementation deliberately avoids several shortcuts a naive version would
take:

- **Physical tables only.** A node is wrapped only when it is a real table
identifier. Table-valued functions (`generate_series(...)`, `TABLE(...)`,
`UNNEST(...)`) are `function` nodes, never wrapped — so no invalid empty
derived-table alias is ever produced. `DUAL` is also skipped.
- **Quoting preserved.** The original table node is spliced verbatim, so a
quoted reserved word like `"order"` stays quoted (a rewrite that drops quotes
would emit invalid SQL).
- **Scope-correct CTE detection.** CTE names are tracked on a scope stack during
the walk, not collected globally. A CTE named `t` in one branch does **not**
cause a real table `t` in a sibling branch to be left unfiltered — a security
bug a global CTE-name set would introduce. (`TestCTEScopeSecurity` is the
regression, and it has been verified to fail if the scoping is weakened.) CTE
*references* are never wrapped, only the real tables in or around them.
- **Verbatim splice.** The WHERE expression is parsed and spliced as an AST
node, not concatenated into the query text, so the surrounding statement is
never disturbed. Binding/escaping of any values inside it is the caller's
responsibility.



## Tests

```bash
go test ./...
```

Tests load the bundled engine the same way the API does (lazily on first use),
so the matching `.ffi/` artifact for the host platform is loaded automatically.
If no library can be resolved the
tests **skip** (they do not fail), so CI without the artifact stays green.

Tests are assertion-based, not "did not panic":

- `TestRewriteStructural` / `TestDialectBoundary` — every case must parse,
rewrite, **re-parse as valid SQL**, have **no empty alias**, and wrap exactly
the expected number of physical tables.
- `TestCTEScopeSecurity` — the real-table-left-unfiltered regression.
- `TestQuotingPreserved`, `TestTableFunctionsNotWrapped`, `TestDualSkipped`,
`TestUserLiteralEscaped`, `TestErrors`, `TestConfigValidation`,
`TestSelfCheck`.
- `TestTrino*` (in `lineage_test.go`) — the full set of column-lineage cases:
wildcard + `WHERE`, duplicate qualified columns, ambiguous bare columns,
expression/function columns, and CTE-to-root resolution.

