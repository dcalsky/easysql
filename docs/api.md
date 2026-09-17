# API reference

Public APIs are stateless and safe for concurrent callers. Supported dialects
include PostgreSQL, Trino/Presto, StarRocks, and MySQL.

## Runtime initialization

`Init` eagerly authenticates and loads the native SQL engine embedded in the Go
module. Calling it is optional because SQL APIs initialize that runtime lazily.

`InitWithRuntimePath` is for controlled deployments that select a trusted engine
at an explicit location. It applies the same regular-file, SHA-256, and version
checks. Initialization is process-wide and first-call-wins, so an explicit path
must be selected before any SQL API call.

## ApplyRowFilter

```go
out, err := easysql.ApplyRowFilter(
	sql,
	whereClause,
	easysql.WithDialect("trino"),
	easysql.WithTableNames("orders", "sales.orders"),
)
```

`ApplyRowFilter` accepts a `SELECT` or set operation and wraps every selected
physical source table in a derived query with `whereClause`. Other statement
types return `ErrUnsupported`.

Output is normalized by the SQL generator. Comments are dropped; identifiers
and literals are retained. If no table is in scope, the input is returned
unchanged. The predicate is parsed as a dialect-aware builder expression and
inserted into the AST, so callers must bind and escape values before calling it.

| Option | Meaning |
| --- | --- |
| `WithDialect(d)` | SQL dialect; default `mysql`. |
| `WithTableNames(...)` | Limit rewriting to bare, schema-qualified, or catalog-qualified table names. |
| `WithTableRegexp(...)` | Limit rewriting to names matching a Go regular expression. |
| `WithDefaultDB(db)` | Schema used to resolve unqualified tables. |
| `WithSelfCheck(bool)` | Deprecated no-op. Output is always parsed again. |

## BindCTEs

```go
out, err := easysql.BindCTEs(
	`SELECT * FROM orders WHERE total_amount > 1000`,
	[]easysql.CTEBinding{{
		Name:  "orders",
		Query: `SELECT customer_id, SUM(amount) AS total_amount FROM raw_orders GROUP BY customer_id`,
	}},
	easysql.WithBindCTEDialect("trino"),
)
```

Both the consumer and every binding must be a single `SELECT` or set operation.
Bindings are placed before CTEs that already exist in the consumer, in slice
order. A binding can use an earlier binding; duplicate names are rejected.
Output is normalized and parsed again before it is returned.

## LineageSourceColumns

```go
cols, err := easysql.LineageSourceColumns(
	`SELECT o.order_id, p.paid_amount
	 FROM hive.raw.orders o
	 JOIN hive.raw.payments p ON o.order_id = p.order_id
	 WHERE o.status = 'PAID'`,
	easysql.WithLineageDialect("trino"),
	easysql.WithLineageMetadata(map[string][]string{
		"hive.raw.orders":   {"order_id", "status"},
		"hive.raw.payments": {"order_id", "paid_amount"},
	}),
)
// map[string][]string{
//   "hive.raw.orders":   {"order_id"},
//   "hive.raw.payments": {"paid_amount"},
// }
```

Returns sorted source columns whose values reach the result, keyed by root
physical table. A physical source table is present even when it has no flowing
column.

- Columns used only by `WHERE`, `JOIN ... ON`, `GROUP BY`, `HAVING`, or top-level
  `ORDER BY` are excluded.
- `UNION` and `UNION ALL` retain value columns from both branches.
- `EXCEPT` and `INTERSECT` retain values from their left branch; right-side
  tables remain in the result with empty lists unless they contribute elsewhere.
- CTEs and derived subqueries are resolved to physical sources.
- Statements containing a query are supported, including bare `SELECT`/`UNION`,
  `CREATE VIEW`, `CREATE TABLE AS SELECT`, and `INSERT ... SELECT`.
- `UPDATE` assignment values and `MERGE` update/insert values are resolved
  structurally, including values sourced through CTEs and derived tables;
  `WHERE`, `ON`, and `WHEN` conditions remain filter-only.
- Metadata expands `*` and resolves ambiguous unqualified columns. Metadata for
  tables outside the query is ignored.

| Option | Meaning |
| --- | --- |
| `WithLineageDialect(d)` | Parse and analyze dialect; default `trino`. |
| `WithLineageMetadata(m)` | `table -> columns` metadata for wildcard expansion and ambiguity resolution. |
| `WithLineageProducer(uri)` | OpenLineage producer metadata. |
| `WithLineageNamespace(ns)` | OpenLineage dataset namespace. |

