# easysql

Go library for SQL analysis and rewriting, built on the
[Polyglot SQL](https://github.com/tobilg/polyglot) engine (multi-dialect,
sqlglot-compatible parser over FFI).

Four capabilities, one dependency:

| Function | Purpose |
| -------- | ------- |
| [`ApplyRowFilter`](#applyrowfilter) | Rewrite `SELECT` statements to enforce row-level access policies |
| [`LineageSourceColumns`](#lineagesourcecolumns) | Trace which source-table columns flow into a query's result |
| [`ParseColumns`](#parsecolumns) | List the column names a statement exposes |
| [`ReferencedColumns`](#referencedcolumns) | List every column a statement touches (projection + filters), per table |

Supported dialects include PostgreSQL, Trino/Presto, StarRocks, and MySQL.

## Installation

```bash
go get github.com/dcalsky/easysql@latest
```

No extra setup. A prebuilt native library for your platform ships inside the
module (`.ffi/`) and loads automatically on the first API call. Optional
startup validation:

```go
if err := easysql.Init(); err != nil { log.Fatal(err) }
```

See [Native engine](#native-engine) for platform matrix and version pinning.

## Quick start

```go
package main

import (
    "log"

    "github.com/dcalsky/easysql"
)

func main() {
    out, err := easysql.ApplyRowFilter(
        `select "uid" from a`,
        `user = 'alice'`,
        easysql.WithDialect("postgres"),
    )
    if err != nil {
        log.Fatal(err)
    }
    // SELECT "uid" FROM (SELECT * FROM a WHERE "user" = 'alice') AS a
    _ = out
}
```

All public functions are stateless and safe for concurrent use.

## API

### ApplyRowFilter

Rewrites a `SELECT` (or set operation) so every in-scope **physical** table is
wrapped in a derived table pre-filtered by a caller-supplied predicate:

```sql
-- input
select * from a

-- output  (whereClause = "user = 'alice'")
SELECT * FROM (SELECT * FROM a WHERE user = 'alice') AS a
```

Each table gets its own subquery rather than a single appended `WHERE`, which
keeps the rewrite correct across `JOIN`, `LEFT JOIN`, CTEs, subqueries, and
`UNION`. Only `SELECT` and set operations are accepted; anything else returns
`ErrUnsupported`.

The output is **normalized** by the SQL generator (casing, whitespace); comments
are dropped. Identifiers and literals are preserved. When no table is in scope the
input is returned unchanged.

```go
out, err := easysql.ApplyRowFilter(sql, whereClause,
    easysql.WithDialect("trino"),
    easysql.WithTableNames("orders", "sales.orders"),
)
```

| Option | Description |
| ------ | ----------- |
| `WithDialect(d)` | `mysql` (default), `starrocks`, `postgres`, `trino` |
| `WithTableNames(...)` | Restrict rewriting to these tables (bare or `schema.table`) |
| `WithTableRegexp(...)` | Restrict to tables matching a Go regexp; composes with `WithTableNames` |
| `WithDefaultDB(db)` | Schema for resolving unqualified table names |
| `WithSelfCheck(bool)` | Deprecated no-op; output is always re-parsed |

`whereClause` is spliced verbatim into the AST — the caller is responsible for
binding and escaping any values inside it.

### LineageSourceColumns

Returns, for each root physical source table, the sorted list of **source columns
that reach the query result**. Columns used only in filters (`WHERE`, `JOIN … ON`,
`GROUP BY`, `ORDER BY`, …) are excluded. Semantics align with sqllineage's
`get_source_table_columns`.

Works on any statement that contains a query — bare `SELECT`/`UNION`, as well as
`CREATE VIEW`, `CREATE TABLE AS SELECT`, and `INSERT … SELECT` (unwrapped to
their inner query). Every source table appears in the result, even when no column
flows from it.

```go
cols, err := easysql.LineageSourceColumns(`
    CREATE VIEW hive.analytics.v AS
    SELECT o.order_id, p.paid_amount
    FROM hive.raw.orders o
    JOIN hive.raw.payments p ON o.order_id = p.order_id
    WHERE o.status = 'PAID'`,
    easysql.WithLineageDialect("trino"),
    easysql.WithLineageMetadata(map[string][]string{
        "hive.raw.orders":   {"order_id", "user_id", "amount", "status"},
        "hive.raw.payments": {"order_id", "paid_amount", "paid_at"},
    }),
)
// map[string][]string{
//   "hive.raw.orders":   {"order_id"},
//   "hive.raw.payments": {"paid_amount"},
// }
```

| Option | Description |
| ------ | ----------- |
| `WithLineageDialect(d)` | Parse/analyze dialect (default `trino`) |
| `WithLineageMetadata(m)` | `table → columns` catalog for wildcard expansion and ambiguous-column resolution |
| `WithLineageProducer(uri)` | OpenLineage provenance metadata only; does not affect the result |
| `WithLineageNamespace(ns)` | OpenLineage provenance metadata only; does not affect the result |

`LineageSourceColumnsConcurrent` is a drop-in variant that analyzes `UNION` /
`INTERSECT` / `EXCEPT` branches in parallel. Single-`SELECT` statements take
the same serial path with no goroutine overhead.

### ParseColumns

Returns the **column names a statement exposes**, in projection order. This is
the complement of `LineageSourceColumns`: it names what consumers of a view,
table, or query would see, rather than tracing lineage back to source tables.

Handles `SELECT`, `WITH`, `UNION`, `CREATE VIEW`, `CREATE TABLE AS SELECT`,
`INSERT … SELECT`, `CREATE TABLE (…)`, `CREATE TABLE … (LIKE …)`, and
`INSERT INTO t (cols) VALUES …`. An explicit column list on `CREATE VIEW`,
`CREATE TABLE`, or `INSERT` overrides names inferred from the `SELECT` list.

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

| Behavior | Detail |
| -------- | ------ |
| Unaliased expressions | Named `_col{index}` (Trino convention) |
| `SELECT *` / `t.*` | Expanded via `WithLineageMetadata`; table absent from metadata → `["*"]`; table present with `[]` → `[]` |
| Multi-table `SELECT *` | All base tables must be in metadata, else `["*"]` |
| `CREATE TABLE … (LIKE t)` | Requires metadata for the source table |
| `INSERT INTO t VALUES (…)` without column list | `ErrUnsupported` |

Reuses `WithLineageDialect` and `WithLineageMetadata` from the lineage options.

### ReferencedColumns

`ReferencedColumns` returns, for each root physical table, the columns the
statement touches **anywhere** — projection, `WHERE`, `JOIN … ON`, `GROUP BY`,
`HAVING`, `ORDER BY`, `QUALIFY`, and window clauses alike. It is the complement
of `LineageSourceColumns` (which reports only columns that *flow into the
result*) and is a **superset** of it: a column appearing only in a filter
position is included here but not there.

```go
// SELECT a FROM t WHERE b > 1
ref, _     := easysql.ReferencedColumns("SELECT a FROM t WHERE b > 1", easysql.WithLineageDialect("trino"))
// map[string][]string{"t": {"a", "b"}}   -- b is a filter column
lineage, _ := easysql.LineageSourceColumns("SELECT a FROM t WHERE b > 1", easysql.WithLineageDialect("trino"))
// map[string][]string{"t": {"a"}}        -- b does not reach the result
```

The native engine does not expose the source table of a filter-position column,
so resolution is done structurally on the AST by a recursive scope resolver:
each query level maps its FROM/JOIN aliases to a source (a physical table, or —
for a CTE or derived subquery — its recursively resolved output→root mapping),
then attributes every referenced column to its root physical table.

| Behavior | Detail |
| -------- | ------ |
| Filter positions | `WHERE`, `JOIN … ON`, `USING`, `GROUP BY`, `HAVING`, `ORDER BY`, `QUALIFY`, window `PARTITION BY` all counted |
| CTEs / derived subqueries | Seen through to root physical tables |
| Correlated / `IN` / `EXISTS` / `ANY` / scalar subqueries | Resolved as their own scopes (outer refs via correlation) |
| Set operations | `UNION`/`INTERSECT`/`EXCEPT` branches merged |
| `FROM` constructs | `PIVOT`, `UNNEST`, `LATERAL VIEW`, self-joins resolved |
| DML | `DELETE … WHERE`, `UPDATE … SET … FROM … WHERE`, `MERGE … ON … WHEN` read columns are captured |
| Qualified `t.c` / `cat.sch.tbl.c` | Attributed to the resolved table |
| Unqualified `c` | Attributed to tables declaring it in `WithLineageMetadata`; with no metadata, to every physical source in scope (safe superset) |
| `SELECT *` / `t.*` | Expanded via metadata; otherwise the sentinel `"*"` |
| Tables with no referenced column | Still present with an empty list |

It accepts the same query-bearing statements as `LineageSourceColumns`
(`SELECT`/`UNION`, `CREATE VIEW`, `CREATE TABLE AS SELECT`, `INSERT … SELECT`, …)
plus the DML mutations above, and reuses `WithLineageDialect` /
`WithLineageMetadata`. Use it for access-control / masking decisions, where a
column read in a `WHERE` still counts as "touched".

**Fail-open by design.** The result is meant to over- rather than
under-approximate: a reference whose table cannot be resolved (an unknown alias,
a column absent from incomplete metadata, an unexpanded `SELECT *`) is
*broadcast* to every candidate physical table in scope instead of being dropped.
For an access-control caller, an extra column is harmless; a missing one is not.

## Errors

Classify failures with `errors.Is`:

| Error | Meaning |
| ----- | ------- |
| `ErrParse` | Input SQL could not be parsed |
| `ErrUnsupported` | Parsed, but statement shape is not supported |
| `ErrInternal` | Rewriter or analyzer produced invalid output |

Configuration mistakes (empty predicate, unknown dialect, bad regexp, missing
metadata for `LIKE`, …) return a plain `error`.

## Native engine

Polyglot runs as a bundled shared library loaded via PureGo (no cgo). The path
is fixed to the artifact in `.ffi/`; there is no environment override.

| GOOS / GOARCH | Library |
| ------------- | ------- |
| `darwin` / `arm64` | `.ffi/polyglot-sql-ffi-macos-aarch64/libpolyglot_sql_ffi.dylib` |
| `linux` / `amd64` | `.ffi/polyglot-sql-ffi-linux-x86_64/libpolyglot_sql_ffi.so` |
| `windows` / `amd64` | `.ffi/polyglot-sql-ffi-windows-x86_64/polyglot_sql_ffi.dll` |

The loaded library version must match the pinned Polyglot Go SDK in `go.mod`
(checked via `polyglot.Version()`). Set `EASYSQL_SKIP_FFI_VERSION_CHECK=1` to
bypass (unsupported). Unsupported platforms fail closed on first use.

## Design notes

`ApplyRowFilter` operates on Polyglot's JSON AST: parse once, clone a
wrapper-subquery template with the predicate baked in, replace each in-scope
physical table in place, generate SQL, then re-parse as a validity gate. FFI
call count is constant regardless of table count.

Notable properties:

- **Physical tables only** — table-valued functions and `DUAL` are never wrapped.
- **Quoting preserved** — quoted identifiers like `"order"` stay quoted.
- **Schema-qualified column rewrite** — `schema.table.col` becomes `table.col`
  after wrapping under a bare alias.
- **Scope-correct CTE handling** — CTE names are tracked per scope, not globally
  (`TestCTEScopeSecurity` guards a known security regression).

## Development

```bash
go test ./...
go test -bench=. -run='^$'    # benchmarks
```

Tests skip (rather than fail) when no `.ffi/` artifact exists for the host
platform. End-to-end `go get` smoke test:

```bash
scripts/verify-goget.sh
EASYSQL_VERIFY_VERSION=v0.1.0 scripts/verify-goget.sh
```

CI runs this on Linux, macOS, and Windows
([`.github/workflows/goget-verify.yml`](.github/workflows/goget-verify.yml)).
