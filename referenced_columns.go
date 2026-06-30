// This file implements ReferencedColumns: given a SQL statement it returns,
// for every root physical table, the columns referenced *anywhere* in the
// statement — projection, WHERE, JOIN ... ON, GROUP BY, HAVING, ORDER BY,
// QUALIFY and window clauses alike. It is the complement of LineageSourceColumns
// (which reports only columns that flow into the result) and is a superset of
// it: a column appearing only in a filter position is included here but not
// there.
//
// Unlike the lineage analyzer, the native engine does not expose the source
// table of a filter-position column, so resolution is done structurally on the
// parsed AST. A small recursive resolver walks each query scope, maps every
// table/alias in its FROM and JOINs to a source (a physical table, or — for a
// CTE or derived subquery — the recursively resolved output→root-table mapping
// of its body), then attributes each referenced column to its root physical
// table. Unqualified columns are resolved against the supplied metadata; with no
// metadata they are attributed to every physical source in scope (a safe
// superset, suited to access-control / masking use cases).
//
// The structural parts of the AST (the SELECT body, table/column/subquery/CTE
// nodes, DML statements) are decoded into the typed structs below. Only the
// genuinely polymorphic expression subtrees (WHERE/ON/SET/projection values,
// which are arbitrary tagged-union nodes such as gt/eq/add/case/func) stay as
// raw JSON and are walked generically to find the column references inside them.

package easysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// ----------------------------------------------------------------------------
// Typed AST views. `expr` is an arbitrary expression subtree kept as raw JSON
// and walked generically; everything else is a structural node we fully model.
// ----------------------------------------------------------------------------

type expr = json.RawMessage

// ident is any identifier node ({"name": "...", "quoted": ...}). It models a
// table/column/alias name; quoting is preserved verbatim in Name.
type ident struct {
	Name string `json:"name"`
}

func (i *ident) text() string {
	if i == nil {
		return ""
	}
	return i.Name
}

type tableNode struct {
	Name    *ident `json:"name"`
	Schema  *ident `json:"schema"`
	Catalog *ident `json:"catalog"`
	Alias   *ident `json:"alias"`
}

type columnNode struct {
	Name  *ident `json:"name"`
	Table *ident `json:"table"`
}

type starNode struct {
	Table *ident `json:"table"`
}

type aliasNode struct {
	Alias *ident `json:"alias"`
	This  expr   `json:"this"`
}

type dotNode struct {
	This  expr   `json:"this"`
	Field *ident `json:"field"`
}

type subqueryNode struct {
	This          *queryNode `json:"this"`
	Alias         *ident     `json:"alias"`
	ColumnAliases []*ident   `json:"column_aliases"`
}

type pivotNode struct {
	This        expr   `json:"this"`
	Expressions []expr `json:"expressions"`
	Fields      []expr `json:"fields"`
}

type cteNode struct {
	Alias   *ident     `json:"alias"`
	Columns []*ident   `json:"columns"`
	This    *queryNode `json:"this"`
}

type withClause struct {
	CTEs []*cteNode `json:"ctes"`
}

type fromClause struct {
	Expressions []expr `json:"expressions"`
}

type joinClause struct {
	This  expr     `json:"this"`
	On    expr     `json:"on"`
	Using []*ident `json:"using"`
}

// selectBody is the inner object of a {"select": …} node.
type selectBody struct {
	Expressions  []expr       `json:"expressions"`
	From         *fromClause  `json:"from"`
	Joins        []joinClause `json:"joins"`
	Where        expr         `json:"where_clause"`
	GroupBy      expr         `json:"group_by"`
	Having       expr         `json:"having"`
	Qualify      expr         `json:"qualify"`
	Windows      expr         `json:"windows"`
	OrderBy      expr         `json:"order_by"`
	SortBy       expr         `json:"sort_by"`
	DistributeBy expr         `json:"distribute_by"`
	ClusterBy    expr         `json:"cluster_by"`
	Connect      expr         `json:"connect"`
	LateralViews expr         `json:"lateral_views"`
	With         *withClause  `json:"with"`
}

// setOpBody is the inner object of a {"union"|"intersect"|"except": …} node.
type setOpBody struct {
	Left  *queryNode  `json:"left"`
	Right *queryNode  `json:"right"`
	With  *withClause `json:"with"`
}

// queryNode is a SELECT or set operation (the unit innerQuery yields).
type queryNode struct {
	Select    *selectBody `json:"select"`
	Union     *setOpBody  `json:"union"`
	Intersect *setOpBody  `json:"intersect"`
	Except    *setOpBody  `json:"except"`
}

