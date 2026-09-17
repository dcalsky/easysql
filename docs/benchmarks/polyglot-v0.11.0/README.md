# Polyglot v0.11.0 重构与 Benchmark 报告

日期：2026-09-17。基线提交：`66dc082039d6ed48a252f30fd1cfeee8ad892217`。

本次将 Go SDK 和三个平台的内嵌 FFI 从 v0.9.0 同步升级至 v0.11.0。生产 Go 文件净减少 **392 行（含注释）**，主要来自输出列分析的自研修复逻辑。新增测试与 Benchmark 单独计数，不包含在这个净减少数中。

## 实际重构与兼容边界

| 范围 | 修改 | 保留的责任 |
| --- | --- | --- |
| `ParseColumns` | 使用 `OutputColumns`；仅未解析的星号才调用 `OutputColumnsWithSchema`，删除 `AnalyzeQuery` 后的列名猜测、星号匹配和 metadata 重排；普通查询复用原 SQL，省去 Generate 往返 | 单语句校验、DDL/DML 包装、匿名列 `_colN` 格式、已知空表兼容 |
| 输入保护 | 删除 Go 原始文本括号扫描，使用 v0.11 原生解析器递归保护 | 1 MiB 字节限制；原生复杂度错误映射为 `ErrUnsupported` |
| `ReferencedColumns` / `ReferencedColumnUsages` | 核对 v0.10 新增的 `columnUses`，保留当前解析器并更新说明 | 内层投影引用、DML、未知列的保守归属以及现有 clause 分类 |
| 行级过滤、表替换、CTE 绑定、血缘 | 继续使用已接入的原生 builder、Parse/Generate、OpenLineage；统一入口的错误分类随升级更新 | 项目自己的表作用域、访问控制改写和血缘回退语义 |

