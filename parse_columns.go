// This file implements ParseColumns: given a SQL statement it returns the
// field names the statement exposes, in projection order.
//
// Query-bearing statements (SELECT, WITH, UNION, CREATE VIEW, CREATE TABLE AS
// SELECT, INSERT ... SELECT, ...) are unwrapped to their inner query and
// analyzed with Polyglot's AnalyzeQuery. CREATE VIEW / CREATE TABLE statements
// with an explicit column list take precedence over names inferred from the
// SELECT list. CREATE TABLE (col type, ...) and CREATE TABLE ... (LIKE ...)
// yield declared or metadata-resolved column names.

package easysql

import (
	"errors"
	"fmt"
	"strings"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// ParseColumns returns the names of fields exposed by sql, in left-to-right
// projection order. It accepts query-bearing statements (SELECT, WITH, UNION,
// CREATE VIEW, CREATE TABLE AS SELECT, INSERT ... SELECT, ...) as well as
// CREATE TABLE/VIEW with an explicit column list. For DDL wrappers the inner
// query is analyzed; an explicit column list on CREATE VIEW, CREATE TABLE or
// INSERT overrides names inferred from the SELECT list.
//
// Wildcards (SELECT *, t.*) expand when WithLineageMetadata supplies table
// catalogs; without metadata an unexpanded star appears as "*". For multi-table
// SELECT * every referenced base table must appear in metadata or the result
// falls back to ["*"].
//
// CREATE TABLE ... (LIKE other) requires WithLineageMetadata for the source
// table. INSERT INTO t VALUES (...) without a target column list cannot be
// resolved statically and returns ErrUnsupported.
//
// The native SQL engine is loaded automatically on first use (see Init).
func ParseColumns(sql string, opts ...LineageOption) ([]string, error) {
	client, err := defaultClient()
	if err != nil {
		return nil, err
	}
	return parseColumns(client, sql, opts...)
}

func parseColumns(client *polyglot.Client, sql string, opts ...LineageOption) ([]string, error) {
	if client == nil {
		return nil, errors.New("easysql: nil polyglot client")
	}

	cfg := lineageOptions{dialect: "trino", producer: lineageProducer, namespace: lineageNamespace}
	for _, o := range opts {
		o(&cfg)
	}
	if strings.TrimSpace(cfg.dialect) == "" {
		cfg.dialect = "trino"
	}

	stmt, err := parseFirstStatement(client, sql, cfg.dialect)
	if err != nil {
		return nil, err
	}

	if ins, ok := stmt["insert"].(map[string]any); ok {
		if cols := identListFromArray(ins["columns"]); len(cols) > 0 {
			return cols, nil
		}
		if hasInsertValues(ins) {
			return nil, fmt.Errorf("%w: INSERT without target column list", ErrUnsupported)
		}
	}

	if cols := ddlExplicitColumns(stmt); len(cols) > 0 {
		return cols, nil
	}

	if cols, err := createTableDDLColumns(stmt, cfg.metadata); err != nil {
		return nil, err
	} else if cols != nil {
		return cols, nil
	}

	inner := innerQuery(stmt)
	if inner == nil {
		return nil, nil
	}

	innerSQL, err := generateStatement(client, inner, cfg.dialect)
	if err != nil {
		return nil, err
	}

	analysis, err := client.AnalyzeQuery(innerSQL, polyglot.AnalyzeQueryOptions{
		Dialect: cfg.dialect,
		Schema:  metadataToSchema(cfg.metadata),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: analyze failed: %v", ErrInternal, err)
	}

	return columnsFromAnalysis(analysis, cfg.metadata), nil
}

func hasInsertValues(ins map[string]any) bool {
	if vals, ok := ins["values"].([]any); ok && len(vals) > 0 {
		return true
	}
	if def, ok := ins["default_values"].(bool); ok && def {
		return true
	}
	return false
}

func identListFromArray(raw any) []string {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		if n := identName(item); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// ddlExplicitColumns returns column names from a CREATE VIEW / CREATE TABLE
// payload when the statement declares an explicit column list. nil means none.
func ddlExplicitColumns(stmt map[string]any) []string {
	for _, key := range []string{"create_view", "create_materialized_view"} {
		if payload, ok := stmt[key].(map[string]any); ok {
			if cols := columnDefsFromPayload(payload); len(cols) > 0 {
				return cols
			}
		}
	}
	if ct, ok := stmt["create_table"].(map[string]any); ok {
		if ct["as_select"] != nil {
			if cols := columnDefsFromPayload(ct); len(cols) > 0 {
				return cols
			}
		}
	}
	return nil
}

func columnDefsFromPayload(payload map[string]any) []string {
	raw, ok := payload["columns"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		col, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n := identName(col["name"]); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// createTableDDLColumns handles CREATE TABLE definitions that mix column specs
// and LIKE clauses (no AS SELECT). nil, nil means not applicable.
func createTableDDLColumns(stmt map[string]any, metadata map[string][]string) ([]string, error) {
	payload, ok := stmt["create_table"].(map[string]any)
	if !ok {
		return nil, nil
	}
	if payload["as_select"] != nil {
		return nil, nil
	}

	rawCols, _ := payload["columns"].([]any)
	rawConstraints, _ := payload["constraints"].([]any)
	if len(rawCols) == 0 && len(rawConstraints) == 0 {
		return nil, nil
	}

	var out []string
	inlineLike := false
	for _, item := range rawCols {
		col, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if like, ok := col["Like"].(map[string]any); ok {
			expanded, err := expandLikeSource(like["source"], metadata)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
			inlineLike = true
			continue
		}
		if n := identName(col["name"]); n != "" {
			out = append(out, n)
		}
	}

	var likeCols []string
	for _, item := range rawConstraints {
		constraint, ok := item.(map[string]any)
		if !ok {
			continue
		}
		like, ok := constraint["Like"].(map[string]any)
		if !ok {
			continue
		}
		expanded, err := expandLikeSource(like["source"], metadata)
		if err != nil {
			return nil, err
		}
		likeCols = append(likeCols, expanded...)
	}

	if len(likeCols) > 0 && !inlineLike {
		if len(out) >= 2 && len(rawConstraints) == 1 && len(rawCols) == len(out) {
			// Parser stores (col1, LIKE src, col2) as columns=[col1,col2] plus a
			// separate Like constraint; interleave LIKE columns after the first.
			out = append(append([]string{out[0]}, likeCols...), out[1:]...)
		} else {
			out = append(out, likeCols...)
		}
	}

	if len(out) == 0 {
		if len(likeCols) > 0 {
			return likeCols, nil
		}
		return nil, nil
	}
	return out, nil
}

func expandLikeSource(source any, metadata map[string][]string) ([]string, error) {
	ref := tableRefName(source)
	cols, ok := lookupMetadataColumns(metadata, ref)
	if !ok {
		return nil, fmt.Errorf("%w: CREATE TABLE LIKE %s requires metadata", ErrUnsupported, ref)
	}
	return cols, nil
}

func tableRefName(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	if c := identName(m["catalog"]); c != "" {
		parts = append(parts, c)
	}
	if s := identName(m["schema"]); s != "" {
		parts = append(parts, s)
	}
	if n := identName(m["name"]); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, ".")
}

func lookupMetadataColumns(metadata map[string][]string, tableRef string) ([]string, bool) {
	if len(metadata) == 0 || tableRef == "" {
		return nil, false
	}
	if cols, ok := metadata[tableRef]; ok {
		return append([]string{}, cols...), true
	}
	suffix := tableRef
	if !strings.Contains(tableRef, ".") {
		suffix = "." + tableRef
	}
	var found []string
	var count int
	for key, cols := range metadata {
		if key == tableRef || strings.HasSuffix(key, suffix) {
			found = cols
			count++
		}
	}
	if count == 1 {
		return append([]string{}, found...), true
	}
	return nil, false
}

func columnsFromAnalysis(a polyglot.QueryAnalysis, metadata map[string][]string) []string {
	if needsStarFallback(a, metadata) {
		return []string{"*"}
	}

	starByIndex := make(map[int]polyglot.StarProjectionFact, len(a.StarProjections))
	starSpan := make(map[int]int, len(a.StarProjections))
	for _, sp := range a.StarProjections {
		starByIndex[sp.Index] = sp
		if len(sp.ExpandedColumns) > 0 && !isPlaceholderStar(sp.ExpandedColumns) {
			starSpan[sp.Index] = len(sp.ExpandedColumns)
		}
	}

	out := make([]string, 0, len(a.Projections))
	for i := 0; i < len(a.Projections); {
		if n, ok := starSpan[i]; ok {
			out = append(out, orderStarColumns(starByIndex[i], a, metadata)...)
			i += n
			continue
		}
		p := a.Projections[i]
		if p.IsStar {
			if sp, ok := starByIndex[p.Index]; ok {
				if expanded := orderStarColumns(sp, a, metadata); len(expanded) > 0 {
					out = append(out, expanded...)
					i++
					continue
				}
			}
			if p.Name != nil && *p.Name != "" {
				out = append(out, *p.Name)
			} else {
				out = append(out, "*")
			}
			i++
			continue
		}
		if p.Name != nil && *p.Name != "" {
			out = append(out, *p.Name)
		} else {
			out = append(out, fmt.Sprintf("_col%d", p.Index))
		}
		i++
	}
	return finalizeColumnOrder(out, a, metadata)
}

func isPlaceholderStar(cols []string) bool {
	for _, c := range cols {
		if c == "*" {
			return true
		}
	}
	return false
}

func finalizeColumnOrder(out []string, a polyglot.QueryAnalysis, metadata map[string][]string) []string {
	if len(out) <= 1 || len(metadata) == 0 {
		return out
	}
	if len(out) == 1 && out[0] == "*" {
		return out
	}
	if spansMultipleMetadataTables(out, metadata) {
		return reorderMultiTableStar(out, metadata)
	}
	if len(a.BaseTables) == 1 {
		return orderExpandedColumns(out, nil, a, metadata)
	}
	return out
}

func spansMultipleMetadataTables(cols []string, metadata map[string][]string) bool {
	colToTable := make(map[string]string)
	for table, tableCols := range metadata {
		for _, c := range tableCols {
			colToTable[c] = table
		}
	}
	var seen string
	for _, c := range cols {
		table := colToTable[c]
		if table == "" {
			continue
		}
		if seen == "" {
			seen = table
			continue
		}
		if table != seen {
			return true
		}
	}
	return false
}

func orderStarColumns(sp polyglot.StarProjectionFact, a polyglot.QueryAnalysis, metadata map[string][]string) []string {
	if len(sp.ExpandedColumns) == 0 {
		return nil
	}
	if sp.Table != nil && *sp.Table != "" {
		return orderExpandedColumns(sp.ExpandedColumns, sp.Table, a, metadata)
	}
	if len(a.BaseTables) > 1 {
		return reorderMultiTableStar(sp.ExpandedColumns, metadata)
	}
	return orderExpandedColumns(sp.ExpandedColumns, nil, a, metadata)
}

func reorderMultiTableStar(expanded []string, metadata map[string][]string) []string {
	if len(metadata) == 0 || len(expanded) == 0 {
		return expanded
	}
	colToTable := make(map[string]string)
	for table, cols := range metadata {
		for _, c := range cols {
			colToTable[c] = table
		}
	}
	expandedSet := make(map[string]struct{}, len(expanded))
	for _, c := range expanded {
		expandedSet[c] = struct{}{}
	}
	var tableOrder []string
	seenTable := make(map[string]struct{})
	for _, c := range expanded {
		table := colToTable[c]
		if table == "" {
			continue
		}
		if _, ok := seenTable[table]; !ok {
			tableOrder = append(tableOrder, table)
			seenTable[table] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(expanded))
	added := make(map[string]struct{}, len(expanded))
	for _, table := range tableOrder {
		cols, ok := lookupMetadataColumns(metadata, table)
		if !ok {
			continue
		}
		for _, c := range cols {
			if _, in := expandedSet[c]; !in {
				continue
			}
			if _, dup := added[c]; dup {
				continue
			}
			ordered = append(ordered, c)
			added[c] = struct{}{}
		}
	}
	if len(ordered) == len(expanded) {
		return ordered
	}
	return expanded
}

func orderExpandedColumns(expanded []string, starTable *string, a polyglot.QueryAnalysis, metadata map[string][]string) []string {
	if len(metadata) == 0 {
		return expanded
	}
	expandedSet := make(map[string]struct{}, len(expanded))
	for _, c := range expanded {
		expandedSet[c] = struct{}{}
	}

	var tables []polyglot.RelationFact
	if starTable != nil && *starTable != "" {
		for _, bt := range a.BaseTables {
			if bt.Alias != nil && *bt.Alias == *starTable {
				tables = append(tables, bt)
				break
			}
			if bt.Name == *starTable || strings.HasSuffix(bt.Name, "."+*starTable) {
				tables = append(tables, bt)
				break
			}
		}
	} else {
		if len(a.BaseTables) > 1 {
			return expanded
		}
		tables = a.BaseTables
	}

	ordered := make([]string, 0, len(expanded))
	seen := make(map[string]struct{}, len(expanded))
	for _, bt := range tables {
		cols, ok := lookupMetadataColumns(metadata, bt.Name)
		if !ok {
			continue
		}
		for _, c := range cols {
			if _, in := expandedSet[c]; !in {
				continue
			}
			if _, dup := seen[c]; dup {
				continue
			}
			ordered = append(ordered, c)
			seen[c] = struct{}{}
		}
	}
	if len(ordered) == len(expanded) {
		return ordered
	}
	return expanded
}

func needsStarFallback(a polyglot.QueryAnalysis, metadata map[string][]string) bool {
	for _, sp := range a.StarProjections {
		if sp.Table != nil && *sp.Table != "" {
			continue
		}
		if len(sp.ExpandedColumns) == 0 {
			return true
		}
		if len(a.BaseTables) > 1 && !allBaseTablesInMetadata(a.BaseTables, metadata) {
			return true
		}
	}
	for _, p := range a.Projections {
		if !p.IsStar {
			continue
		}
		if p.Name != nil && *p.Name == "*" && len(a.BaseTables) > 1 &&
			!allBaseTablesInMetadata(a.BaseTables, metadata) {
			return true
		}
	}
	return false
}

func allBaseTablesInMetadata(baseTables []polyglot.RelationFact, metadata map[string][]string) bool {
	if len(metadata) == 0 {
		return false
	}
	for _, bt := range baseTables {
		if bt.Name == "" {
			continue
		}
		if _, ok := lookupMetadataColumns(metadata, bt.Name); !ok {
			return false
		}
	}
	return true
}