// fromEntry is the decoded shape of one FROM/JOIN list entry.
type fromEntry struct {
	Table    *tableNode    `json:"table"`
	Subquery *subqueryNode `json:"subquery"`
	Pivot    *pivotNode    `json:"pivot"`
	Unpivot  *pivotNode    `json:"unpivot"`
}

type deleteNode struct {
	Table   *tableNode   `json:"table"`
	Joins   []joinClause `json:"joins"`
	Using   []expr       `json:"using"`
	Where   expr         `json:"where_clause"`
	OrderBy expr         `json:"order_by"`
	With    *withClause  `json:"with"`
}

type updateNode struct {
	Table      *tableNode   `json:"table"`
	Set        [][]expr     `json:"set"`
	FromClause *fromClause  `json:"from_clause"`
	FromJoins  []joinClause `json:"from_joins"`
	Where      expr         `json:"where_clause"`
	OrderBy    expr         `json:"order_by"`
	With       *withClause  `json:"with"`
}

type mergeNode struct {
	This  expr        `json:"this"`
	Using expr        `json:"using"`
	On    expr        `json:"on"`
	Whens expr        `json:"whens"`
	With  *withClause `json:"with_"`
}

// statement carries only the DML wrappers we dispatch on; query-bearing
// statements (SELECT and DDL wrappers) go through innerQuery instead.
type statement struct {
	Delete *deleteNode `json:"delete"`
	Update *updateNode `json:"update"`
	Merge  *mergeNode  `json:"merge"`
}

// ----------------------------------------------------------------------------
// Public API
// ----------------------------------------------------------------------------

// ReferencedColumns returns, for each root physical table referenced by sql, the
// sorted list of columns touched anywhere in the statement (projection and
// filter positions alike). It accepts the same query-bearing statements as
// LineageSourceColumns (SELECT/UNION and the wrappers CREATE VIEW, CREATE TABLE
// AS SELECT, INSERT ... SELECT, …, unwrapped to their inner query first) plus
// the DML mutations DELETE / UPDATE / MERGE, and returns a superset of
// LineageSourceColumns's result.
//
// Resolution rules:
//
//   - CTEs and derived subqueries are seen through to their root physical
//     tables, exactly like LineageSourceColumns.
//   - A qualified column (t.c / schema.table.c) is attributed to the table its
//     qualifier resolves to in scope.
//   - An unqualified column is attributed to every in-scope source that declares
//     it in WithLineageMetadata. With no metadata it is attributed to every
//     physical source in that scope (a safe superset).
//   - SELECT * / t.* expand against metadata when available; a star that cannot
//     be expanded records the sentinel column "*" for the affected table.
//   - A reference whose table cannot be resolved is broadcast to every candidate
//     physical table in scope rather than dropped (fail-open by design).
//
// Every physical table that appears in a FROM/JOIN is present in the result even
// when no column is attributed to it (an empty list). The native SQL engine is
// loaded automatically on first use (see Init).
func ReferencedColumns(sql string, opts ...LineageOption) (map[string][]string, error) {
	client, err := defaultClient()
	if err != nil {
		return nil, err
	}
	return referencedColumns(client, sql, opts...)
}

func referencedColumns(client *polyglot.Client, sql string, opts ...LineageOption) (map[string][]string, error) {
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

	rr := &refResolver{metadata: cfg.metadata, result: map[string]map[string]struct{}{}}

	// DML mutations (DELETE / UPDATE / MERGE) are not query-bearing, so
	// innerQuery cannot reach the columns they read in their WHERE / SET / ON /
	// WHEN clauses. Decode and handle them explicitly so those reads survive.
	var st statement
	if err := decodeNode(stmt, &st); err != nil {
		return nil, err
	}
	switch {
	case st.Delete != nil:
		rr.resolveDelete(st.Delete)
		return sortedResult(rr.result), nil
	case st.Update != nil:
		rr.resolveUpdate(st.Update)
		return sortedResult(rr.result), nil
	case st.Merge != nil:
		rr.resolveMerge(st.Merge)
		return sortedResult(rr.result), nil
	}

	// CREATE VIEW / CTAS / INSERT ... SELECT etc. are unwrapped to their inner
	// query by the shared innerQuery helper before structural decoding.
	inner := innerQuery(stmt)
	if inner == nil {
		return sortedResult(rr.result), nil
	}
	q, err := decodeQueryMap(inner)
	if err != nil {
		return nil, err
	}
	rr.resolveQuery(q, nil)
	return sortedResult(rr.result), nil
}

