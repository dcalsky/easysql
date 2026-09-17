// This file implements ParseColumns: given a SQL statement it returns the
// field names the statement exposes, in projection order.
//
// Query-bearing statements (SELECT, WITH, UNION, CREATE VIEW, CREATE TABLE AS
// SELECT, INSERT ... SELECT, ...) are unwrapped to their inner query and
// inspected with Polyglot's OutputColumns APIs. CREATE VIEW / CREATE TABLE statements
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
// catalogs. A table absent from metadata means its schema was not provided and
// an unexpanded star falls back to ["*"]. A table present with an empty column
// list means the table is known to expose zero columns. For multi-table SELECT *
// every referenced base table must appear in metadata or the result falls back
// to ["*"].
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

	cfg, err := configuredLineageOptions(opts...)
	if err != nil {
		return nil, err
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

	innerSQL := sql
	if !isSupportedRoot(stmt) {
		innerSQL, err = generateStatement(client, inner, cfg.dialect)
		if err != nil {
			return nil, err
		}
	}

	// Inspect first without qualification: explicit names and unnamed slots
	// need no schema, and user aliases such as _col_0 must remain unchanged.
	output, err := client.OutputColumns(innerSQL, cfg.dialect)
	if err != nil {
		return nil, fmt.Errorf("%w: output columns failed: %v", ErrInternal, err)
	}
	expanded := false
	if !output.OrdinalComplete {
		if schema := metadataToSchema(cfg.metadata); schema != nil {
			output, err = client.OutputColumnsWithSchema(innerSQL, *schema, cfg.dialect)
			if err != nil {
				return nil, fmt.Errorf("%w: output columns failed: %v", ErrInternal, err)
			}
			expanded = true
		}
	}
	// Polyglot treats an empty column schema as open/unknown; easysql's
	// documented contract treats it as a known zero-column table. Reuse the
	// existing scope resolver only for this exceptional unresolved-star case.
	if !output.OrdinalComplete && hasEmptyTableMetadata(cfg.metadata) {
		q, err := decodeQueryMap(inner)
		if err != nil {
			return nil, err
		}
		rr := &refResolver{metadata: cfg.metadata, result: map[string]map[string]struct{}{}, flowOnly: true}
		resolved := rr.resolveQuery(q, nil)
		emptySource := false
		for table := range rr.result {
			if cols, ok := lookupMetadataColumns(cfg.metadata, table); ok && len(cols) == 0 {
				emptySource = true
				break
			}
		}
		if emptySource {
			names := append([]string{}, resolved.names...)
			for i, name := range names {
				if name == "*" {
					for _, col := range output.Columns {
						if col.Kind == polyglot.OutputColumnWildcard && col.Qualifier == nil {
							return []string{"*"}, nil
						}
					}
				}
				if name == "" {
					names[i] = fmt.Sprintf("_col%d", i)
				}
			}
			return names, nil
		}
	}
	names := make([]string, 0, len(output.Columns))
	for i, col := range output.Columns {
		switch col.Kind {
		case polyglot.OutputColumnNamed:
			name := *col.Name
			if expanded {
				name = normalizeSyntheticColumnName(name)
			}
			names = append(names, name)
		case polyglot.OutputColumnUnnamed:
			names = append(names, fmt.Sprintf("_col%d", i))
		case polyglot.OutputColumnWildcard:
			if col.Qualifier == nil {
				return []string{"*"}, nil
			}
			names = append(names, "*")
		}
	}
	return names, nil
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
	likeExpanded := false
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
			likeExpanded = true
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
		likeExpanded = true
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
		// A LIKE clause was expanded but the source table exposes zero columns.
		// Return a non-nil empty slice so callers distinguish "a query defining
		// no columns" from nil, which means "not a column-producing statement".
		if likeExpanded {
			return []string{}, nil
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
	var found []string
	var count int
	for key, cols := range metadata {
		if hasTableSuffix(key, tableRef) {
			found = cols
			count++
		}
	}
	if count == 1 {
		return append([]string{}, found...), true
	}
	return nil, false
}

// normalizeSyntheticColumnName converts an engine-internal `_col_{n}` name to
// the documented `_col{n}` form, leaving all other names untouched.
func normalizeSyntheticColumnName(name string) string {
	const prefix = "_col_"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return name
	}
	for _, r := range name[len(prefix):] {
		if r < '0' || r > '9' {
			return name
		}
	}
	return "_col" + name[len(prefix):]
}

func hasEmptyTableMetadata(metadata map[string][]string) bool {
	for _, cols := range metadata {
		if len(cols) == 0 {
			return true
		}
	}
	return false
}
