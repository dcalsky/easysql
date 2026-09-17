// This file implements ReferencedColumns: given a SQL statement it returns,
// for every root physical table, the columns referenced *anywhere* in the
// statement — projection, WHERE, JOIN ... ON, GROUP BY, HAVING, ORDER BY,
// QUALIFY and window clauses alike. It is the complement of LineageSourceColumns
// (which reports only columns that flow into the result) and is a superset of
// it: a column appearing only in a filter position is included here but not
// there.
//
// Polyglot v0.10+ exposes non-projection columnUses, but those facts omit
// inner projection references that do not reach the final output and do not
// implement this API's DML and fail-open contracts. Resolution therefore still
// uses the parsed AST. A recursive resolver walks each query scope, maps every
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
	"sort"
	"strconv"
	"strings"

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
	Recursive bool       `json:"recursive"`
	CTEs      []*cteNode `json:"ctes"`
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
	Left    *queryNode  `json:"left"`
	Right   *queryNode  `json:"right"`
	With    *withClause `json:"with"`
	OrderBy expr        `json:"order_by"`
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

// ColumnClause identifies the SQL clause that contains a column reference.
// References inside nested queries are classified against the nested query's
// own clause. For example, a column in a scalar subquery's SELECT list is
// ColumnClauseSelect even when that subquery appears inside an outer WHERE.
type ColumnClause string

const (
	ColumnClauseSelect          ColumnClause = "SELECT"
	ColumnClauseFrom            ColumnClause = "FROM"
	ColumnClauseJoinOn          ColumnClause = "JOIN_ON"
	ColumnClauseJoinUsing       ColumnClause = "JOIN_USING"
	ColumnClauseWhere           ColumnClause = "WHERE"
	ColumnClauseGroupBy         ColumnClause = "GROUP_BY"
	ColumnClauseHaving          ColumnClause = "HAVING"
	ColumnClauseQualify         ColumnClause = "QUALIFY"
	ColumnClauseWindow          ColumnClause = "WINDOW"
	ColumnClauseOrderBy         ColumnClause = "ORDER_BY"
	ColumnClauseSortBy          ColumnClause = "SORT_BY"
	ColumnClauseDistributeBy    ColumnClause = "DISTRIBUTE_BY"
	ColumnClauseClusterBy       ColumnClause = "CLUSTER_BY"
	ColumnClauseConnectBy       ColumnClause = "CONNECT_BY"
	ColumnClauseLateralView     ColumnClause = "LATERAL_VIEW"
	ColumnClauseUpdateSetTarget ColumnClause = "UPDATE_SET_TARGET"
	ColumnClauseUpdateSetValue  ColumnClause = "UPDATE_SET_VALUE"
	ColumnClauseMergeOn         ColumnClause = "MERGE_ON"
	ColumnClauseMergeWhen       ColumnClause = "MERGE_WHEN"
)

// ColumnUse is one distinct use of a root physical-table column in a SQL
// clause. Table is fully qualified when the input SQL is fully qualified.
// Repeated references to the same table, column, and clause are deduplicated.
type ColumnUse struct {
	Table  string
	Column string
	Clause ColumnClause
}

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
	rr, err := analyzeReferencedColumns(client, sql, false, opts...)
	if err != nil {
		return nil, err
	}
	return sortedResult(rr.result), nil
}

// ReferencedColumnUsages returns the distinct root physical-table columns used
// by sql together with the clause containing each use. Results are sorted by
// table, column, then clause. It accepts the same statements, options, and
// fail-open resolution rules as ReferencedColumns. Projection aliases and
// positive ordinals in output-aware clauses resolve back to their source
// columns.
//
// A table with no column reference has no ColumnUse entry; use
// ReferencedColumns when callers also need empty table entries.
func ReferencedColumnUsages(sql string, opts ...LineageOption) ([]ColumnUse, error) {
	client, err := defaultClient()
	if err != nil {
		return nil, err
	}
	return referencedColumnUsages(client, sql, opts...)
}