// ----------------------------------------------------------------------------
// Resolver state
// ----------------------------------------------------------------------------

// colRef is a (root physical table, column) pair.
type colRef struct {
	table string
	col   string
}

// resolvedOut is the result of resolving a query scope: the ordered output
// column names and, per output name, the root (table, column) refs that produced
// it. A parent scope uses it to resolve references to this scope's alias.
type resolvedOut struct {
	names  []string
	byName map[string][]colRef
}

func emptyOut() *resolvedOut { return &resolvedOut{byName: map[string][]colRef{}} }

func (o *resolvedOut) add(name string, refs []colRef) {
	o.names = append(o.names, name)
	key := strings.ToLower(name)
	o.byName[key] = append(o.byName[key], refs...)
}

type sourceKind int

const (
	srcPhysical sourceKind = iota
	srcDerived
)

// source is one entry in a scope's FROM/JOIN list.
type source struct {
	kind  sourceKind
	table string       // root physical name (srcPhysical)
	out   *resolvedOut // resolved body output (srcDerived: CTE or subquery)
	roots []string     // root physical tables this source draws from
}

// cteEntry holds a CTE definition awaiting (memoized) resolution.
type cteEntry struct {
	node      *queryNode
	colAlias  []string  // explicit column aliases: c(x, y)
	parent    *scopeCtx // scope the CTE body resolves under
	memo      *resolvedOut
	resolving bool // cycle guard for recursive CTEs
}

// scopeCtx is the lexical scope of one query level.
type scopeCtx struct {
	sources  []*source
	byAlias  map[string]*source // lower(alias) -> source
	ctes     map[string]*cteEntry
	parent   *scopeCtx // enclosing scope, for correlated references
	deferred []expr    // FROM-position wrappers (PIVOT/UNNEST/table fn) collected after sources are built
}

type refResolver struct {
	metadata map[string][]string
	result   map[string]map[string]struct{}
}

func (rr *refResolver) add(table, col string) {
	if table == "" || col == "" {
		return
	}
	ensureSet(rr.result, table)[col] = struct{}{}
}

func (rr *refResolver) addRefs(refs []colRef) {
	for _, r := range refs {
		rr.add(r.table, r.col)
	}
}

// ----------------------------------------------------------------------------
// Scope resolution
// ----------------------------------------------------------------------------

// resolveQuery resolves a SELECT or set operation and returns its output mapping.
func (rr *refResolver) resolveQuery(q *queryNode, parent *scopeCtx) *resolvedOut {
	if q == nil {
		return emptyOut()
	}
	if q.Select != nil {
		return rr.resolveSelect(q.Select, parent)
	}
	for _, so := range []*setOpBody{q.Union, q.Intersect, q.Except} {
		if so != nil {
			return rr.resolveSetOp(so, parent)
		}
	}
	return emptyOut()
}

// newScope builds a scope whose visible CTEs come from with plus the parent.
func (rr *refResolver) newScope(with *withClause, parent *scopeCtx) *scopeCtx {
	ctes := map[string]*cteEntry{}
	if parent != nil {
		for k, v := range parent.ctes {
			ctes[k] = v
		}
	}
	if with != nil {
		for _, c := range with.CTEs {
			name := strings.ToLower(c.Alias.text())
			if name == "" {
				continue
			}
			ctes[name] = &cteEntry{node: c.This, colAlias: identNames(c.Columns)}
		}
	}
	ctx := &scopeCtx{byAlias: map[string]*source{}, ctes: ctes, parent: parent}
	// CTE bodies resolve under this scope (so they see sibling/outer CTEs).
	for _, e := range ctes {
		if e.parent == nil {
			e.parent = ctx
		}
	}
	return ctx
}

// resolveSelect resolves a single SELECT scope.
func (rr *refResolver) resolveSelect(sel *selectBody, parent *scopeCtx) *resolvedOut {
	ctx := rr.newScope(sel.With, parent)
	rr.buildSources(sel, ctx)

	// FROM-position wrappers (PIVOT/UNNEST/table functions) collected once all
	// sources — and thus all aliases — are known.
	for _, d := range ctx.deferred {
		rr.collect(d, ctx)
	}

	// Projection: build the output mapping and record projection refs.
	out := rr.buildOut(sel.Expressions, ctx)

	// Filter and grouping positions: record refs only.
	for _, e := range []expr{
		sel.Where, sel.GroupBy, sel.Having, sel.Qualify, sel.Windows,
		sel.OrderBy, sel.SortBy, sel.DistributeBy, sel.ClusterBy, sel.Connect, sel.LateralViews,
	} {
		rr.collect(e, ctx)
	}
	for _, j := range sel.Joins {
		rr.collect(j.On, ctx)
		// USING (c1, c2) names columns shared by both joined tables; resolve each
		// as an unqualified column.
		for _, u := range j.Using {
			rr.addRefs(rr.resolveUnqualified(u.text(), ctx))
		}
	}
	return out
}