`OutputColumns` 并非 v0.11 首次引入；本次改用这个专用接口，同时获得 v0.9.1 后的按名集合运算修复。新版本能力依据：[版本 Changelog](https://github.com/tobilg/polyglot/blob/v0.11.0/CHANGELOG.md)、[Go 官方集成测试](https://github.com/tobilg/polyglot/blob/v0.11.0/packages/go/integration_test.go)。

不能直接用 `columnUses + 最终 projections` 替代现有引用 API。例如 `WITH c AS (SELECT id, amount, unused FROM orders) SELECT id FROM c WHERE amount > 0` 的原生 columnUses 只含 `amount`，最终投影只含 `id`；本项目还要求报告内层已引用的 `unused`。对应的直接调用证据已保存于 [native-contract.txt](native-contract.txt)，并有独立集成测试固定该边界。

空 schema 在原生接口表示“未知/open”，本项目空 metadata 表示“已知零列”。兼容分支复用现有作用域解析器，仅在涉及真实空表且仍有未展开星号时启用。schema 展开产生的 `_col_N` 名称仍按既有 `_colN` 约定转换，保留了这一小段兼容逻辑。

原来的 64 层原始括号阈值被原生限制替换：默认逻辑解析深度 1024、分组嵌套 512。这些限制单位不同；字符串和注释中的括号不再误触发限制，未带括号的深层 unary/IF 链也受保护。

## 正确性验证

- macOS ARM64 实际执行根模块测试：669 个测试/子测试通过，1 个可选 `TestLineageDumpForDiff` 因未提供外部 `LINEAGE_DUMP` 数据集跳过。
- `go test -race ./...`、`go vet ./...` 通过。
- 实际构建 native library 并通过 C ABI、Python、JavaScript、独立 Go binding 消费者测试；Go binding 额外使用 `-count=1`，避免动态库测试被 Go 缓存。
- 全新临时 Go module 使用本地 replace 的 go-get 消费者验证通过。
- Linux/amd64、Windows/amd64 根测试二进制交叉编译通过，**未在对应操作系统运行**。三个发布归档均已核对 [发布 SHA-256](release-checksums.sha256)，各动态库摘要已更新。
- 新测试覆盖原生输出槽、metadata 顺序、空 schema、按名 UNION、空表作用域、解析深度和截断 SQL。
- 临时恢复旧 `ParseColumns` 实现后，新测试能重现三处失败：CTE 投影顺序被改乱、显式重复计算别名被改写、无星号查询的 `_col_0` 显式别名被改名；恢复重构实现后全部通过。

验证记录：[verification.txt](verification.txt)。

## Benchmark 方法

环境：Apple M4，macOS 15.7.9，darwin/arm64，Go 1.27.0，默认 GOMAXPROCS=10。没有启用 race。当前 Sonic v1.15.2 在 Go 1.27 上回退至 `encoding/json`，三组使用同一运行环境；不要将绝对值外推到启用 Sonic 快路径或其他平台的服务。

比较三组单独编译的测试二进制：

1. **before**：原始 v0.9.0 代码，仅增加相同 Benchmark fixture。
2. **upgrade-only**：只升级 SDK、三平台 FFI/头文件和 digest，其余生产代码保持基线。
3. **after**：v0.11.0 和最终重构实现。

每组含 16 个分析 API 场景及 10 个现有 rewrite 场景，共 26 项。每项运行 6 次，每次 `-benchtime=200ms`；三组进程串行、轮换先后次序，共 468 个测量样本。初始化和预热不计入计时；分析 API 每次重新分析，不缓存结果。Rewrite 复用预编译的 rewriter，不包含谓词编译成本。因此这是稳态调用测试，不是进程冷启动测试，也不覆盖并发吞吐或全部方言。

耗时包含 Go→FFI→Rust 的整条调用链；`B/op` / `allocs/op` **只统计 Go 堆，不含 Rust/native 分配**。表格使用 6 次中位数，负变化表示减少耗时。完整统计和显著性见 [benchstat.txt](benchstat.txt)，部分案例存在较大系统噪声，应结合置信区间与 p 值阅读。

## 耗时结果

| 场景 | before μs/op | 仅升级 μs/op | 重构后 μs/op | 重构后 vs before | 重构后 vs 仅升级 |
| --- | ---: | ---: | ---: | ---: | ---: |
| Analysis/ParseColumns/simple | 117.38 | 166.47 | 38.91 | -66.9% | -76.6% |
| Analysis/ParseColumns/star | 209.52 | 297.10 | 125.48 | -40.1% | -57.8% |
| Analysis/ParseColumns/cte | 321.90 | 585.91 | 123.54 | -61.6% | -78.9% |
| Analysis/ParseColumns/union | 249.59 | 336.39 | 65.16 | -73.9% | -80.6% |
| Analysis/ReferencedColumns/simple | 71.44 | 89.37 | 89.36 | +25.1% | -0.0% |
| Analysis/ReferencedColumns/star | 92.90 | 133.85 | 133.89 | +44.1% | +0.0% |
| Analysis/ReferencedColumns/cte | 210.85 | 308.97 | 307.57 | +45.9% | -0.5% |
| Analysis/ReferencedColumns/union | 118.45 | 155.00 | 153.97 | +30.0% | -0.7% |
| Analysis/ReferencedColumnUsages/simple | 68.92 | 95.73 | 91.08 | +32.2% | -4.9% |
| Analysis/ReferencedColumnUsages/star | 94.65 | 141.54 | 135.11 | +42.7% | -4.5% |
| Analysis/ReferencedColumnUsages/cte | 215.58 | 312.46 | 309.38 | +43.5% | -1.0% |
| Analysis/ReferencedColumnUsages/union | 120.61 | 155.79 | 154.63 | +28.2% | -0.7% |
| Analysis/LineageSourceColumns/simple | 165.27 | 217.09 | 211.73 | +28.1% | -2.5% |
| Analysis/LineageSourceColumns/star | 193.59 | 285.46 | 265.58 | +37.2% | -7.0% |
| Analysis/LineageSourceColumns/cte | 458.74 | 764.49 | 753.18 | +64.2% | -1.5% |
| Analysis/LineageSourceColumns/union | 322.60 | 433.40 | 431.35 | +33.7% | -0.5% |
| Rewrite/simple | 65.00 | 71.90 | 71.93 | +10.7% | +0.0% |
| Rewrite/two_join | 144.54 | 194.79 | 193.83 | +34.1% | -0.5% |
| Rewrite/three_join | 216.55 | 289.59 | 279.85 | +29.2% | -3.4% |
| Rewrite/left_join | 134.00 | 168.19 | 166.25 | +24.1% | -1.2% |
| Rewrite/subquery_from | 163.08 | 193.01 | 199.10 | +22.1% | +3.2% |
| Rewrite/cte | 169.03 | 202.22 | 208.56 | +23.4% | +3.1% |
| Rewrite/union | 129.55 | 140.64 | 148.19 | +14.4% | +5.4% |
| Rewrite/scalar_subquery | 170.82 | 213.19 | 213.65 | +25.1% | +0.2% |
| Rewrite/schema_qualified | 150.60 | 202.38 | 207.49 | +37.8% | +2.5% |
| Rewrite/analytical | 323.37 | 438.60 | 452.28 | +39.9% | +3.1% |

## ParseColumns 的 Go 分配

| 场景 | before B/op | 重构后 B/op | before allocs/op | 重构后 allocs/op |
| --- | ---: | ---: | ---: | ---: |
| simple | 34439 | 28034 | 317.0 | 346.0 |
| star | 52617 | 50977 | 486.0 | 608.0 |
| cte | 90734 | 98057 | 823.5 | 1123.0 |
| union | 62200 | 51372 | 502.0 | 566.0 |

## 结果解释

- `ParseColumns` 四组耗时比原版降低 **40.1%–73.9%**。专门的输出接口避免了完整查询分析和 Go 重排，且修复了可复现的列顺序/别名问题。
- `ReferencedColumns` / `ReferencedColumnUsages` 比基线上升约 **25.1%–45.9%**，血缘分析上升约 **28.1%–64.2%**；同样的退化已出现在仅升级阶段。现有兼容行为继续保留；尚未做 CPU profile，无法进一步确定原生内部的耗时来源。
- Rewrite 的完整结果见上表和 benchstat。中位数变化不等于统计显著；本次原生版本升级并未让所有 API 都加速。
- 耗时降低不意味着 Go 分配都减少：ParseColumns 的 CTE 场景 B/op 和若干场景 allocs/op 仍高于 v0.9.0。原生堆未计入这些分配数。
- 26 项耗时的等权几何均值：before **160.30 μs**，仅升级 **216.64 μs**，重构后 **174.44 μs**。重构后相对基线变化 **+8.8%**、相对仅升级 **-19.5%**；这不是实际业务请求占比加权的总体吞吐。

## 复现

Benchmark 源码在仓库根目录 `api_bench_test.go` 和 `easysql_bench_test.go`。建立基线提交的独立 checkout，复制相同的 `api_bench_test.go`，编译 before。然后仅同步当前的 `go.mod` / `go.sum`、`.ffi/` 和三个 `runtime_asset_*` 平台文件，编译 upgrade-only。当前工作树编译 after：

```sh
# 分别在上述三种源码状态下编译，使用不同输出文件名。
go test -c -o /tmp/easysql-before.test .
go test -c -o /tmp/easysql-upgrade-only.test .
go test -c -o /tmp/easysql-after.test .

# 在当前 checkout 根目录运行；结果写入独立目录以保留本报告原始数据。
python3 docs/benchmarks/polyglot-v0.11.0/run_benchmarks.py \
  --before /tmp/easysql-before.test \
  --upgrade-only /tmp/easysql-upgrade-only.test \
  --after /tmp/easysql-after.test \
  --output-dir /tmp/easysql-benchmark-repeat

# 本报告固定使用的 benchstat 版本。
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da \
  /tmp/easysql-benchmark-repeat/before.txt \
  /tmp/easysql-benchmark-repeat/upgrade-only.txt \
  /tmp/easysql-benchmark-repeat/after.txt
```

原始数据：[before.txt](before.txt)、[upgrade-only.txt](upgrade-only.txt)、[after.txt](after.txt)。旧的外部 differential lineage Benchmark 未运行；新增的本地 fixture 已覆盖 `LineageSourceColumns` 的四个基本场景。