`LineageSourceColumnsConcurrent` is deprecated. It delegates to
`LineageSourceColumns`.

## ParseColumns

```go
cols, err := easysql.ParseColumns(
	`CREATE VIEW analytics.v AS SELECT u.name, o.amount AS amt FROM users u JOIN orders o ON u.id = o.user_id`,
	easysql.WithLineageDialect("trino"),
)
// []string{"name", "amt"}
```

Returns output column names in projection order. It supports `SELECT`, `WITH`,
set operations, `CREATE VIEW`, `CREATE TABLE AS SELECT`, `INSERT ... SELECT`,
`CREATE TABLE (...)`, `CREATE TABLE ... (LIKE ...)`, and `INSERT ... VALUES`.
An explicit column list on `CREATE VIEW`, `CREATE TABLE`, or `INSERT` overrides
inferred names.

| Input | Result |
| --- | --- |
| Unaliased expression | `_col{index}` |
| `SELECT *` / `t.*` | Expanded from `WithLineageMetadata`; missing metadata yields `"*"`. |
| Multi-table `SELECT *` | Every source table must have metadata, otherwise result is `"*"`. |
| `CREATE TABLE ... (LIKE t)` | Requires metadata for `t`. |
| `INSERT INTO t VALUES (...)` without target columns | `ErrUnsupported`. |

`ParseColumns` uses the native output-column inspection APIs. Explicit
projection order (including CTE/derived-table order) and duplicate aliases are
preserved; name-aligned set operations follow the selected dialect. Metadata
is used for unresolved wildcard expansion. Empty table metadata keeps the
existing easysql meaning of zero columns, even though the native schema API
uses an empty column list to mean unknown/open.

## ReferencedColumns

```go
ref, err := easysql.ReferencedColumns(
	`SELECT a FROM t WHERE b > 1`,
	easysql.WithLineageDialect("trino"),
)
// map[string][]string{"t": {"a", "b"}}
```

Returns sorted columns read anywhere in a statement, keyed by root physical
table. It is a superset of `LineageSourceColumns`: filter and join inputs are
included.

- Supports query-bearing statements and `DELETE`, `UPDATE`, and `MERGE`.
- Resolves CTEs, derived queries, correlated subqueries, set operations,
  `PIVOT`, `UNNEST`, lateral views, and self-joins.
- Tracks `SELECT`, `WHERE`, `JOIN ON`, `USING`, grouping, ordering, window, and
  DML read positions.
- With metadata, an unqualified column is assigned to matching source tables.
  Without it, unresolved references are assigned to every candidate source.
- An unexpanded wildcard is reported as `"*"`.

The resolver is deliberately fail-open: unresolved references are attributed to
candidate physical sources instead of being dropped.

## ReferencedColumnUsages

```go
uses, err := easysql.ReferencedColumnUsages(
	`SELECT u.name FROM users u JOIN orders o ON u.id = o.user_id WHERE o.status = 'PAID'`,
	easysql.WithLineageDialect("trino"),
)
```

Returns sorted `[]ColumnUse`, with a physical table, column, and `ColumnClause`
for each distinct use. Clause values include `SELECT`, `FROM`, `JOIN_ON`,
`JOIN_USING`, `WHERE`, `GROUP_BY`, `HAVING`, `QUALIFY`, `WINDOW`, `ORDER_BY`,
`SORT_BY`, `DISTRIBUTE_BY`, `CLUSTER_BY`, `CONNECT_BY`, `LATERAL_VIEW`,
`UPDATE_SET_TARGET`, `UPDATE_SET_VALUE`, `MERGE_ON`, and `MERGE_WHEN`.

Projection aliases and positive ordinals in output-aware clauses resolve to
their projected source columns. Unlike `ReferencedColumns`, this API has no
empty-table placeholder because every result entry describes a real use.

## Errors

Use `errors.Is` for package errors:

| Error | Meaning |
| --- | --- |
| `ErrParse` | SQL could not be parsed. |
| `ErrUnsupported` | Parsed SQL is not supported by that API. |
| `ErrInternal` | The rewriter or analyzer produced invalid output. |

Invalid options return ordinary errors.

## Implementation notes

- `ApplyRowFilter` rewrites the parsed AST. It only rewrites physical tables;
  table-valued functions and `DUAL` are left unchanged.
- Rewriting preserves quoted identifiers and maps schema-qualified column
  references to the generated table aliases.
- CTE visibility is scoped to each query level.