// resolveSetOp resolves a UNION/INTERSECT/EXCEPT and merges its branches'
// outputs positionally (the result takes the left branch's column names).
func (rr *refResolver) resolveSetOp(body *setOpBody, parent *scopeCtx) *resolvedOut {
	ctx := rr.newScope(body.With, parent)
	left := rr.resolveQuery(body.Left, ctx)
	right := rr.resolveQuery(body.Right, ctx)

	out := emptyOut()
	out.names = append(out.names, left.names...)
	for i, name := range left.names {
		key := strings.ToLower(name)
		refs := append([]colRef{}, left.byName[key]...)
		if i < len(right.names) {
			refs = append(refs, right.byName[strings.ToLower(right.names[i])]...)
		}
		out.byName[key] = refs
	}
	return out
}

// resolveCTE resolves (and memoizes) a CTE body, applying its column aliases.
func (rr *refResolver) resolveCTE(e *cteEntry) *resolvedOut {
	if e.memo != nil {
		return e.memo
	}
	if e.resolving || e.node == nil {
		return emptyOut()
	}
	e.resolving = true
	out := applyColumnAliases(rr.resolveQuery(e.node, e.parent), e.colAlias)
	e.resolving = false
	e.memo = out
	return out
}

// ----------------------------------------------------------------------------
// Sources (FROM / JOIN)
// ----------------------------------------------------------------------------

// buildSources fills ctx.sources / ctx.byAlias from a SELECT's FROM and JOINs.
func (rr *refResolver) buildSources(sel *selectBody, ctx *scopeCtx) {
	if sel.From != nil {
		for _, e := range sel.From.Expressions {
			rr.addSource(e, ctx)
		}
	}
	for _, j := range sel.Joins {
		rr.addSource(j.This, ctx)
	}
}

// addSource registers one FROM/JOIN entry (raw JSON) as a scope source.
func (rr *refResolver) addSource(e expr, ctx *scopeCtx) {
	var fe fromEntry
	if decodeInto(e, &fe) != nil {
		return
	}
	switch {
	case fe.Pivot != nil:
		rr.addPivot(fe.Pivot, ctx)
	case fe.Unpivot != nil:
		rr.addPivot(fe.Unpivot, ctx)
	case fe.Subquery != nil:
		rr.addSubquery(fe.Subquery, ctx)
	case fe.Table != nil:
		rr.addTable(fe.Table, ctx)
	default:
		// A bare table object (a DML USING/FROM entry is not wrapped in "table")
		// has a top-level identifier "name" — distinct from a function node whose
		// "name" is a string. Register it; otherwise it is an unrecognized
		// wrapper (UNNEST, table function, …) whose columns we defer-collect.
		if tn := bareTable(e); tn != nil {
			rr.addTable(tn, ctx)
			return
		}
		if len(e) > 0 {
			ctx.deferred = append(ctx.deferred, e)
		}
	}
}

// bareTable decodes an unwrapped table object (one whose top-level "name" is an
// identifier node), or returns nil if e is not such a node.
func bareTable(e expr) *tableNode {
	obj, ok := decodeObj(e)
	if !ok {
		return nil
	}
	if _, ok := obj["name"]; !ok {
		return nil
	}
	var tn tableNode
	if decodeInto(e, &tn) != nil || tn.Name.text() == "" {
		return nil
	}
	return &tn
}

// addPivot registers a PIVOT/UNPIVOT's wrapped source and defers its own column
// references (aggregate args and FOR … IN fields).
func (rr *refResolver) addPivot(pv *pivotNode, ctx *scopeCtx) {
	rr.addSource(pv.This, ctx)
	ctx.deferred = append(ctx.deferred, pv.Expressions...)
	ctx.deferred = append(ctx.deferred, pv.Fields...)
}

func (rr *refResolver) addSubquery(sub *subqueryNode, ctx *scopeCtx) {
	out := applyColumnAliases(rr.resolveQuery(sub.This, ctx), identNames(sub.ColumnAliases))
	src := &source{kind: srcDerived, out: out, roots: rootsOfOut(out)}
	ctx.sources = append(ctx.sources, src)
	if alias := strings.ToLower(sub.Alias.text()); alias != "" {
		ctx.byAlias[alias] = src
	}
}

