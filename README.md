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
select * from a
-- output (where clause "user = 'alice'")
SELECT * FROM (SELECT * FROM a WHERE user = 'alice') AS a
```

Wrapping each table in its own pre-filtered subquery (instead of appending a
single top-level `WHERE`) keeps the rewrite correct across `JOIN` / `LEFT JOIN`
(no outer-join degradation), CTEs, subqueries and `UNION`. Only `SELECT` and set
operations are accepted; anything else is rejected fail-closed.

The rewrite operates on the parsed AST and re-emits the statement with the
engine's generator, so the **output is normalized**: keyword casing and
whitespace follow the generator's style and comments are not preserved.
Identifiers and string literals are carried through unchanged (e.g. a quoted
`"order"` stays quoted, a literal `'北京'` stays intact). The rewritten SQL is
always re-parsed before being returned, so a call either yields valid SQL or
fails closed (`ErrInternal`). When nothing is in scope — no table is rewritten —
the input is returned **unchanged** (no generator round-trip, so no
reformatting).

## Native library setup

The Go SDK calls a `polyglot-sql-ffi` shared library via PureGo (no cgo). You do
**not** need to download, build or install anything: the per-platform prebuilt
libraries (matching SDK version `v0.5.10`) ship *inside the module*, so `go get`
pulls in the one for your platform automatically. They live in the
[`.ffi/`](.ffi) directory, one per platform:

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

Install:

```bash
go get github.com/dcalsky/easysql@latest
```

That is the only setup step. The native engine ships with the module, so `go get`
pulls the matching library for your platform and it loads automatically on first
use — there is nothing else to download or install.

```go
// No setup: the bundled engine loads on first use.
out, err := easysql.ApplyRowFilter(`select "uid" from a`, "user = 'alice'", easysql.WithDialect("postgres"))
if err != nil { log.Fatal(err) }
// SELECT "uid" FROM (SELECT * FROM a WHERE "user" = 'alice') AS a
```

`ApplyRowFilter` is a one-shot, stateless call — there is no client to open or
constructor to keep around, mirroring `LineageSourceColumns` below. It is safe for
concurrent use. Call `easysql.Init()` at startup if you want to validate the
native engine eagerly instead of on first use.



### Options


| Option                     | Description                                                                     |
| -------------------------- | ------------------------------------------------------------------------------- |
| `WithDialect(d)`           | `mysql` (default), `starrocks`, `postgres`, `trino`.                            |
| `WithTableNames(names...)` | Restrict the WHERE clause to these tables (bare or `schema.table`).             |
| `WithTableRegexp(pats...)` | Restrict to tables matching any Go regexp. Composes with `WithTableNames`.      |
| `WithDefaultDB(db)`        | Schema used to resolve unqualified names against a qualified scope.             |
| `WithSelfCheck(b)`         | Deprecated no-op: the output is **always** re-parsed and the call fails closed (`ErrInternal`) if invalid, so self-checking can no longer be disabled. |


The `whereClause` is a boolean SQL expression spliced **verbatim** as an AST
node (not string-concatenated into the query), so binding and escaping any
values inside it is the caller's responsibility. Errors classify via `errors.Is`
against `ErrParse`, `ErrUnsupported`, `ErrInternal`; configuration mistakes
(empty `whereClause`, unknown dialect, bad table regexp, …) return a plain
error.

## Column-level lineage

For every root physical source table, `LineageSourceColumns` returns the
**source columns that flow into the statement's result** — columns appearing
only in filter positions (`WHERE`, `JOIN ... ON`, `GROUP BY`, `ORDER BY`, …) are
excluded. It mirrors the semantics of sqllineage's `get_source_table_columns`.

It analyzes **any** statement that contains a query: a bare `SELECT`/`UNION`,
as well as `CREATE VIEW`, `CREATE TABLE AS SELECT` and `INSERT ... SELECT`
(which are unwrapped to their inner query first). Every source table is listed,
even when no column flows from it.

```go
cols, err := easysql.LineageSourceColumns(`
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
client is concurrency-safe, so callers can invoke `LineageSourceColumns` directly
and layer their own caching if needed. `LineageSourceColumnsConcurrent` is a
drop-in variant with identical options, semantics and result that analyzes the
leaf `SELECT`s of a set operation (`UNION` / `INTERSECT` / `EXCEPT`) in parallel;
a single-`SELECT` statement takes the same serial path, so there is no goroutine
overhead on the common case.

## ParseColumns

`ParseColumns` returns the **field names a statement exposes**, in left-to-right
projection order. It is the complement of `LineageSourceColumns`: instead of
tracing which source-table columns flow into the result, it names the columns
that consumers of a view, table or query would see.

It accepts query-bearing statements (`SELECT`, `WITH`, `UNION`, `CREATE VIEW`,
`CREATE TABLE AS SELECT`, `INSERT ... SELECT`, …) as well as DDL that declares
columns directly (`CREATE TABLE (…)`, `CREATE TABLE … (LIKE …)`, `INSERT INTO t
(a, b) VALUES …`). Wrappers are unwrapped to their inner query; an explicit
column list on `CREATE VIEW`, `CREATE TABLE` or `INSERT` overrides names
inferred from the `SELECT` list.

```go
cols, err := easysql.ParseColumns(`
    CREATE VIEW hive.analytics.v AS
    SELECT u.user_name, o.amount AS amt
    FROM hive.raw.users u
    JOIN hive.raw.orders o ON u.user_id = o.user_id`,
    easysql.WithLineageDialect("trino"),
)
// []string{"user_name", "amt"}
```

On top of Polyglot's `AnalyzeQuery` projections:

- **Unaliased expressions** are named `_col{index}` (0-based position in the
  `SELECT` list), matching Trino's default naming.
- **Wildcards** (`SELECT *`, `t.*`) expand when `WithLineageMetadata` supplies
  table catalogs; without metadata an unexpanded star appears as `"*"`. For
  multi-table `SELECT *` every referenced base table must appear in metadata,
  otherwise the result falls back to `["*"]`.
- **`CREATE TABLE … (LIKE other)`** requires metadata for the source table;
  bare table names match metadata keys by suffix (e.g. `other` matches
  `c.s.other`).
- **Statically unknowable shapes** (`INSERT INTO t VALUES (…)` with no target
  column list) return `ErrUnsupported`.

Reuses the lineage options `WithLineageDialect` and `WithLineageMetadata`
(`WithLineageProducer` / `WithLineageNamespace` are accepted but do not affect
the result). See `parse_columns_test.go` for the full Trino scenario suite
(real-world views, CTAS, CTEs, quoted aliases, partial metadata, …).

## Design notes

The transform runs directly on Polyglot's JSON AST. Each `ApplyRowFilter` call
parses the input once, parses a wrapper-subquery template (with the predicate
baked into its `WHERE`) **once** and caches it, then deep-clones that template
and replaces each in-scope table node in place. The statement is rendered back
to SQL by the engine's generator, and the result is re-parsed as a final
validity gate. The number of FFI calls is therefore constant — it does not grow
with the table count.

Because the output comes from the generator, it is **normalized** rather than
byte-identical to the input: keyword casing and whitespace follow the
generator's style and comments are not preserved. Identifiers and string
literals are carried through unchanged. A call that wraps **no** table (nothing
in scope) skips the generator entirely and returns the input verbatim.

This implementation deliberately avoids several shortcuts a naive version would
take:

- **Physical tables only.** A node is wrapped only when it is a real table
identifier. Table-valued functions (`generate_series(...)`, `TABLE(...)`,
`UNNEST(...)`) are `function` nodes, never wrapped — so no invalid empty
derived-table alias is ever produced. `DUAL` is also skipped.
- **Quoting preserved.** The original table node is carried through the AST, so a
quoted reserved word like `"order"` stays quoted (a rewrite that drops quotes
would emit invalid SQL).
- **Schema-qualified columns rewritten.** When a `schema.table` reference is
wrapped under a synthesized (bare-name) alias, outer `schema.table.col`
references are rewritten to `table.col` so they keep resolving against the
derived table.
- **Scope-correct CTE detection.** CTE names are tracked on a scope stack during
the walk, not collected globally. A CTE named `t` in one branch does **not**
cause a real table `t` in a sibling branch to be left unfiltered — a security
bug a global CTE-name set would introduce. (`TestCTEScopeSecurity` is the
regression, and it has been verified to fail if the scoping is weakened.) CTE
*references* are never wrapped, only the real tables in or around them.
- **Predicate parsed once.** The WHERE expression is parsed into the wrapper
template and reused as an AST subtree, never concatenated into the final query
text. Binding/escaping of any values inside it is the caller's responsibility.



## Tests

```bash
go test ./...
```

Tests load the bundled engine the same way the API does (lazily on first use),
so the matching `.ffi/` artifact for the host platform is loaded automatically.
If no library can be resolved the
tests **skip** (they do not fail), so CI without the artifact stays green.

To verify the consumer experience end to end — that a project can build and run
with nothing but `go get` (the native library ships with the module and loads on
first use) — run the smoke test, which spins up a throwaway module against the
published version and executes a rewrite:

```bash
scripts/verify-goget.sh                            # latest published version
EASYSQL_VERIFY_VERSION=v0.1.0 scripts/verify-goget.sh
```

The same check runs on Linux, macOS and Windows in CI
([`.github/workflows/goget-verify.yml`](.github/workflows/goget-verify.yml)).

Tests are assertion-based, not "did not panic":

- `TestRewriteStructural` / `TestDialectBoundary` / `TestRewriteCorpus` — every
case must parse, rewrite, **re-parse as valid SQL**, have **no empty alias**, and
wrap exactly the expected number of physical tables; `TestRewriteCorpus` also
checks the physical-table multiset is preserved.
- `TestRewriteInvariants` / `FuzzRewrite` — external-oracle invariants (semantic
no-op, output validity, table-multiset preservation) over a broad dialect corpus
and as a fuzz target. `TestInputGuard` covers the deep-nesting / oversize
fail-closed guards.
- `TestCTEScopeSecurity` — the real-table-left-unfiltered regression.
- `TestSchemaStrip`, `TestQuotingPreserved`, `TestTableFunctionsNotWrapped`,
`TestDualSkipped`, `TestWhereSplicedVerbatim`, `TestErrors`,
`TestConfigValidation`, `TestSelfCheck`.
- `BenchmarkRewrite` — steady-state per-call rewrite cost over a representative
corpus (`go test -bench=. -run='^$'`).
- `TestTrino*` (in `lineage_test.go`) — the full set of column-lineage cases:
wildcard + `WHERE`, duplicate qualified columns, ambiguous bare columns,
expression/function columns, and CTE-to-root resolution.
- `TestParseColumns*` (in `parse_columns_test.go`) — column-name extraction
for `CREATE VIEW` / `CREATE TABLE` / `INSERT` / `WITH` / wildcard expansion,
ported from the Python `view_columns` suite.

