package easysql

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// TableRef identifies a table-like relation. Catalog and Schema are optional;
// Table is required when the ref is used as a rewrite target.
type TableRef struct {
	Catalog string
	Schema  string
	Table   string
}

// TableRewrite describes how a physical table reference should be rewritten.
// MatchKey is the lower-case "schema.table" key to match after transparent
// catalogs configured by StripMatchCatalogs have been removed.
type TableRewrite struct {
	MatchKey string

	// Inline replaces the matched table reference with this table. Mutually
	// exclusive with Union.
	Inline *TableRef

	// Union replaces the matched table reference with a derived table backed by
	// UNION DISTINCT branches. Mutually exclusive with Inline.
	Union *UnionRewrite
}

// UnionRewrite describes a derived table backed by UNION DISTINCT branches.
// TableAlias is only used when the matched table reference has no explicit
// alias of its own; an explicit alias always wins so existing column references
// keep resolving.
type UnionRewrite struct {
	TableAlias string
	Columns    []string
	Branches   []TableRef
}

// RewriteTablesOption configures RewriteTableReferences.
type RewriteTablesOption func(*rewriteTablesConfig)

type rewriteTablesConfig struct {
	dialect            string
	stripMatchCatalogs map[string]struct{}
}

// WithRewriteDialect selects the SQL dialect used by RewriteTableReferences.
// It accepts the same dialect names as ApplyRowFilter.
func WithRewriteDialect(dialect string) RewriteTablesOption {
	return func(o *rewriteTablesConfig) { o.dialect = dialect }
}

// StripMatchCatalogs treats the listed catalogs as transparent while matching
// table references. For example, with StripMatchCatalogs("vdm_rda"),
// vdm_rda.sales.orders matches the "sales.orders" rewrite key.
func StripMatchCatalogs(catalogs ...string) RewriteTablesOption {
	return func(o *rewriteTablesConfig) {
		if o.stripMatchCatalogs == nil {
			o.stripMatchCatalogs = map[string]struct{}{}
		}
		for _, catalog := range catalogs {
			catalog = strings.ToLower(strings.TrimSpace(catalog))
			if catalog != "" {
				o.stripMatchCatalogs[catalog] = struct{}{}
			}
		}
	}
}

