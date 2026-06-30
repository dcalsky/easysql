// This file adds a concurrent driver for column-level SQL lineage. It reuses
// the exact setup and per-leaf analysis of the serial LineageSourceColumns
// (lineage.go) and only changes how the leaf SELECTs are iterated: when a
// statement splits into more than one leaf (a UNION / INTERSECT / EXCEPT), the
// leaves are analyzed in parallel instead of one after another.

package easysql

import "sync"

// LineageSourceColumnsConcurrent is a drop-in alternative to LineageSourceColumns
// that analyzes the leaf SELECTs of a set operation (UNION / INTERSECT /
// EXCEPT) concurrently. Options, semantics and result are identical to the
// serial version; only the leaf iteration differs.
//
// Concurrency is engaged only when the statement actually splits into more than
// one leaf. The overwhelmingly common single-SELECT statement takes the very
// same serial path as LineageSourceColumns (no goroutines, and the already
// rendered inner SQL is reused), so there is no goroutine overhead to pay on
// the hot path.
//
// Each leaf is analyzed into its own private result map; the maps are folded
// into the shared result only after every goroutine has joined, so the shared
// map is never written concurrently and no locking is needed during analysis.
// This is safe because the polyglot client is built for concurrent use: each
// call takes a read lock and the native library is reached through stateless
// function pointers.
//
// At most maxLineageConcurrency leaves are analyzed at once. Benchmarks show the
// per-leaf speedup peaks at a handful of in-flight analyses and then flattens
// (the serial whole-statement setup begins to dominate), so a small cap keeps
// the win while bounding goroutine, memory and native-call pressure on wide set
// operations.
const maxLineageConcurrency = 4

func LineageSourceColumnsConcurrent(sql string, opts ...LineageOption) (map[string][]string, error) {
	client, err := defaultClient()
	if err != nil {
		return nil, err
	}
	req, err := prepareLineage(client, sql, opts...)
	if err != nil {
		return nil, err
	}

	// Common path: zero or one leaf. Reuse the inner SQL already rendered by
	// prepareLineage and stay fully serial — identical to LineageSourceColumns.
	if len(req.leaves) <= 1 {
		for range req.leaves {
			if err := aggregateColumns(client, req.innerSQL, req.cfg, req.schema, req.tableCols); err != nil {
				return nil, err
			}
		}
		return sortedResult(req.tableCols), nil
	}

	// Set operation with multiple leaves: analyze each leaf concurrently into
	// its own private map so the goroutines never touch shared state. A buffered
	// channel caps the number of in-flight analyses at maxLineageConcurrency.
	locals := make([]map[string]map[string]struct{}, len(req.leaves))
	errs := make([]error, len(req.leaves))
	sem := make(chan struct{}, maxLineageConcurrency)
	var wg sync.WaitGroup
	for i, leaf := range req.leaves {
		wg.Add(1)
		sem <- struct{}{} // blocks once maxLineageConcurrency analyses are running
		go func(i int, leaf map[string]any) {
			defer wg.Done()
			defer func() { <-sem }()
			leafSQL, gerr := generateStatement(client, leaf, req.cfg.dialect)
			if gerr != nil {
				errs[i] = gerr
				return
			}
			local := map[string]map[string]struct{}{}
			if aerr := aggregateColumns(client, leafSQL, req.cfg, req.schema, local); aerr != nil {
				errs[i] = aerr
				return
			}
			locals[i] = local
		}(i, leaf)
	}
	wg.Wait()

	// Surface the first error in leaf order, matching the serial version's
	// fail-fast ordering.
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}

	// Fold the per-leaf maps into the seeded result. Done serially after the
	// goroutines join, so the shared map is written by a single goroutine.
	for _, local := range locals {
		for table, cols := range local {
			dst := ensureSet(req.tableCols, table)
			for c := range cols {
				dst[c] = struct{}{}
			}
		}
	}
	return sortedResult(req.tableCols), nil
}
