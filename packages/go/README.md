# easysql Go SDK

The official Go SDK for the prebuilt easysql native library. The module is a
thin PureGo wrapper: it does not import the easysql implementation module, does
not compile the SQL engine from source, and does not require cgo.

## Install

Use the same version for the Go SDK and native release:

```bash
go get github.com/dcalsky/easysql/packages/go@v0.10.5
```

Download the matching archive from the
[easysql releases](https://github.com/dcalsky/easysql/releases), then either
pass the library path to `Open` or set `EASYSQL_LIBRARY_PATH` and call
`OpenDefault`.

| Platform | Native library |
| --- | --- |
| Linux x86-64 | `lib/libeasysql.so` |
| macOS ARM64 | `lib/libeasysql.dylib` |
| Windows x86-64 | `lib/easysql.dll` |

## Example

```go
package main

import (
	"fmt"
	"log"

	easysql "github.com/dcalsky/easysql/packages/go"
)

func main() {
	client, err := easysql.OpenDefault()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	query, err := client.ApplyRowFilter(
		"SELECT id FROM orders",
		"tenant_id = 7",
		easysql.ApplyRowFilterOptions{Dialect: "postgres"},
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(query)
}
```

For example, after extracting the Linux archive:

```bash
export EASYSQL_LIBRARY_PATH="$PWD/easysql-native-linux-x86_64-v0.10.5/lib/libeasysql.so"
go run .
```

`OpenDefault` also checks for the platform library in the current directory,
its `lib/` subdirectory, the executable directory, and the executable's `lib/`
subdirectory. `Open(path)` is recommended when the application controls its
installation layout.

## API

`Client` exposes typed methods for every native operation:

- `ApplyRowFilter`
- `BindCTEs`
- `LineageSourceColumns`
- `ParseColumns`
- `ReferencedColumns`
- `ReferencedColumnUsages`
- `RewriteTableReferences`

`Execute` exposes the versioned JSON ABI for forward-compatible or low-level
use. `RuntimeVersion` returns the loaded native library version, while
`Version` returns the Go module version selected by the consuming build.

The native archive is a separate runtime dependency. The SDK intentionally
does not download executable code at application startup.
