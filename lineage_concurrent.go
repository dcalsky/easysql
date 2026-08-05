// This file preserves the former concurrent lineage API as a compatibility
// wrapper. Polyglot analyzes complete set operations in one native call, so a
// separate branch-level scheduler is no longer useful.

package easysql

// LineageSourceColumnsConcurrent retains source compatibility with earlier
// releases and has exactly the same behavior as LineageSourceColumns.
//
// Deprecated: use LineageSourceColumns. Complete set operations are now
// analyzed by one native OpenLineage call, so this wrapper provides no
// concurrency benefit.
func LineageSourceColumnsConcurrent(sql string, opts ...LineageOption) (map[string][]string, error) {
	return LineageSourceColumns(sql, opts...)
}