func (rr *refResolver) addTable(tbl *tableNode, ctx *scopeCtx) {
	name := tbl.Name.text()
	if name == "" {
		return
	}
	lname := strings.ToLower(name)
	aliasKey := lname
	if a := tbl.Alias.text(); a != "" {
		aliasKey = strings.ToLower(a)
	}

	// A bare name matching a visible CTE is a CTE reference, not a physical table.
	if cte, ok := ctx.ctes[lname]; ok && tbl.Schema.text() == "" && tbl.Catalog.text() == "" {
		out := rr.resolveCTE(cte)
		src := &source{kind: srcDerived, out: out, roots: rootsOfOut(out)}
		ctx.sources = append(ctx.sources, src)
		ctx.byAlias[aliasKey] = src
		return
	}

	root := qualifiedName(tbl)
	ensureSet(rr.result, root) // seed: table appears even with no column referenced
	src := &source{kind: srcPhysical, table: root, roots: []string{root}}
	ctx.sources = append(ctx.sources, src)
	ctx.byAlias[aliasKey] = src
}

// rootsOfOut returns the distinct root physical tables a resolved output draws
// from, in first-seen order.
func rootsOfOut(out *resolvedOut) []string {
	if out == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var roots []string
	for _, name := range out.names {
		for _, r := range out.byName[strings.ToLower(name)] {
			if r.table == "" {
				continue
			}
			if _, ok := seen[r.table]; !ok {
				seen[r.table] = struct{}{}
				roots = append(roots, r.table)
			}
		}
	}
	return roots
}

// ----------------------------------------------------------------------------
// Projection output mapping
// ----------------------------------------------------------------------------

type namedRefs struct {
	name string
	refs []colRef
}

// buildOut maps a SELECT's projection list to output names and their refs,
// recording every referenced column into the result along the way.
func (rr *refResolver) buildOut(expressions []expr, ctx *scopeCtx) *resolvedOut {
	out := emptyOut()
	for _, item := range expressions {
		obj, ok := decodeObj(item)
		if !ok {
			continue
		}
		switch {
		case has(obj, "star"):
			var sn starNode
			_ = decodeInto(obj["star"], &sn)
			for _, nr := range rr.expandStar(&sn, ctx) {
				out.add(nr.name, nr.refs)
			}
		case has(obj, "alias"):
			var al aliasNode
			_ = decodeInto(obj["alias"], &al)
			out.add(al.Alias.text(), rr.collectRefs(al.This, ctx))
		case has(obj, "column"):
			var cn columnNode
			_ = decodeInto(obj["column"], &cn)
			out.add(cn.Name.text(), rr.collectRefs(item, ctx))
		case has(obj, "dot"):
			col, _ := dotColumn(item)
			out.add(col, rr.collectRefs(item, ctx))
		default:
			// Unaliased expression: still record its referenced columns; its
			// output name is engine-defined (_colN) and not tracked here.
			rr.collectRefs(item, ctx)
		}
	}
	return out
}

// expandStar expands SELECT * / t.* against the scope's sources.
func (rr *refResolver) expandStar(star *starNode, ctx *scopeCtx) []namedRefs {
	qualifier := strings.ToLower(star.Table.text())
	var out []namedRefs
	emit := func(s *source) {
		switch s.kind {
		case srcDerived:
			for _, name := range s.out.names {
				refs := s.out.byName[strings.ToLower(name)]
				rr.addRefs(refs)
				out = append(out, namedRefs{name: name, refs: refs})
			}
		case srcPhysical:
			if cols, ok := lookupMetadataColumns(rr.metadata, s.table); ok {
				for _, c := range cols {
					rr.add(s.table, c)
					out = append(out, namedRefs{name: c, refs: []colRef{{s.table, c}}})
				}
			} else {
				rr.add(s.table, "*")
				out = append(out, namedRefs{name: "*", refs: []colRef{{s.table, "*"}}})
			}
		}
	}
	if qualifier != "" {
		if s := ctx.byAlias[qualifier]; s != nil {
			emit(s)
		}
		return out
	}
	for _, s := range ctx.sources {
		emit(s)
	}
	return out
}

// ----------------------------------------------------------------------------
// Generic expression walking
// ----------------------------------------------------------------------------