// RewriteTableReferences rewrites physical table references in a single SELECT
// or set-operation statement using a caller-provided rewrite plan.
//
// Every matched table reference is replaced by a derived-table subquery (an
// inline single-table copy, or a UNION DISTINCT of several tables) that inherits
// the reference's alias -- or, when it had none, the reference's bare name
// (uniquely disambiguated if two distinct tables would collide on it).
//
// Schema- and catalog-qualified column references (schema.table.col,
// catalog.schema.table.col) and qualified stars (schema.table.*, …) are
// rebound -- their leading qualifiers are dropped so they point at the
// derived table's alias. Such qualified forms can only denote a physical
// table, so rebinding them is scope-safe. Bare, single-segment references
// (table.col, table.*) are rebound only when the derived alias differs from
// the bare table name and the bare name is not bound to another in-scope
// relation (CTE, sibling alias, or inner-scope alias); otherwise they already
// resolve or must stay with the shadowing relation.
//
// The whole rewrite operates on the parsed AST: target relations and column
// identifiers are built by substituting leaf identifiers into a parsed
// placeholder skeleton, never by concatenating SQL text, so hostile identifiers
// cannot break parsing and per-dialect identifier quoting is emitted by the
// generator. String literals (including dialect-specific forms such as Postgres
// dollar quoting) are preserved by the parser rather than scanned by hand;
// comments are dropped from the rewritten output.
//
// The function only performs structural SQL mutation. It does not make access
// control decisions, inspect permissions, or apply catalog policy. The output is
// generated from the parsed AST and re-parsed before it is returned; parse or
// generation failures are classified with ErrParse, ErrUnsupported, or
// ErrInternal.
func RewriteTableReferences(sql string, specs []TableRewrite, opts ...RewriteTablesOption) (string, error) {
	if len(specs) == 0 {
		return sql, nil
	}

	client, err := defaultClient()
	if err != nil {
		return "", err
	}

	cfg := rewriteTablesConfig{dialect: "trino", stripMatchCatalogs: map[string]struct{}{}}
	for _, opt := range opts {
		opt(&cfg)
	}
	if strings.TrimSpace(cfg.dialect) == "" {
		cfg.dialect = "trino"
	}
	pg, ok := dialectToPolyglot[cfg.dialect]
	if !ok {
		return "", fmt.Errorf("easysql: unknown dialect %q", cfg.dialect)
	}

	plan, err := compileTableRewritePlan(specs)
	if err != nil {
		return "", err
	}

	// Reuse the row-filter machinery for parsing, single-supported-statement
	// validation and the scope-aware table walk (which correctly skips CTE
	// references). An empty scope means every physical table is a candidate.
	r := &rewriter{
		client:  client,
		dialect: cfg.dialect,
		pg:      pg,
		byKey:   map[string]bool{},
		byName:  map[string]bool{},
	}
	if cfg.dialect == "starrocks" {
		r.normalize = starrocksNormalizer
	}

	stmts, decs, err := r.prepare(sql)
	if err != nil {
		return "", err
	}

	var matches []*tableMatch
	for _, d := range decs {
		if !d.wrap {
			continue
		}
		table, _ := d.node["table"].(map[string]any)
		if table == nil {
			continue
		}
		key := tableRewriteMatchKey(table, cfg.stripMatchCatalogs)
		if key == "" {
			continue
		}
		spec, ok := plan[key]
		if !ok {
			continue
		}
		matches = append(matches, newTableMatch(d.node, table, spec))
	}

	// Nothing matched: a genuine no-op. Return the input unchanged rather than
	// round-tripping it through the generator (which would reformat untouched
	// SQL).
	if len(matches) == 0 {
		return sql, nil
	}

	assignDerivedAliases(stmts[0], matches)
	rebindCtx := buildRebindContext(matches)

	for _, m := range matches {
		sub, err := m.buildSubquery(client, pg)
		if err != nil {
			return "", err
		}
		delete(m.node, "table")
		m.node["subquery"] = sub
	}

	rebindColumnRefs(stmts[0], rebindCtx)

	// Drop comments so the regenerated SQL cannot carry stale commented-out text
	// or leak intent the caller placed in comments. Comments were separated out
	// by the parser (into *_comments fields), so this touches no string literal.
	stripASTComments(stmts[0])

	gen, err := client.Generate(mustMarshal(stmts), pg)
	if err != nil || len(gen) == 0 {
		return "", fmt.Errorf("%w: generate failed: %v", ErrInternal, err)
	}
	out := gen[0]
	if _, err := client.ParseOne(out, pg); err != nil {
		return "", fmt.Errorf("%w: rewritten SQL failed to re-parse: %v\nrewritten: %s",
			ErrInternal, err, out)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Plan compilation
// ---------------------------------------------------------------------------

type compiledTableRewrite struct {
	inline *TableRef
	union  *UnionRewrite
}

func compileTableRewritePlan(specs []TableRewrite) (map[string]compiledTableRewrite, error) {
	plan := make(map[string]compiledTableRewrite, len(specs))
	for _, spec := range specs {
		key := strings.ToLower(strings.TrimSpace(spec.MatchKey))
		if key == "" {
			return nil, errors.New("easysql: table rewrite match key must not be empty")
		}
		if _, exists := plan[key]; exists {
			return nil, fmt.Errorf("easysql: duplicate table rewrite match key %q", spec.MatchKey)
		}
		if (spec.Inline == nil) == (spec.Union == nil) {
			return nil, fmt.Errorf("easysql: rewrite %q must have exactly one target", spec.MatchKey)
		}
		if spec.Inline != nil {
			if strings.TrimSpace(spec.Inline.Table) == "" {
				return nil, fmt.Errorf("easysql: inline rewrite %q requires a target table", spec.MatchKey)
			}
			plan[key] = compiledTableRewrite{inline: spec.Inline}
			continue
		}
		if strings.TrimSpace(spec.Union.TableAlias) == "" {
			return nil, fmt.Errorf("easysql: union rewrite %q requires a table alias", spec.MatchKey)
		}
		if len(spec.Union.Columns) == 0 {
			return nil, fmt.Errorf("easysql: union rewrite %q requires at least one column", spec.MatchKey)
		}
		if len(spec.Union.Branches) == 0 {
			return nil, fmt.Errorf("easysql: union rewrite %q requires at least one branch", spec.MatchKey)
		}
		for _, branch := range spec.Union.Branches {
			if strings.TrimSpace(branch.Table) == "" {
				return nil, fmt.Errorf("easysql: union rewrite %q has a branch without table", spec.MatchKey)
			}
		}
		plan[key] = compiledTableRewrite{union: spec.Union}
	}
	return plan, nil
}

// tableRewriteMatchKey builds the lower-case "schema.table" match key for a
// table node, treating stripCatalogs as transparent. It returns "" for tables
// that cannot match a rewrite (bare names, or a non-transparent catalog).
func tableRewriteMatchKey(table map[string]any, stripCatalogs map[string]struct{}) string {
	name := strings.ToLower(identName(table["name"]))
	if name == "" {
		return ""
	}
	schema := strings.ToLower(identName(table["schema"]))
	catalog := strings.ToLower(identName(table["catalog"]))
	if catalog != "" {
		if _, strip := stripCatalogs[catalog]; strip {
			catalog = ""
		}
	}
	if catalog != "" || schema == "" {
		return ""
	}
	return schema + "." + name
}

// ---------------------------------------------------------------------------
// Matched table references
// ---------------------------------------------------------------------------

// aliasRef is an identifier name together with whether it must be quoted.
type aliasRef struct {
	name   string
	quoted bool
}

// tableMatch is one physical-table reference that matched a rewrite spec.
type tableMatch struct {
	node  map[string]any // the {"table":...} wrapper, mutated to {"subquery":...}
	table map[string]any // node["table"]
	spec  compiledTableRewrite

	// Identity as written (lower-cased), used to match column references.
	nameL    string
	schemaL  string
	catalogL string

	// hasAlias records whether the reference carried an explicit alias, in
	// which case identity is "" and column references need no rebinding.
	hasAlias bool
	identity string // "catalog\x00schema\x00name" (lower) for unaliased tables

	// alias is the derived-table alias, assigned by assignDerivedAliases.
	alias aliasRef
}

func newTableMatch(node, table map[string]any, spec compiledTableRewrite) *tableMatch {
	m := &tableMatch{
		node:     node,
		table:    table,
		spec:     spec,
		nameL:    strings.ToLower(identName(table["name"])),
		schemaL:  strings.ToLower(identName(table["schema"])),
		catalogL: strings.ToLower(identName(table["catalog"])),
	}
	if a, ok := table["alias"].(map[string]any); ok && a != nil {
		m.hasAlias = true
		m.alias = aliasRef{name: identName(a), quoted: identQuoted(a)}
	} else {
		m.identity = m.catalogL + "\x00" + m.schemaL + "\x00" + m.nameL
	}
	return m
}

// defaultAlias is the derived-table alias an unaliased match prefers before
// collision resolution: the caller-chosen union alias, or the bare table name
// for an inline rewrite (preserving its original quoting).
func (m *tableMatch) defaultAlias() aliasRef {
	if m.spec.union != nil {
		return aliasRef{name: m.spec.union.TableAlias, quoted: needsQuote(m.spec.union.TableAlias)}
	}
	return aliasRef{name: identName(m.table["name"]), quoted: identQuoted(m.table["name"])}
}

// claimedQualifiers lists the multi-segment column-reference qualifiers
// (lower-cased) that unambiguously resolve to this unaliased table: schema.table
// and, when written, catalog.schema.table. The bare name is intentionally
// excluded -- a single-segment qualifier is scope-sensitive and either already
// matches the derived alias or denotes a different relation.
func (m *tableMatch) claimedQualifiers() [][]string {
	if m.schemaL == "" {
		return nil
	}
	quals := [][]string{{m.schemaL, m.nameL}}
	if m.catalogL != "" {
		quals = append(quals, []string{m.catalogL, m.schemaL, m.nameL})
	}
	return quals
}

func (m *tableMatch) buildSubquery(client *polyglot.Client, pg string) (map[string]any, error) {
	if m.spec.inline != nil {
		return buildInlineSubquery(client, pg, *m.spec.inline, m.alias)
	}
	return buildUnionSubquery(client, pg, *m.spec.union, m.alias)
}

// ---------------------------------------------------------------------------
// Derived-table alias assignment (collision resolution)
// ---------------------------------------------------------------------------

// assignDerivedAliases picks a derived-table alias for every match. Aliased
// references keep their alias. Unaliased references default to their preferred
// alias, but when two distinct tables would claim the same name (or the name is
// already taken by an existing alias/CTE), the colliding tables are given
// unique, schema-qualified aliases so the rewritten FROM has no duplicate
// relation names.
func assignDerivedAliases(stmt any, matches []*tableMatch) {
	scopeOccupied := computeMatchScopeOccupancy(stmt, matches)

	var order []string
	byIdentity := map[string][]*tableMatch{}
	defaults := map[string]aliasRef{}
	for _, m := range matches {
		if m.hasAlias {
			continue
		}
		if _, seen := byIdentity[m.identity]; !seen {
			order = append(order, m.identity)
			defaults[m.identity] = m.defaultAlias()
		}
		byIdentity[m.identity] = append(byIdentity[m.identity], m)
	}

	distinctPerName := map[string]int{}
	for _, id := range order {
		distinctPerName[strings.ToLower(defaults[id].name)]++
	}

	used := map[string]bool{}
	for _, occ := range scopeOccupied {
		for n := range occ {
			used[n] = true
		}
	}
	needsDisamb := map[string]bool{}
	for _, id := range order {
		nameL := strings.ToLower(defaults[id].name)
		if distinctPerName[nameL] > 1 {
			needsDisamb[id] = true
			continue
		}
		for _, m := range byIdentity[id] {
			if scopeOccupied[m][nameL] {
				needsDisamb[id] = true
				break
			}
		}
	}

	assigned := map[string]aliasRef{}
	// Reserve the non-colliding names first so disambiguation avoids them.
	for _, id := range order {
		if needsDisamb[id] {
			continue
		}
		a := defaults[id]
		assigned[id] = a
		used[strings.ToLower(a.name)] = true
	}
	for _, id := range order {
		if !needsDisamb[id] {
			continue
		}
		a := disambiguateAlias(id, defaults[id], used)
		assigned[id] = a
		used[strings.ToLower(a.name)] = true
	}

	for id, group := range byIdentity {
		for _, m := range group {
			m.alias = assigned[id]
		}
	}
}

// disambiguateAlias derives a unique alias for a colliding table identity,
// preferring schema_table, then catalog_schema_table, then numeric suffixes.
func disambiguateAlias(identity string, def aliasRef, used map[string]bool) aliasRef {
	parts := strings.SplitN(identity, "\x00", 3)
	catalogL, schemaL, nameL := parts[0], parts[1], parts[2]

	var candidates []string
	if schemaL != "" {
		candidates = append(candidates, schemaL+"_"+nameL)
	}
	if catalogL != "" && schemaL != "" {
		candidates = append(candidates, catalogL+"_"+schemaL+"_"+nameL)
	}
	for _, c := range candidates {
		if !used[strings.ToLower(c)] {
			return aliasRef{name: c, quoted: needsQuote(c)}
		}
	}
	for i := 2; ; i++ {
		c := def.name + "_" + strconv.Itoa(i)
		if !used[strings.ToLower(c)] {
			return aliasRef{name: c, quoted: needsQuote(c)}
		}
	}
}

// relationEntryName returns the lower-case relation name bound by a FROM/JOIN
// entry (physical table or derived table). Projection aliases are ignored.
func relationEntryName(entry map[string]any) (string, bool) {
	if tbl, ok := entry["table"].(map[string]any); ok && tbl != nil {
		if a, ok := tbl["alias"].(map[string]any); ok && a != nil {
			if n := identName(a); n != "" {
				return strings.ToLower(n), true
			}
		}
		if n := identName(tbl["name"]); n != "" {
			return strings.ToLower(n), true
		}
	}
	if sub, ok := entry["subquery"].(map[string]any); ok && sub != nil {
		if n := identName(sub["alias"]); n != "" {
			return strings.ToLower(n), true
		}
	}
	return "", false
}

func fromJoinEntries(body map[string]any) []map[string]any {
	var entries []map[string]any
	if from, ok := body["from"].(map[string]any); ok {
		if exprs, ok := from["expressions"].([]any); ok {
			for _, e := range exprs {
				if entry, ok := e.(map[string]any); ok {
					entries = append(entries, entry)
				}
			}
		}
	}
	if joins, ok := body["joins"].([]any); ok {
		for _, j := range joins {
			if jm, ok := j.(map[string]any); ok {
				if this, ok := jm["this"].(map[string]any); ok {
					entries = append(entries, this)
				}
			}
		}
	}
	return entries
}

func collectFromRelationNames(body map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, entry := range fromJoinEntries(body) {
		if n, ok := relationEntryName(entry); ok {
			names[n] = true
		}
	}
	return names
}

func cteScopeNames(scope []map[string]bool) map[string]bool {
	names := map[string]bool{}
	for _, set := range scope {
		for n := range set {
			names[n] = true
		}
	}
	return names
}

func innerQueryBody(sub map[string]any) map[string]any {
	if this, ok := sub["this"].(map[string]any); ok {
		return queryBody(this)
	}
	return nil
}

// isSetOperationBody reports whether body is the inner object of a UNION /
// INTERSECT / EXCEPT node (as opposed to a plain SELECT body).
func isSetOperationBody(body map[string]any) bool {
	_, ok := body["left"]
	return ok
}

func extendScopeWithCTEs(body map[string]any, cteScope []map[string]bool, walk func(any, []map[string]bool)) []map[string]bool {
	w, ok := body["with"].(map[string]any)
	if !ok {
		return cteScope
	}
	if list, ok := w["ctes"].([]any); ok {
		for _, e := range list {
			if cte, ok := e.(map[string]any); ok {
				walk(cte["this"], cteScope)
			}
		}
	}
	names := cteNames(w)
	if len(names) > 0 {
		return append(append([]map[string]bool{}, cteScope...), names)
	}
	return cteScope
}

func sameASTMap(a, b map[string]any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// computeMatchScopeOccupancy maps each unaliased match to the lower-cased
// relation names already bound in the same FROM scope (CTEs visible there plus
// sibling FROM/JOIN relations), excluding the match's own FROM entry.
func computeMatchScopeOccupancy(stmt any, matches []*tableMatch) map[*tableMatch]map[string]bool {
	result := map[*tableMatch]map[string]bool{}

	var walk func(node any, cteScope []map[string]bool)
	var walkQuery func(body map[string]any, cteScope []map[string]bool)
	walkQuery = func(body map[string]any, cteScope []map[string]bool) {
		if isSetOperationBody(body) {
			newScope := extendScopeWithCTEs(body, cteScope, walk)
			walk(body["left"], newScope)
			walk(body["right"], newScope)
			walk(body["order_by"], newScope)
			return
		}

		newScope := extendScopeWithCTEs(body, cteScope, walk)
		for _, entry := range fromJoinEntries(body) {
			occ := cteScopeNames(newScope)
			for _, other := range fromJoinEntries(body) {
				if sameASTMap(other, entry) {
					continue
				}
				if n, ok := relationEntryName(other); ok {
					occ[n] = true
				}
			}
			for _, m := range matches {
				if m.hasAlias || !sameASTMap(m.node, entry) {
					continue
				}
				result[m] = occ
			}
		}
		queryRelations := maps.Clone(cteScopeNames(newScope))
		for n := range collectFromRelationNames(body) {
			queryRelations[n] = true
		}
		walkQueryParts(body, newScope, queryRelations, walk, walkQuery)
	}
	walk = func(node any, cteScope []map[string]bool) {
		switch v := node.(type) {
		case map[string]any:
			if body := queryBody(v); body != nil {
				walkQuery(body, cteScope)
				return
			}
			if sub, ok := v["subquery"].(map[string]any); ok {
				if inner := innerQueryBody(sub); inner != nil {
					walkQuery(inner, cteScope)
				}
				return
			}
			for _, c := range v {
				walk(c, cteScope)
			}
		case []any:
			for _, c := range v {
				walk(c, cteScope)
			}
		}
	}

	if m, ok := stmt.(map[string]any); ok {
		if body := queryBody(m); body != nil {
			walkQuery(body, nil)
		}
	}
	return result
}

func walkQueryParts(body map[string]any, cteScope []map[string]bool, queryRelations map[string]bool, walk func(any, []map[string]bool), walkQuery func(map[string]any, []map[string]bool)) {
	walkWithRelations := func(node any) {
		walkWithQueryRelations(node, cteScope, queryRelations, walk, walkQuery)
	}
	walkWithRelations(body["hint"])
	walkWithRelations(body["expressions"])
	walkWithRelations(body["from"])
	if joins, ok := body["joins"].([]any); ok {
		for _, j := range joins {
			if jm, ok := j.(map[string]any); ok {
				walkWithRelations(jm["this"])
				walkWithRelations(jm["on"])
				walkWithRelations(jm["using"])
			}
		}
	}
	for _, k := range []string{
		"where_clause", "group_by", "having", "qualify", "windows",
		"distinct_on", "sort_by", "order_by", "lateral_views", "connect",
	} {
		walkWithRelations(body[k])
	}
}

func walkWithQueryRelations(node any, cteScope []map[string]bool, queryRelations map[string]bool, walk func(any, []map[string]bool), walkQuery func(map[string]any, []map[string]bool)) {
	switch v := node.(type) {
	case map[string]any:
		if body := queryBody(v); body != nil {
			walkQuery(body, cteScope)
			return
		}
		if sub, ok := v["subquery"].(map[string]any); ok {
			if inner := innerQueryBody(sub); inner != nil {
				walkQuery(inner, cteScope)
			}
			return
		}
		for _, c := range v {
			walkWithQueryRelations(c, cteScope, queryRelations, walk, walkQuery)
		}
	case []any:
		for _, c := range v {
			walkWithQueryRelations(c, cteScope, queryRelations, walk, walkQuery)
		}
	}
}

// ---------------------------------------------------------------------------
// Column-reference rebinding
// ---------------------------------------------------------------------------

// rebindContext carries qualified and bare-name rebinding rules for rewritten
// unaliased tables.
type rebindContext struct {
	qualified map[string]aliasRef
	bare      map[string]aliasRef
}

func buildRebindContext(matches []*tableMatch) rebindContext {
	qualified := map[string]aliasRef{}
	bare := map[string]aliasRef{}
	for _, m := range matches {
		if m.hasAlias {
			continue
		}
		for _, q := range m.claimedQualifiers() {
			qualified[strings.Join(q, "\x00")] = m.alias
		}
		if strings.ToLower(m.alias.name) != m.nameL {
			bare[m.nameL] = m.alias
		}
	}
	return rebindContext{qualified: qualified, bare: bare}
}

// identInfo is one identifier in a flattened column reference.
type identInfo struct {
	node  map[string]any
	nameL string
}

// refChain flattens a column/dot reference into its ordered identifier
// components (qualifiers followed by the field). It returns false when node is
// not a column reference.
func refChain(node map[string]any) ([]identInfo, bool) {
	if dn, ok := node["dot"].(map[string]any); ok {
		inner, ok := dn["this"].(map[string]any)
		if !ok {
			return nil, false
		}
		chain, ok := refChain(inner)
		if !ok {
			return nil, false
		}
		field, ok := dn["field"].(map[string]any)
		if !ok {
			return nil, false
		}
		return append(chain, identInfo{node: field, nameL: strings.ToLower(identName(field))}), true
	}
	if col, ok := node["column"].(map[string]any); ok {
		var chain []identInfo
		if tbl, ok := col["table"].(map[string]any); ok && tbl != nil {
			chain = append(chain, identInfo{node: tbl, nameL: strings.ToLower(identName(tbl))})
		}
		name, ok := col["name"].(map[string]any)
		if !ok {
			return nil, false
		}
		chain = append(chain, identInfo{node: name, nameL: strings.ToLower(identName(name))})
		return chain, true
	}
	return nil, false
}

// rebindColumnRefs walks the statement and rewrites column references that must
// point at a derived-table alias after the rewrite.
func rebindColumnRefs(stmt any, ctx rebindContext) {
	if m, ok := stmt.(map[string]any); ok {
		if body := queryBody(m); body != nil {
			rebindQuery(body, nil, ctx)
		}
	}
}

func rebindQuery(body map[string]any, cteScope []map[string]bool, ctx rebindContext) {
	if isSetOperationBody(body) {
		newScope := extendScopeWithCTEs(body, cteScope, func(n any, scope []map[string]bool) {
			rebindNode(n, scope, ctx)
		})
		rebindNode(body["left"], newScope, ctx)
		rebindNode(body["right"], newScope, ctx)
		rebindNode(body["order_by"], newScope, ctx)
		return
	}

	newScope := extendScopeWithCTEs(body, cteScope, func(n any, scope []map[string]bool) {
		rebindNode(n, scope, ctx)
	})
	// relations is the set of bare names that already resolve to something in
	// this query scope, so a bare `name.col` ref must NOT be rebound onto a
	// rewritten table's derived alias. It is exactly the relations bound by this
	// body's FROM/JOIN -- physical tables, subquery aliases, and CTEs *referenced
	// in FROM* (which appear there as table entries). A CTE that is merely
	// defined in an enclosing WITH but never referenced in FROM binds no range
	// variable (per SQL scoping: "for a CTE to be visible it must contain the
	// query"), so it must not be seeded here from cteScope -- doing so would
	// leave a bare `cte.col` ref (that actually pointed at the same-named
	// physical table via its implicit alias) dangling after the table is renamed
	// for disambiguation, silently changing which relation the query reads.
	relations := collectFromRelationNames(body)
	rebindWithRelations := func(node any) {
		rebindNodeWithRelations(node, newScope, relations, ctx)
	}
	rebindWithRelations(body["hint"])
	rebindWithRelations(body["expressions"])
	rebindWithRelations(body["from"])
	if joins, ok := body["joins"].([]any); ok {
		for _, j := range joins {
			if jm, ok := j.(map[string]any); ok {
				rebindWithRelations(jm["this"])
				rebindWithRelations(jm["on"])
				rebindWithRelations(jm["using"])
			}
		}
	}
	for _, k := range []string{
		"where_clause", "group_by", "having", "qualify", "windows",
		"distinct_on", "sort_by", "order_by", "lateral_views", "connect",
	} {
		rebindWithRelations(body[k])
	}
}

func rebindNode(node any, cteScope []map[string]bool, ctx rebindContext) {
	switch v := node.(type) {
	case map[string]any:
		if body := queryBody(v); body != nil {
			rebindQuery(body, cteScope, ctx)
			return
		}
		if sub, ok := v["subquery"].(map[string]any); ok {
			if inner := innerQueryBody(sub); inner != nil {
				rebindQuery(inner, cteScope, ctx)
			}
			return
		}
		for _, c := range v {
			rebindNode(c, cteScope, ctx)
		}
	case []any:
		for _, c := range v {
			rebindNode(c, cteScope, ctx)
		}
	}
}

func rebindNodeWithRelations(node any, cteScope []map[string]bool, relations map[string]bool, ctx rebindContext) {
	switch v := node.(type) {
	case map[string]any:
		if body := queryBody(v); body != nil {
			rebindQuery(body, cteScope, ctx)
			return
		}
		if sub, ok := v["subquery"].(map[string]any); ok {
			if inner := innerQueryBody(sub); inner != nil {
				rebindQuery(inner, cteScope, ctx)
			}
			return
		}
		if _, ok := v["star"]; ok {
			if tryRebindStarAtScope(v, ctx, relations) {
				return
			}
		}
		if _, isDot := v["dot"]; isDot || v["column"] != nil {
			if tryRebindRefAtScope(v, ctx, relations) {
				return
			}
		}
		for _, c := range v {
			rebindNodeWithRelations(c, cteScope, relations, ctx)
		}
	case []any:
		for _, c := range v {
			rebindNodeWithRelations(c, cteScope, relations, ctx)
		}
	}
}

// tryRebindRefAtScope rewrites v in place to <alias>.<field> when its qualifier
// resolves to a rewritten unaliased table. Multi-segment qualifiers are rebound
// unconditionally; bare table.col references are rebound only when the bare name
// is not already bound to another relation in the current scope.
func tryRebindRefAtScope(v map[string]any, ctx rebindContext, relations map[string]bool) bool {
	chain, ok := refChain(v)
	if !ok || len(chain) < 2 {
		return false
	}
	field := chain[len(chain)-1]

	qualParts := make([]string, len(chain)-1)
	for i, q := range chain[:len(chain)-1] {
		qualParts[i] = q.nameL
	}
	a, matched := lookupRebindAlias(qualParts, ctx, relations)
	if !matched {
		return false
	}

	newCol := map[string]any{
		"join_mark":         false,
		"name":              deepCopyJSON(field.node),
		"table":             newIdent(a.name, a.quoted),
		"trailing_comments": []any{},
	}
	delete(v, "dot")
	delete(v, "column")
	v["column"] = newCol
	return true
}

// tryRebindStarAtScope rewrites a qualified star's table qualifier to the derived
// alias when it names a rewritten unaliased table, using the same scope rules as
// column references.
func tryRebindStarAtScope(v map[string]any, ctx rebindContext, relations map[string]bool) bool {
	star, ok := v["star"].(map[string]any)
	if !ok || star == nil {
		return false
	}
	parts, ok := starQualifierParts(star)
	if !ok || len(parts) == 0 {
		return false
	}
	a, matched := lookupRebindAlias(parts, ctx, relations)
	if !matched {
		return false
	}
	star["table"] = newIdent(a.name, a.quoted)
	return true
}

// starQualifierParts splits the lower-cased qualifier segments of a star node.
// An unqualified SELECT * yields ok=false.
func starQualifierParts(star map[string]any) ([]string, bool) {
	tbl, ok := star["table"].(map[string]any)
	if !ok || tbl == nil {
		return nil, false
	}
	name := identName(tbl)
	if name == "" {
		return nil, false
	}
	if !strings.Contains(name, ".") {
		return []string{strings.ToLower(name)}, true
	}
	segs := strings.Split(name, ".")
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = strings.ToLower(s)
	}
	return parts, true
}

// lookupRebindAlias resolves qualifier segments to a derived-table alias using
// the same qualified/bare rules as column-reference rebinding.
func lookupRebindAlias(parts []string, ctx rebindContext, relations map[string]bool) (aliasRef, bool) {
	switch len(parts) {
	case 0:
		return aliasRef{}, false
	case 1:
		tableL := parts[0]
		if relations[tableL] {
			return aliasRef{}, false
		}
		a, ok := ctx.bare[tableL]
		return a, ok
	default:
		a, ok := ctx.qualified[strings.Join(parts, "\x00")]
		return a, ok
	}
}

// ---------------------------------------------------------------------------
// AST-native derived-table construction
// ---------------------------------------------------------------------------

// Placeholder identifiers used in parse skeletons. They are simple identifiers
// so the skeleton always parses in any dialect; their leaf values are then
// substituted with the real (possibly hostile) identifiers.
var (
	phTableRe = regexp.MustCompile(`^__ez_t(\d+)__$`)
	phColRe   = regexp.MustCompile(`^__ez_c(\d+)__$`)
)

// buildInlineSubquery builds the AST for (SELECT * FROM <target>) AS <alias>.
func buildInlineSubquery(client *polyglot.Client, pg string, target TableRef, alias aliasRef) (map[string]any, error) {
	sub, err := parseSubqueryWrapper(client, pg, "SELECT * FROM __ez_t0__")
	if err != nil {
		return nil, err
	}
	if err := substitutePlaceholders(sub, []TableRef{target}, nil); err != nil {
		return nil, err
	}
	sub["alias"] = newIdent(alias.name, alias.quoted)
	return sub, nil
}

// buildUnionSubquery builds the AST for
// (SELECT <cols> FROM <branch0> UNION DISTINCT SELECT <cols> FROM <branch1> ...) AS <alias>.
func buildUnionSubquery(client *polyglot.Client, pg string, spec UnionRewrite, alias aliasRef) (map[string]any, error) {
	colPlaceholders := make([]string, len(spec.Columns))
	for j := range spec.Columns {
		colPlaceholders[j] = fmt.Sprintf("__ez_c%d__", j)
	}
	colList := strings.Join(colPlaceholders, ", ")

	branches := make([]string, len(spec.Branches))
	for k := range spec.Branches {
		branches[k] = fmt.Sprintf("SELECT %s FROM __ez_t%d__", colList, k)
	}
	inner := strings.Join(branches, " UNION DISTINCT ")

	sub, err := parseSubqueryWrapper(client, pg, inner)
	if err != nil {
		return nil, err
	}
	if err := substitutePlaceholders(sub, spec.Branches, spec.Columns); err != nil {
		return nil, err
	}
	sub["alias"] = newIdent(alias.name, alias.quoted)
	return sub, nil
}

// parseSubqueryWrapper parses "SELECT * FROM (<innerSQL>) AS <ph>" and returns
// the derived-table (subquery) node, which carries every field the generator
// requires.
func parseSubqueryWrapper(client *polyglot.Client, pg, innerSQL string) (map[string]any, error) {
	raw, err := client.ParseOne("SELECT * FROM ("+innerSQL+") AS __ez_alias__", pg)
	if err != nil {
		return nil, fmt.Errorf("%w: build derived table: %v", ErrInternal, err)
	}
	var stmt map[string]any
	if err := sonic.Unmarshal(raw, &stmt); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	exprs, ok := dig(stmt, "select", "from", "expressions")
	if !ok {
		return nil, fmt.Errorf("%w: malformed derived table template", ErrInternal)
	}
	list, ok := exprs.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%w: malformed derived table template", ErrInternal)
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: malformed derived table template", ErrInternal)
	}
	sub, ok := entry["subquery"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: malformed derived table template", ErrInternal)
	}
	return sub, nil
}

// substitutePlaceholders walks a parsed skeleton, replacing placeholder table
// nodes (__ez_tN__) with tables[N] and placeholder column identifiers
// (__ez_cN__) with columns[N]. Only leaf identifier values are touched, so the
// engine-required structure produced by the parser stays intact.
func substitutePlaceholders(node any, tables []TableRef, columns []string) error {
	switch v := node.(type) {
	case map[string]any:
		if tbl, ok := v["table"].(map[string]any); ok {
			if idx, ok := placeholderIndex(phTableRe, tbl["name"]); ok {
				if idx >= len(tables) {
					return fmt.Errorf("%w: table placeholder %d out of range", ErrInternal, idx)
				}
				setTableRef(tbl, tables[idx])
				return nil
			}
		}
		if col, ok := v["column"].(map[string]any); ok {
			if idx, ok := placeholderIndex(phColRe, col["name"]); ok {
				if idx >= len(columns) {
					return fmt.Errorf("%w: column placeholder %d out of range", ErrInternal, idx)
				}
				col["name"] = newIdent(columns[idx], needsQuote(columns[idx]))
				return nil
			}
		}
		for _, c := range v {
			if err := substitutePlaceholders(c, tables, columns); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range v {
			if err := substitutePlaceholders(c, tables, columns); err != nil {
				return err
			}
		}
	}
	return nil
}

func placeholderIndex(re *regexp.Regexp, ident any) (int, bool) {
	m := re.FindStringSubmatch(identName(ident))
	if m == nil {
		return 0, false
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return idx, true
}

// setTableRef sets a parsed table node's catalog/schema/name identifiers from a
// TableRef, leaving every other field (alias, hints, flags) as the parser
// produced it.
func setTableRef(table map[string]any, ref TableRef) {
	table["name"] = newIdent(ref.Table, needsQuote(ref.Table))
	table["schema"] = optIdent(ref.Schema)
	table["catalog"] = optIdent(ref.Catalog)
}

// ---------------------------------------------------------------------------
// Identifier helpers
// ---------------------------------------------------------------------------

var simpleIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// needsQuote reports whether an identifier must be quoted to survive parsing.
// The generator emits the dialect-correct quote characters from the quoted flag.
func needsQuote(name string) bool {
	return !simpleIdentRe.MatchString(name)
}

// newIdent builds an identifier AST node matching the parser's shape.
func newIdent(name string, quoted bool) map[string]any {
	return map[string]any{
		"name":              name,
		"quoted":            quoted,
		"trailing_comments": []any{},
	}
}

// optIdent returns an identifier node, or nil when the name is empty.
func optIdent(name string) any {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return newIdent(name, needsQuote(name))
}

// identQuoted reports whether an identifier node is marked quoted.
func identQuoted(v any) bool {
	if m, ok := v.(map[string]any); ok {
		if q, ok := m["quoted"].(bool); ok {
			return q
		}
	}
	return false
}

// stripASTComments empties every comment attachment on the AST (the parser
// stores them in *_comments fields, e.g. leading_comments / trailing_comments /
// operator_comments), so the regenerated SQL carries no comments. String
// literal values live elsewhere and are left untouched.
func stripASTComments(node any) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if strings.HasSuffix(k, "_comments") {
				if _, ok := child.([]any); ok {
					v[k] = []any{}
				}
				continue
			}
			stripASTComments(child)
		}
	case []any:
		for _, child := range v {
			stripASTComments(child)
		}
	}
}
