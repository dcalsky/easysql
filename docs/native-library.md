# Native C ABI

easysql can be distributed without its Go source as a versioned C shared
library. The public contract consists of the checked-in
[`easysql.h`](../native/include/easysql.h) header and JSON request/response
schemas; Go types are not part of the ABI.

## Build and test

```bash
bash scripts/build-native.sh
bash scripts/test-native.sh
```

`build-native.sh` produces a platform package under `dist/native/` containing:

```text
include/easysql.h
lib/libeasysql.{so,dylib}       # easysql.dll on Windows
lib/libeasysql.dll.a             # Windows import library only
bindings/python/
bindings/javascript/
bindings/go/
licenses/
```

The platform-specific SQL engine is embedded in `libeasysql`; there is no
runtime sidecar to install or place on a library search path. On first use the
library authenticates the embedded bytes, writes them to a content-addressed
private cache, and loads that cache entry. The public package layout and API
only expose easysql names. Third-party notices remain under `licenses/`.

Supported release targets are currently:

| Target | Package |
| --- | --- |
| macOS ARM64 | `macos-aarch64` |
| Linux x86-64 | `linux-x86_64` |
| Windows x86-64 | `windows-x86_64` |

Set `EASYSQL_NATIVE_OUTPUT` to change the output directory and
`EASYSQL_NATIVE_VERSION` to set the version returned by `easysql_version`.
Release builds normally set the latter to the release tag.

## Language bindings

The Python binding uses only the standard library:

```python
import easysql

response = easysql.execute({
    "abiVersion": 1,
    "operation": "parseColumns",
    "args": {"sql": "SELECT id FROM orders"},
})
```

Add `bindings/python` to `PYTHONPATH`, or copy its `easysql` package into the
application. It locates the sibling `lib/` directory automatically;
`EASYSQL_LIBRARY_PATH` can select an explicit library.

The JavaScript binding supports Node.js 16 or newer and uses
[`koffi`](https://koffi.dev/):

```javascript
const easysql = require('./bindings/javascript');
const response = easysql.execute({
  abiVersion: 1,
  operation: 'parseColumns',
  args: {sql: 'SELECT id FROM orders'},
});
```

Run `npm install` in `bindings/javascript` before first use. The Go binding is
a small cgo module under `bindings/go`; applications can reference it with a
local `replace` directive and call `Execute` or `ExecuteJSON`. Its linker flags
locate the library shipped in the same extracted SDK. On Windows, keep
`easysql.dll` beside the application executable or add the SDK's `lib` directory
to `PATH`.

## ABI

The library exports four functions:

```c
uint32_t easysql_abi_version(void);
char *easysql_version(void);
char *easysql_execute(const void *request_json, size_t request_len);
void easysql_free_string(char *value);
```

Strings returned by `easysql_version` and `easysql_execute` are allocated by
the library and must be released exactly once with `easysql_free_string`.
Requests do not need a trailing NUL. All functions are safe for concurrent
callers; no Go pointer is retained across the C boundary.

Every request has this envelope:

```json
{
  "abiVersion": 1,
  "operation": "applyRowFilter",
  "args": {}
}
```

Every response is UTF-8 JSON. Success:

```json
{"abiVersion":1,"status":0,"data":"SELECT ..."}
```

Failure:

```json
{"abiVersion":1,"status":2,"error":"easysql: parse error: ..."}
```

| Status | Meaning |
| ---: | --- |
| `0` | Success |
| `1` | Invalid request, option, or argument |
| `2` | SQL parse error |
| `3` | Unsupported SQL statement or bounded-input rejection |
| `4` | Internal processing or runtime initialization error |
| `99` | Recovered panic at the native boundary |

The ABI version covers function signatures, operation names, request fields,
response shapes, and status meanings. Additive compatible fields do not require
a new ABI version; breaking changes do.

## Operations

### `applyRowFilter`

Arguments: `sql`, `whereClause`, and optional `dialect`, `tableNames`,
`tableRegexps`, `defaultDB`. The result data is a SQL string.

```json
{
  "abiVersion": 1,
  "operation": "applyRowFilter",
  "args": {
    "sql": "SELECT id FROM orders",
    "whereClause": "tenant_id = 7",
    "dialect": "postgres"
  }
}
```

### `bindCTEs`

Arguments: `consumerSQL`, `bindings` (`[{"name":"...","query":"..."}]`),
and optional `dialect`. The result data is a SQL string.

### Column analysis

`lineageSourceColumns`, `parseColumns`, `referencedColumns`, and
`referencedColumnUsages` accept `sql` plus optional `dialect`, `metadata`,
`producer`, and `namespace`. Their data shapes are respectively:

- `{"table":["column"]}`
- `["output_column"]`
- `{"table":["referenced_column"]}`
- `[{"table":"...","column":"...","clause":"SELECT"}]`

### `rewriteTableReferences`

Arguments: `sql`, `specs`, optional `dialect`, and optional
`stripMatchCatalogs`. `specs` follows the public `TableRewrite` structure using
lower-camel-case JSON fields. The result data is a SQL string.

## C example

```c
#include <string.h>
#include "easysql.h"

const char *request =
    "{\"abiVersion\":1,\"operation\":\"parseColumns\","
    "\"args\":{\"sql\":\"SELECT id FROM orders\"}}";
char *response = easysql_execute(request, strlen(request));
/* consume response */
easysql_free_string(response);
```

The native package is a binary distribution boundary, not an anti-reversing
guarantee. Builds use `-trimpath` and ask the Go linker to omit its DWARF and
symbol table, but machine code and required native symbols remain inspectable.