// collect walks an arbitrary clause subtree recording every column reference. A
// nested query (subquery in IN/EXISTS/ANY/scalar, …) is resolved as its own
// scope; a bare "*" (e.g. count(*)) is ignored — projection-position stars are
// expanded by buildOut instead.
func (rr *refResolver) collect(e expr, ctx *scopeCtx) {
	if obj, ok := decodeObj(e); ok {
		switch {
		case isQueryObj(obj):
			rr.resolveQuery(mustQuery(e), ctx)
		case has(obj, "column"):
			rr.recordColumn(obj["column"], ctx)
		case has(obj, "dot"):
			rr.recordDot(e, ctx)
		default:
			for _, v := range obj {
				rr.collect(v, ctx)
			}
		}
		return
	}
	for _, v := range decodeArr(e) {
		rr.collect(v, ctx)
	}
}

// collectRefs is collect that also returns the resolved refs, used to build a
// projection's output mapping.
func (rr *refResolver) collectRefs(e expr, ctx *scopeCtx) []colRef {
	var acc []colRef
	var walk func(expr)
	walk = func(n expr) {
		if obj, ok := decodeObj(n); ok {
			switch {
			case isQueryObj(obj):
				rr.resolveQuery(mustQuery(n), ctx)
			case has(obj, "column"):
				acc = append(acc, rr.recordColumn(obj["column"], ctx)...)
			case has(obj, "dot"):
				acc = append(acc, rr.recordDot(n, ctx)...)
			default:
				for _, v := range obj {
					walk(v)
				}
			}
			return
		}
		for _, v := range decodeArr(n) {
			walk(v)
		}
	}
	walk(e)
	return acc
}

// recordColumn resolves and records a two-part-or-less column reference (the raw
// is the body under the "column" key).
func (rr *refResolver) recordColumn(colBody expr, ctx *scopeCtx) []colRef {
	var cn columnNode
	if decodeInto(colBody, &cn) != nil || cn.Name.text() == "" {
		return nil
	}
	refs := rr.resolve(cn.Table.text(), cn.Name.text(), ctx)
	rr.addRefs(refs)
	return refs
}

// recordDot resolves and records a dotted reference (schema.table.col, …). node
// is the whole {"dot": …} wrapper.
func (rr *refResolver) recordDot(node expr, ctx *scopeCtx) []colRef {
	col, qualifier := dotColumn(node)
	if col == "" {
		return nil
	}
	refs := rr.resolve(qualifier, col, ctx)
	rr.addRefs(refs)
	return refs
}

// ----------------------------------------------------------------------------
// Column attribution
// ----------------------------------------------------------------------------

// resolve attributes a column reference to its root physical table(s).
func (rr *refResolver) resolve(qualifier, name string, ctx *scopeCtx) []colRef {
	if qualifier != "" {
		return rr.resolveQualified(strings.ToLower(qualifier), name, ctx)
	}
	return rr.resolveUnqualified(name, ctx)
}

func (rr *refResolver) resolveQualified(qualifier, name string, ctx *scopeCtx) []colRef {
	for c := ctx; c != nil; c = c.parent {
		if s := c.byAlias[qualifier]; s != nil {
			switch s.kind {
			case srcPhysical:
				return []colRef{{s.table, name}}
			case srcDerived:
				if refs, ok := s.out.byName[strings.ToLower(name)]; ok {
					return refs
				}
				// Name not in the derived output (e.g. an unexpanded SELECT *):
				// attribute to the derived source's roots rather than dropping it.
				return refsForRoots(s.roots, name)
			}
		}
	}
	// Unknown qualifier (alias we could not resolve): never drop — attribute to
	// every physical source in scope and its parents (safe superset).
	if roots := chainRoots(ctx); len(roots) > 0 {
		return refsForRoots(roots, name)
	}
	// No sources at all: surface the column under its written qualifier so it is
	// not lost (last resort).
	return []colRef{{qualifier, name}}
}

func (rr *refResolver) resolveUnqualified(name string, ctx *scopeCtx) []colRef {
	if name == "" {
		return nil
	}
	var refs []colRef
	matched := false

	// Derived sources that expose the name resolve through to their roots.
	for _, s := range ctx.sources {
		if s.kind == srcDerived {
			if r, ok := s.out.byName[strings.ToLower(name)]; ok {
				refs = append(refs, r...)
				matched = true
			}
		}
	}

	phys := physicalSources(ctx)
	// Single unambiguous physical source: attribute directly.
	if !matched && len(phys) == 1 && len(ctx.sources) == 1 {
		return []colRef{{phys[0].table, name}}
	}

	for _, s := range phys {
		if cols, ok := lookupMetadataColumns(rr.metadata, s.table); ok && containsString(cols, name) {
			refs = append(refs, colRef{s.table, name})
			matched = true
		}
	}
	if matched {
		return refs
	}
	// Unresolved (no metadata, or metadata silent on this column): attribute to
	// every source root in scope rather than dropping it (safe superset).
	return refsForRoots(scopeRoots(ctx), name)
}