func referencedColumnUsages(client *polyglot.Client, sql string, opts ...LineageOption) ([]ColumnUse, error) {
	rr, err := analyzeReferencedColumns(client, sql, true, opts...)
	if err != nil {
		return nil, err
	}
	return sortedColumnUses(rr.usages), nil
}

// analyzeReferencedColumns is the shared implementation behind the legacy
// table->columns view and the richer clause-aware view.
func analyzeReferencedColumns(
	client *polyglot.Client,
	sql string,
	includeUsages bool,
	opts ...LineageOption,
) (*refResolver, error) {
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

	rr := &refResolver{metadata: cfg.metadata, result: map[string]map[string]struct{}{}}
	if includeUsages {
		rr.usages = map[ColumnUse]struct{}{}
	}

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
		return rr, nil
	case st.Update != nil:
		rr.resolveUpdate(st.Update)
		return rr, nil
	case st.Merge != nil:
		rr.resolveMerge(st.Merge)
		return rr, nil
	}

	// CREATE VIEW / CTAS / INSERT ... SELECT etc. are unwrapped to their inner
	// query by the shared innerQuery helper before structural decoding.
	inner := innerQuery(stmt)
	if inner == nil {
		return rr, nil
	}
	q, err := decodeQueryMap(inner)
	if err != nil {
		return nil, err
	}
	rr.resolveQuery(q, nil)
	return rr, nil
}

func sortedColumnUses(usages map[ColumnUse]struct{}) []ColumnUse {
	out := make([]ColumnUse, 0, len(usages))
	for use := range usages {
		out = append(out, use)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		if out[i].Column != out[j].Column {
			return out[i].Column < out[j].Column
		}
		return out[i].Clause < out[j].Clause
	})
	return out
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
	names     []string
	byName    map[string][]colRef
	positions [][]colRef
}

func emptyOut() *resolvedOut { return &resolvedOut{byName: map[string][]colRef{}} }

func (o *resolvedOut) add(name string, refs []colRef) {
	o.names = append(o.names, name)
	key := strings.ToLower(name)
	o.byName[key] = append(o.byName[key], refs...)
	o.positions = append(o.positions, refs)
}

func (o *resolvedOut) addPosition(refs []colRef) {
	o.positions = append(o.positions, refs)
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
	usages   map[ColumnUse]struct{}

	// flowOnly switches the resolver from "columns touched anywhere"
	// (ReferencedColumns) to "columns whose values flow into the result"
	// (LineageSourceColumns): filter positions are skipped and column refs are
	// NOT recorded eagerly — instead the caller records only the refs of the
	// query's final output names (so an intermediate CTE/derived column that no
	// downstream projection selects is correctly dropped).
	flowOnly bool
}

func (rr *refResolver) add(table, col string, clause ColumnClause) {
	if rr.flowOnly {
		// In flow mode nothing is recorded eagerly; the caller records the
		// final output refs. Column refs still propagate through the returned
		// resolvedOut mappings.
		return
	}
	if table == "" || col == "" {
		return
	}
	ensureSet(rr.result, table)[col] = struct{}{}
	if clause != "" && rr.usages != nil {
		rr.usages[ColumnUse{Table: table, Column: col, Clause: clause}] = struct{}{}
	}
}

