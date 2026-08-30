# easysql

SQL analysis and rewriting for Go.

## Install

```bash
go get github.com/dcalsky/easysql@latest
```

## Use

```go
package main

import (
    "log"

    "github.com/dcalsky/easysql"
)

func main() {
    sql, err := easysql.ApplyRowFilter(
        `SELECT "uid" FROM accounts`,
        `user = 'alice'`,
        easysql.WithDialect("postgres"),
    )
    if err != nil {
        log.Fatal(err)
    }
    _ = sql
}
```

Call `easysql.Init()` at startup when the application should verify the bundled
runtime before serving requests.

## Examples

### BindCTEs

```go
out, err := easysql.BindCTEs(
    `SELECT id FROM recent_orders`,
    []easysql.CTEBinding{{
        Name:  "recent_orders",
        Query: `SELECT id FROM orders WHERE created_at >= DATE '2026-01-01'`,
    }},
    easysql.WithBindCTEDialect("trino"),
)
// WITH recent_orders AS (
//   SELECT id FROM orders WHERE created_at >= DATE '2026-01-01'
// )
// SELECT id FROM recent_orders
```

### ApplyRowFilter

```go
out, err := easysql.ApplyRowFilter(
    `SELECT "uid" FROM accounts`,
    `user = 'alice'`,
    easysql.WithDialect("postgres"),
)
// SELECT "uid" FROM (SELECT * FROM accounts WHERE "user" = 'alice') AS accounts
```

### LineageSourceColumns

```go
cols, err := easysql.LineageSourceColumns(
    `SELECT o.id, p.amount
     FROM orders o JOIN payments p ON o.id = p.order_id
     WHERE o.status = 'PAID'`,
    easysql.WithLineageMetadata(map[string][]string{
        "orders":   {"id", "status"},
        "payments": {"order_id", "amount"},
    }),
)
// map[string][]string{
//   "orders":   {"id"},
//   "payments": {"amount"},
// }
```

### ParseColumns

```go
cols, err := easysql.ParseColumns(`SELECT id, amount AS total FROM orders`)
// []string{"id", "total"}
```

### ReferencedColumns

```go
cols, err := easysql.ReferencedColumns(
    `SELECT id FROM orders WHERE status = 'PAID'`,
)
// map[string][]string{"orders": {"id", "status"}}
```

### ReferencedColumnUsages

```go
uses, err := easysql.ReferencedColumnUsages(
    `SELECT id FROM orders WHERE status = 'PAID'`,
)
// []easysql.ColumnUse{
//   {Table: "orders", Column: "id", Clause: easysql.ColumnClauseSelect},
//   {Table: "orders", Column: "status", Clause: easysql.ColumnClauseWhere},
// }
```

## Documentation

- [API reference](docs/api.md)
- [Native runtime](docs/native-runtime.md)
- [Native C ABI](docs/native-library.md)
- [Development](docs/development.md)