func physicalSources(ctx *scopeCtx) []*source {
	var out []*source
	for _, s := range ctx.sources {
		if s.kind == srcPhysical {
			out = append(out, s)
		}
	}
	return out
}

// refsForRoots pairs each root table with the column name.
func refsForRoots(roots []string, name string) []colRef {
	refs := make([]colRef, 0, len(roots))
	for _, r := range roots {
		refs = append(refs, colRef{r, name})
	}
	return refs
}

// scopeRoots returns the distinct root tables of every source in ctx.
func scopeRoots(ctx *scopeCtx) []string {
	seen := map[string]struct{}{}
	var roots []string
	for _, s := range ctx.sources {
		for _, r := range s.roots {
			if _, ok := seen[r]; !ok {
				seen[r] = struct{}{}
				roots = append(roots, r)
			}
		}
	}
	return roots
}

// chainRoots returns the distinct root tables across ctx and all enclosing scopes.
func chainRoots(ctx *scopeCtx) []string {
	seen := map[string]struct{}{}
	var roots []string
	for c := ctx; c != nil; c = c.parent {
		for _, r := range scopeRoots(c) {
			if _, ok := seen[r]; !ok {
				seen[r] = struct{}{}
				roots = append(roots, r)
			}
		}
	}
	return roots
}

// applyColumnAliases renames a resolved output positionally using explicit
// column aliases (e.g. a CTE or subquery "AS c(x, y)").
func applyColumnAliases(out *resolvedOut, aliases []string) *resolvedOut {
	if out == nil {
		return emptyOut()
	}
	if len(aliases) == 0 {
		return out
	}
	renamed := emptyOut()
	for i, a := range aliases {
		var refs []colRef
		if i < len(out.names) {
			refs = out.byName[strings.ToLower(out.names[i])]
		}
		renamed.add(a, refs)
	}
	return renamed
}

// ----------------------------------------------------------------------------
// DML (DELETE / UPDATE / MERGE)
// ----------------------------------------------------------------------------

// resolveDelete handles DELETE [USING …] WHERE …, capturing the columns its
// predicate (and any USING/JOIN tables) reference.
func (rr *refResolver) resolveDelete(del *deleteNode) {
	ctx := rr.newScope(del.With, nil)
	rr.addTable(del.Table, ctx)
	for _, j := range del.Joins {
		rr.addSource(j.This, ctx)
	}
	for _, u := range del.Using {
		rr.addSource(u, ctx)
	}
	for _, d := range ctx.deferred {
		rr.collect(d, ctx)
	}
	rr.collect(del.Where, ctx)
	for _, j := range del.Joins {
		rr.collect(j.On, ctx)
		for _, u := range j.Using {
			rr.addRefs(rr.resolveUnqualified(u.text(), ctx))
		}
	}
	rr.collect(del.OrderBy, ctx)
}

// resolveUpdate handles UPDATE … SET … [FROM …] WHERE …, capturing both the SET
// value-expression reads (and the assigned target columns) and the predicate.
func (rr *refResolver) resolveUpdate(upd *updateNode) {
	ctx := rr.newScope(upd.With, nil)
	rr.addTable(upd.Table, ctx)
	targetRoot := qualifiedName(upd.Table)

	if upd.FromClause != nil {
		for _, e := range upd.FromClause.Expressions {
			rr.addSource(e, ctx)
		}
	}
	for _, j := range upd.FromJoins {
		rr.addSource(j.This, ctx)
	}
	for _, d := range ctx.deferred {
		rr.collect(d, ctx)
	}

	// SET pairs: [targetColumnIdentifier, valueExpression].
	for _, pair := range upd.Set {
		if len(pair) >= 1 {
			var id ident
			if decodeInto(pair[0], &id) == nil && id.Name != "" {
				rr.add(targetRoot, id.Name)
			}
		}
		for i := 1; i < len(pair); i++ {
			rr.collect(pair[i], ctx)
		}
	}

	rr.collect(upd.Where, ctx)
	for _, j := range upd.FromJoins {
		rr.collect(j.On, ctx)
	}
	rr.collect(upd.OrderBy, ctx)
}