func (rr *refResolver) addRefs(refs []colRef, clause ColumnClause) {
	for _, r := range refs {
		rr.add(r.table, r.col, clause)
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
	if q.Union != nil {
		return rr.resolveSetOp(q.Union, parent, true)
	}
	if q.Intersect != nil {
		return rr.resolveSetOp(q.Intersect, parent, false)
	}
	if q.Except != nil {
		return rr.resolveSetOp(q.Except, parent, false)
	}
	return emptyOut()
}

// newScope builds a scope whose visible CTEs come from with plus the parent.
//
// CTE visibility follows SQL scoping. Every CTE declared by `with` is visible
// to the query body that follows (the returned scope's ctes). But each CTE
// *definition body* sees a restricted set: the enclosing scope's CTEs plus,
//   - for a non-recursive WITH, only the EARLIER siblings (a CTE cannot see
//     itself or a later sibling, so a same-named FROM entry inside it is the
//     physical table of that name), or
//   - for a WITH RECURSIVE, all siblings of this WITH (self- and mutual
//     references denote the CTE).
func (rr *refResolver) newScope(with *withClause, parent *scopeCtx) *scopeCtx {
	base := map[string]*cteEntry{}
	if parent != nil {
		for k, v := range parent.ctes {
			base[k] = v
		}
	}

	// all: what the query body following this WITH can see (every sibling).
	all := map[string]*cteEntry{}
	for k, v := range base {
		all[k] = v
	}

	if with != nil {
		// Materialize the sibling entries first so recursive bodies can see all
		// of them (including later siblings) before their scopes are assigned.
		type named struct {
			name string
			e    *cteEntry
		}
		var ordered []named
		for _, c := range with.CTEs {
			name := strings.ToLower(c.Alias.text())
			if name == "" {
				continue
			}
			e := &cteEntry{node: c.This, colAlias: identNames(c.Columns)}
			all[name] = e
			ordered = append(ordered, named{name, e})
		}

		// Assign each new sibling's definition-body scope with the correct
		// visibility. Entries inherited from the parent keep their own parent
		// scope (do not re-point them).
		visible := map[string]*cteEntry{}
		for k, v := range base {
			visible[k] = v
		}
		for _, ne := range ordered {
			bodyCtes := map[string]*cteEntry{}
			if with.Recursive {
				for k, v := range all {
					bodyCtes[k] = v
				}
			} else {
				for k, v := range visible {
					bodyCtes[k] = v
				}
			}
			ne.e.parent = &scopeCtx{byAlias: map[string]*source{}, ctes: bodyCtes, parent: parent}
			visible[ne.name] = ne.e
		}
	}

	return &scopeCtx{byAlias: map[string]*source{}, ctes: all, parent: parent}
}

// resolveSelect resolves a single SELECT scope.
func (rr *refResolver) resolveSelect(sel *selectBody, parent *scopeCtx) *resolvedOut {
	ctx := rr.newScope(sel.With, parent)
	rr.buildSources(sel, ctx)

	// FROM-position wrappers (PIVOT/UNNEST/table functions) collected once all
	// sources — and thus all aliases — are known.
	if !rr.flowOnly {
		for _, d := range ctx.deferred {
			rr.collect(d, ctx, ColumnClauseFrom)
		}
	}

	// Projection: build the output mapping and record projection refs.
	out := rr.buildOut(sel.Expressions, ctx)

	// Flow mode reports only columns that reach the result: filter, grouping,
	// join-ON and USING positions do not flow, so stop after the projection.
	if rr.flowOnly {
		return out
	}

	// Non-projection positions: record refs with their containing clause.
	for _, item := range []struct {
		expr        expr
		clause      ColumnClause
		outputAware bool
	}{
		{sel.Where, ColumnClauseWhere, false},
		{sel.GroupBy, ColumnClauseGroupBy, true},
		{sel.Having, ColumnClauseHaving, true},
		{sel.Qualify, ColumnClauseQualify, true},
		{sel.Windows, ColumnClauseWindow, false},
		{sel.OrderBy, ColumnClauseOrderBy, true},
		{sel.SortBy, ColumnClauseSortBy, true},
		{sel.DistributeBy, ColumnClauseDistributeBy, true},
		{sel.ClusterBy, ColumnClauseClusterBy, true},
		{sel.Connect, ColumnClauseConnectBy, false},
		{sel.LateralViews, ColumnClauseLateralView, false},
	} {
		if item.outputAware {
			rr.collectOutputClause(item.expr, ctx, item.clause, out)
		} else {
			rr.collect(item.expr, ctx, item.clause)
		}
	}
	for _, j := range sel.Joins {
		rr.collect(j.On, ctx, ColumnClauseJoinOn)
		// USING (c1, c2) names columns shared by both joined tables; resolve each
		// as an unqualified column.
		for _, u := range j.Using {
			rr.addRefs(rr.resolveUnqualified(u.text(), ctx), ColumnClauseJoinUsing)
		}
	}
	return out
}

// resolveSetOp resolves a UNION/INTERSECT/EXCEPT and merges its branches'
// outputs positionally (the result takes the left branch's column names).
// ReferencedColumns retains inputs from both branches, while lineage's
// flow-only fallback keeps only the left value branch of INTERSECT/EXCEPT.
func (rr *refResolver) resolveSetOp(body *setOpBody, parent *scopeCtx, rightValuesFlow bool) *resolvedOut {
	ctx := rr.newScope(body.With, parent)
	left := rr.resolveQuery(body.Left, ctx)
	right := rr.resolveQuery(body.Right, ctx)
	includeRight := !rr.flowOnly || rightValuesFlow

	out := emptyOut()
	out.names = append(out.names, left.names...)
	for i, name := range left.names {
		key := strings.ToLower(name)
		refs := append([]colRef{}, left.byName[key]...)
		if includeRight && i < len(right.names) {
			refs = append(refs, right.byName[strings.ToLower(right.names[i])]...)
		}
		out.byName[key] = refs
	}
	positionCount := len(left.positions)
	if includeRight {
		positionCount = max(positionCount, len(right.positions))
	}
	for i := 0; i < positionCount; i++ {
		var refs []colRef
		if i < len(left.positions) {
			refs = append(refs, left.positions[i]...)
		}
		if includeRight && i < len(right.positions) {
			refs = append(refs, right.positions[i]...)
		}
		out.addPosition(refs)
	}
	if !rr.flowOnly {
		rr.collectOutputClause(body.OrderBy, ctx, ColumnClauseOrderBy, out)
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
	for _, refs := range out.positions {
		for _, r := range refs {
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
			out.add(al.Alias.text(), rr.collectRefs(al.This, ctx, ColumnClauseSelect))
		case has(obj, "column"):
			var cn columnNode
			_ = decodeInto(obj["column"], &cn)
			out.add(cn.Name.text(), rr.collectRefs(item, ctx, ColumnClauseSelect))
		case has(obj, "dot"):
			col, _ := dotColumn(item)
			out.add(col, rr.collectRefs(item, ctx, ColumnClauseSelect))
		default:
			// Unaliased expression: still record its referenced columns; its
			// output name is engine-defined (_colN) and not tracked here. In
			// flow mode the refs must still propagate to the caller (e.g. a
			// scalar subquery whose single value is an unaliased aggregate), so
			// attach them to the output under an empty name.
			refs := rr.collectRefs(item, ctx, ColumnClauseSelect)
			if rr.flowOnly {
				out.add("", refs)
			} else {
				out.addPosition(refs)
			}
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
				rr.addRefs(refs, ColumnClauseSelect)
				out = append(out, namedRefs{name: name, refs: refs})
			}
		case srcPhysical:
			if cols, ok := lookupMetadataColumns(rr.metadata, s.table); ok {
				for _, c := range cols {
					rr.add(s.table, c, ColumnClauseSelect)
					out = append(out, namedRefs{name: c, refs: []colRef{{s.table, c}}})
				}
			} else {
				rr.add(s.table, "*", ColumnClauseSelect)
				out = append(out, namedRefs{name: "*", refs: []colRef{{s.table, "*"}}})
			}
		}
	}
	if qualifier != "" {
		// Resolve the qualifier against the current scope first, then enclosing
		// scopes (a correlated star such as `t.*` inside a subquery where `t` is
		// only visible in the outer query).
		for c := ctx; c != nil; c = c.parent {
			if s := c.byAlias[qualifier]; s != nil {
				emit(s)
				return out
			}
		}
		// Unknown qualifier: never drop. Fail open by broadcasting the "*"
		// sentinel onto every physical root in scope and enclosing scopes.
		roots := chainRoots(ctx)
		if len(roots) == 0 {
			roots = []string{qualifier}
		}
		for _, r := range roots {
			rr.add(r, "*", ColumnClauseSelect)
			out = append(out, namedRefs{name: "*", refs: []colRef{{r, "*"}}})
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
func (rr *refResolver) collect(e expr, ctx *scopeCtx, clause ColumnClause) {
	rr.collectWithOutput(e, ctx, clause, nil)
}

// collectOutputClause handles a clause whose unqualified names and positive
// integer ordinals may refer to SELECT outputs. The AST represents ORDER BY 1,
// for example, as a direct numeric literal under one ordered expression; numeric
// literals nested inside a larger expression remain ordinary literals.
func (rr *refResolver) collectOutputClause(
	e expr,
	ctx *scopeCtx,
	clause ColumnClause,
	out *resolvedOut,
) {
	obj, ok := decodeObj(e)
	if !ok {
		rr.collectWithOutput(e, ctx, clause, out)
		return
	}
	items := decodeArr(obj["expressions"])
	if items == nil {
		rr.collectWithOutput(e, ctx, clause, out)
		return
	}
	for _, item := range items {
		target := item
		if ordered, ok := decodeObj(item); ok && has(ordered, "this") {
			target = ordered["this"]
		}
		if ordinal, ok := outputOrdinal(target); ok {
			if ordinal <= len(out.positions) {
				rr.addRefs(out.positions[ordinal-1], clause)
			}
			continue
		}
		rr.collectWithOutput(item, ctx, clause, out)
	}
}

func outputOrdinal(e expr) (int, bool) {
	obj, ok := decodeObj(e)
	if !ok || !has(obj, "literal") {
		return 0, false
	}
	var lit struct {
		LiteralType string `json:"literal_type"`
		Value       string `json:"value"`
	}
	if decodeInto(obj["literal"], &lit) != nil || lit.LiteralType != "number" {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	return n, err == nil && n > 0
}

// collectWithOutput additionally resolves unqualified references to projection
// names visible in clauses such as GROUP BY, QUALIFY, and ORDER BY. This maps an
// alias back to the root columns that produce it instead of fabricating a source
// column with the alias's name.
func (rr *refResolver) collectWithOutput(e expr, ctx *scopeCtx, clause ColumnClause, out *resolvedOut) {
	if obj, ok := decodeObj(e); ok {
		switch {
		case isQueryObj(obj):
			rr.resolveQuery(mustQuery(e), ctx)
		case has(obj, "column"):
			rr.recordColumnWithOutput(obj["column"], ctx, clause, out)
		case has(obj, "dot"):
			rr.recordDot(e, ctx, clause)
		default:
			for _, v := range obj {
				rr.collectWithOutput(v, ctx, clause, out)
			}
		}
		return
	}
	for _, v := range decodeArr(e) {
		rr.collectWithOutput(v, ctx, clause, out)
	}
}

// collectRefs is collect that also returns the resolved refs, used to build a
// projection's output mapping.
func (rr *refResolver) collectRefs(e expr, ctx *scopeCtx, clause ColumnClause) []colRef {
	var acc []colRef
	var walk func(expr)
	walk = func(n expr) {
		if obj, ok := decodeObj(n); ok {
			switch {
			case isQueryObj(obj):
				sub := rr.resolveQuery(mustQuery(n), ctx)
				// A scalar subquery in an expression position contributes its
				// output value(s) to the enclosing projection. Keep those refs
				// for both flow analysis and output-alias resolution.
				for _, refs := range sub.positions {
					acc = append(acc, refs...)
				}
			case has(obj, "column"):
				acc = append(acc, rr.recordColumn(obj["column"], ctx, clause)...)
			case has(obj, "dot"):
				acc = append(acc, rr.recordDot(n, ctx, clause)...)
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
func (rr *refResolver) recordColumn(colBody expr, ctx *scopeCtx, clause ColumnClause) []colRef {
	return rr.recordColumnWithOutput(colBody, ctx, clause, nil)
}

func (rr *refResolver) recordColumnWithOutput(
	colBody expr,
	ctx *scopeCtx,
	clause ColumnClause,
	out *resolvedOut,
) []colRef {
	var cn columnNode
	if decodeInto(colBody, &cn) != nil || cn.Name.text() == "" {
		return nil
	}
	if cn.Table.text() == "" && out != nil {
		if refs, ok := out.byName[strings.ToLower(cn.Name.text())]; ok {
			rr.addRefs(refs, clause)
			return refs
		}
	}
	refs := rr.resolve(cn.Table.text(), cn.Name.text(), ctx)
	rr.addRefs(refs, clause)
	return refs
}

// recordDot resolves and records a dotted reference (schema.table.col, …). node
// is the whole {"dot": …} wrapper.
func (rr *refResolver) recordDot(node expr, ctx *scopeCtx, clause ColumnClause) []colRef {
	col, qualifier := dotColumn(node)
	if col == "" {
		return nil
	}
	refs := rr.resolve(qualifier, col, ctx)
	rr.addRefs(refs, clause)
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
		if refs := resolveQualifiedPhysical(qualifier, name, c); len(refs) > 0 {
			return refs
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

func resolveQualifiedPhysical(qualifier, name string, ctx *scopeCtx) []colRef {
	var refs []colRef
	seen := map[string]struct{}{}
	for _, s := range physicalSources(ctx) {
		if !tableRefMatches(qualifier, s.table) {
			continue
		}
		if _, ok := seen[s.table]; ok {
			continue
		}
		seen[s.table] = struct{}{}
		refs = append(refs, colRef{s.table, name})
	}
	return refs
}

func (rr *refResolver) resolveUnqualified(name string, ctx *scopeCtx) []colRef {
	if name == "" || ctx == nil {
		return nil
	}
	var refs []colRef
	var unknownRoots []string

	// Derived sources that expose the name resolve through to their roots.
	for _, s := range ctx.sources {
		if s.kind == srcDerived {
			if r, ok := s.out.byName[strings.ToLower(name)]; ok {
				refs = append(refs, r...)
				continue
			}
			// An unexpanded star means the derived source may expose this name;
			// retain its roots for the same fail-open treatment as a physical
			// source whose metadata is unknown.
			if _, unknown := s.out.byName["*"]; unknown {
				unknownRoots = append(unknownRoots, s.roots...)
			}
		}
	}

	phys := physicalSources(ctx)
	for _, s := range phys {
		cols, known := lookupMetadataColumns(rr.metadata, s.table)
		if !known {
			unknownRoots = append(unknownRoots, s.table)
			continue
		}
		if containsString(cols, name) {
			refs = append(refs, colRef{s.table, name})
		}
	}
	// Unknown current-scope schemas can legally contain the name and therefore
	// participate alongside known matches. Attribute to them conservatively.
	refs = append(refs, refsForRoots(distinctStrings(unknownRoots), name)...)
	if len(refs) > 0 {
		return refs
	}

	// Every current source had metadata (or a fully known derived output) and
	// none exposed the name. SQL correlation then resolves against enclosing
	// scopes; consult them before applying the final fail-open fallback.
	if ctx.parent != nil {
		if parentRefs := rr.resolveUnqualified(name, ctx.parent); len(parentRefs) > 0 {
			return parentRefs
		}
	}
	// The name is unresolved even across the correlation chain. Keep the API's
	// fail-open guarantee by attributing it to every root in the current scope.
	return refsForRoots(scopeRoots(ctx), name)
}

func distinctStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
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
		if i < len(out.positions) {
			refs = out.positions[i]
		} else if i < len(out.names) {
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
		rr.collect(d, ctx, ColumnClauseFrom)
	}
	rr.collect(del.Where, ctx, ColumnClauseWhere)
	for _, j := range del.Joins {
		rr.collect(j.On, ctx, ColumnClauseJoinOn)
		for _, u := range j.Using {
			rr.addRefs(rr.resolveUnqualified(u.text(), ctx), ColumnClauseJoinUsing)
		}
	}
	rr.collect(del.OrderBy, ctx, ColumnClauseOrderBy)
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
		rr.collect(d, ctx, ColumnClauseFrom)
	}

	// SET pairs: [targetColumnIdentifier, valueExpression].
	for _, pair := range upd.Set {
		if len(pair) >= 1 {
			// A target may be a bare column (`SET x = …`) or table-qualified
			// (`SET t.x = …`); resolve to the bare column name and attribute it to
			// the qualified table, falling back to the UPDATE target.
			col, qualifier := dotColumn(pair[0])
			if col == "" {
				// Some dialects (e.g. MySQL) store a qualified target as a single
				// ident whose name is the dotted path "t.x".
				var id ident
				if decodeInto(pair[0], &id) == nil && id.Name != "" {
					col, qualifier = splitQualifiedName(id.Name)
				}
			}
			if col != "" {
				if qualifier != "" {
					for _, ref := range rr.resolveQualified(strings.ToLower(qualifier), col, ctx) {
						rr.add(ref.table, ref.col, ColumnClauseUpdateSetTarget)
					}
				} else {
					rr.add(targetRoot, col, ColumnClauseUpdateSetTarget)
				}
			}
		}
		for i := 1; i < len(pair); i++ {
			rr.collect(pair[i], ctx, ColumnClauseUpdateSetValue)
		}
	}

	rr.collect(upd.Where, ctx, ColumnClauseWhere)
	for _, j := range upd.FromJoins {
		rr.collect(j.On, ctx, ColumnClauseJoinOn)
	}
	rr.collect(upd.OrderBy, ctx, ColumnClauseOrderBy)
}

// resolveMerge handles MERGE INTO … USING … ON … WHEN …, capturing the ON
// condition and every column referenced in the WHEN clauses.
func (rr *refResolver) resolveMerge(mrg *mergeNode) {
	ctx := rr.newScope(mrg.With, nil)
	rr.addSource(mrg.This, ctx)
	rr.addSource(mrg.Using, ctx)
	for _, d := range ctx.deferred {
		rr.collect(d, ctx, ColumnClauseFrom)
	}
	rr.collect(mrg.On, ctx, ColumnClauseMergeOn)
	rr.collect(mrg.Whens, ctx, ColumnClauseMergeWhen)
}

// lineageDMLValueColumns provides the structural value-flow path used by
// LineageSourceColumns for UPDATE and MERGE. Polyglot's OpenLineage endpoint
// rejects those statement roots, but their assignment-value ASTs can reuse the
// same scope resolver as ReferencedColumns without crediting filter clauses.
func lineageDMLValueColumns(
	stmt map[string]any,
	metadata map[string][]string,
) (map[string]map[string]struct{}, bool, error) {
	var st statement
	if err := decodeNode(stmt, &st); err != nil {
		return nil, false, err
	}
	rr := &refResolver{
		metadata: metadata,
		result:   map[string]map[string]struct{}{},
		flowOnly: true,
	}
	switch {
	case st.Update != nil:
		rr.resolveUpdateValueFlow(st.Update)
		return rr.result, true, nil
	case st.Merge != nil:
		rr.resolveMergeValueFlow(st.Merge)
		return rr.result, true, nil
	default:
		return nil, false, nil
	}
}

func (rr *refResolver) resolveUpdateValueFlow(upd *updateNode) {
	ctx := rr.newScope(upd.With, nil)
	rr.addTable(upd.Table, ctx)
	if upd.FromClause != nil {
		for _, sourceExpr := range upd.FromClause.Expressions {
			rr.addSource(sourceExpr, ctx)
		}
	}
	for _, join := range upd.FromJoins {
		rr.addSource(join.This, ctx)
	}
	for _, pair := range upd.Set {
		for i := 1; i < len(pair); i++ {
			rr.recordFlowRefs(rr.collectRefs(pair[i], ctx, ColumnClauseUpdateSetValue))
		}
	}
}

func (rr *refResolver) resolveMergeValueFlow(mrg *mergeNode) {
	ctx := rr.newScope(mrg.With, nil)
	rr.addSource(mrg.This, ctx)
	rr.addSource(mrg.Using, ctx)
	for _, value := range mergeAssignmentValues(mrg.Whens) {
		rr.recordFlowRefs(rr.collectRefs(value, ctx, ColumnClauseMergeWhen))
	}
}

func (rr *refResolver) recordFlowRefs(refs []colRef) {
	for _, ref := range refs {
		if ref.table == "" || ref.col == "" || ref.col == "*" {
			continue
		}
		ensureSet(rr.result, ref.table)[ref.col] = struct{}{}
	}
}

// mergeAssignmentValues extracts only values written by MERGE actions. WHEN
// conditions and target identifiers are deliberately excluded: they choose
// rows or destinations but do not produce assigned values.
func mergeAssignmentValues(whens expr) []expr {
	root, ok := decodeObj(whens)
	if !ok {
		return nil
	}
	body, ok := decodeObj(root["whens"])
	if !ok {
		return nil
	}
	var values []expr
	for _, rawWhen := range decodeArr(body["expressions"]) {
		wrapper, ok := decodeObj(rawWhen)
		if !ok {
			continue
		}
		when, ok := decodeObj(wrapper["when"])
		if !ok {
			continue
		}
		then, ok := decodeObj(when["then"])
		if !ok {
			continue
		}
		tuple, ok := decodeObj(then["tuple"])
		if !ok {
			continue
		}
		parts := decodeArr(tuple["expressions"])
		if len(parts) < 2 {
			continue
		}
		switch mergeActionName(parts[0]) {
		case "UPDATE":
			assignments, ok := decodeObj(parts[1])
			if !ok {
				continue
			}
			assignmentTuple, ok := decodeObj(assignments["tuple"])
			if !ok {
				continue
			}
			for _, assignment := range decodeArr(assignmentTuple["expressions"]) {
				assignmentObj, ok := decodeObj(assignment)
				if !ok {
					continue
				}
				eq, ok := decodeObj(assignmentObj["eq"])
				if ok && len(eq["right"]) > 0 {
					values = append(values, eq["right"])
				}
			}
		case "INSERT":
			// INSERT actions encode target columns in the penultimate tuple and
			// assigned values in the final tuple.
			valueWrapper, ok := decodeObj(parts[len(parts)-1])
			if !ok {
				continue
			}
			valueTuple, ok := decodeObj(valueWrapper["tuple"])
			if !ok {
				continue
			}
			values = append(values, decodeArr(valueTuple["expressions"])...)
		}
	}
	return values
}

func mergeActionName(action expr) string {
	wrapper, ok := decodeObj(action)
	if !ok {
		return ""
	}
	var actionVar struct {
		This string `json:"this"`
	}
	if decodeInto(wrapper["var"], &actionVar) != nil {
		return ""
	}
	return strings.ToUpper(actionVar.This)
}

// ----------------------------------------------------------------------------
// Decoding helpers
// ----------------------------------------------------------------------------

// decodeNode re-encodes a decoded-JSON map and decodes it into a typed value.
func decodeNode(node map[string]any, v any) error {
	if err := json.Unmarshal(mustMarshal(node), v); err != nil {
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
	return json.Unmarshal(e, v)
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
	if json.Unmarshal(e, &m) != nil {
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
	if json.Unmarshal(e, &a) != nil {
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

// dotColumn extracts (column, qualifier) from a dot chain such as
// catalog.schema.table.column. The outermost field is the column; every segment
// before it is the qualifier so fully-qualified references can disambiguate
// tables with the same bare name. node is the {"dot": …} wrapper.
// splitQualifiedName splits a dotted name into its final segment (column) and
// the preceding qualifier, e.g. "t.x" -> ("x", "t") and "x" -> ("x", "").
func splitQualifiedName(name string) (col, qualifier string) {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:], name[:i]
	}
	return name, ""
}

func dotColumn(node expr) (col, qualifier string) {
	segs := dotSegments(node)
	if len(segs) == 0 {
		return "", ""
	}
	col = segs[len(segs)-1]
	if len(segs) >= 2 {
		qualifier = strings.Join(segs[:len(segs)-1], ".")
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