// resolveMerge handles MERGE INTO … USING … ON … WHEN …, capturing the ON
// condition and every column referenced in the WHEN clauses.
func (rr *refResolver) resolveMerge(mrg *mergeNode) {
	ctx := rr.newScope(mrg.With, nil)
	rr.addSource(mrg.This, ctx)
	rr.addSource(mrg.Using, ctx)
	for _, d := range ctx.deferred {
		rr.collect(d, ctx)
	}
	rr.collect(mrg.On, ctx)
	rr.collect(mrg.Whens, ctx)
}

// ----------------------------------------------------------------------------
// Decoding helpers
// ----------------------------------------------------------------------------

// decodeNode re-encodes a decoded-JSON map and decodes it into a typed value.
func decodeNode(node map[string]any, v any) error {
	if err := sonic.Unmarshal(mustMarshal(node), v); err != nil {
		return fmt.Errorf("%w: %v", ErrInternal, err)
	}
	return nil
}

// decodeQueryMap decodes a query node obtained from innerQuery (a map) into a
// typed queryNode.
func decodeQueryMap(node map[string]any) (*queryNode, error) {
	var q queryNode
	if err := decodeNode(node, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// decodeInto unmarshals raw JSON into v, ignoring errors for absent/null nodes.
func decodeInto(e expr, v any) error {
	if len(e) == 0 {
		return errors.New("empty node")
	}
	return sonic.Unmarshal(e, v)
}

// mustQuery decodes a raw query node ({"select"|"union"|…: …}); a malformed node
// yields an empty queryNode rather than an error (the walker is best-effort).
func mustQuery(e expr) *queryNode {
	var q queryNode
	_ = decodeInto(e, &q)
	return &q
}

// decodeObj decodes a JSON object node into its key->raw children. ok is false
// for arrays, scalars, null and malformed input.
func decodeObj(e expr) (map[string]expr, bool) {
	if firstByte(e) != '{' {
		return nil, false
	}
	var m map[string]expr
	if sonic.Unmarshal(e, &m) != nil {
		return nil, false
	}
	return m, true
}

// decodeArr decodes a JSON array node into its raw elements (nil otherwise).
func decodeArr(e expr) []expr {
	if firstByte(e) != '[' {
		return nil
	}
	var a []expr
	if sonic.Unmarshal(e, &a) != nil {
		return nil
	}
	return a
}

func firstByte(e expr) byte {
	for _, b := range e {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b
		}
	}
	return 0
}

func has(obj map[string]expr, key string) bool {
	v, ok := obj[key]
	return ok && firstByte(v) != 0 && firstByte(v) != 'n' // present and not JSON null
}

func isQueryObj(obj map[string]expr) bool {
	return has(obj, "select") || has(obj, "union") || has(obj, "intersect") || has(obj, "except")
}

// dotColumn extracts (column, immediate-table-qualifier) from a dot chain such
// as catalog.schema.table.column. The outermost field is the column; the segment
// directly before it is the table qualifier. node is the {"dot": …} wrapper.
func dotColumn(node expr) (col, qualifier string) {
	segs := dotSegments(node)
	if len(segs) == 0 {
		return "", ""
	}
	col = segs[len(segs)-1]
	if len(segs) >= 2 {
		qualifier = segs[len(segs)-2]
	}
	return col, qualifier
}

// dotSegments flattens a dot/column node into its identifier path.
func dotSegments(e expr) []string {
	obj, ok := decodeObj(e)
	if !ok {
		return nil
	}
	if d, ok := obj["dot"]; ok {
		var dn dotNode
		if decodeInto(d, &dn) != nil {
			return nil
		}
		return append(dotSegments(dn.This), dn.Field.text())
	}
	if c, ok := obj["column"]; ok {
		var cn columnNode
		if decodeInto(c, &cn) != nil {
			return nil
		}
		var segs []string
		if t := cn.Table.text(); t != "" {
			segs = append(segs, t)
		}
		return append(segs, cn.Name.text())
	}
	return nil
}

// qualifiedName joins a table node's catalog/schema/name into a dotted name.
func qualifiedName(tbl *tableNode) string {
	if tbl == nil {
		return ""
	}
	var parts []string
	if c := tbl.Catalog.text(); c != "" {
		parts = append(parts, c)
	}
	if s := tbl.Schema.text(); s != "" {
		parts = append(parts, s)
	}
	if n := tbl.Name.text(); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, ".")
}

// identNames extracts the names from a list of identifier nodes.
func identNames(ids []*ident) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := id.text(); n != "" {
			out = append(out, n)
		}
	}
	return out
}
