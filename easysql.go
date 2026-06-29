// Package easysql rewrites SELECT statements to enforce row-level access policies,
// built on the Polyglot SQL engine (a multi-dialect sqlglot-compatible parser
// exposed to Go over an FFI library).
//
// Given a boolean predicate, every reference to an in-scope physical table is
// turned into a filtered derived table:
//
//	select * from a  ->  SELECT * FROM (SELECT * FROM a WHERE <predicate>) AS a
//
// Wrapping each table in its own pre-filtered subquery (instead of appending a
// single top-level WHERE) keeps the rewrite correct across JOIN / LEFT JOIN
// (no outer-join degradation), CTEs, subqueries and UNION, because each table
// is already filtered before it is joined or combined.
//
// Only SELECT and set operations (UNION / INTERSECT / EXCEPT) are supported;
// anything else is rejected (fail-closed).
//
// The rewrite operates on the parsed AST: each in-scope physical-table node is
// replaced by a filtered-subquery node, and the statement is rendered back to
// SQL by the engine's generator. The output is therefore normalized
// (re-formatted by the generator) rather than byte-identical to the input. The
// rewritten SQL is always re-parsed before being returned, so the call either
// yields valid SQL or fails closed (ErrInternal).
package easysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// Sentinel errors. Use errors.Is to classify a failure.
var (
	// ErrParse means the input SQL could not be parsed (user syntax error).
	ErrParse = errors.New("easysql: parse error")
	// ErrUnsupported means the input parsed but is not a single supported
	// statement (e.g. UPDATE/DELETE/INSERT, multiple statements).
	ErrUnsupported = errors.New("easysql: unsupported statement")
	// ErrInternal means the rewriter produced invalid SQL (a defect here).
	ErrInternal = errors.New("easysql: internal error")
)

// dialectToPolyglot maps our dialect names to Polyglot dialect identifiers.
// It doubles as the set of accepted dialects.
var dialectToPolyglot = map[string]string{
	"mysql":     "mysql",
	"starrocks": "starrocks",
	"postgres":  "postgresql",
	"trino":     "trino",
}

// rewriter applies a single row-level WHERE expression to SELECT statements. It
// is built per ApplyRowFilter call and is not part of the public API.
type rewriter struct {
	client  *polyglot.Client
	dialect string // our dialect name
	pg      string // polyglot dialect identifier

	// Table scope. scopeSet becomes true once any scope option is supplied;
	// while it is false the WHERE expression applies to every table.
	scopeSet  bool
	byKey     map[string]bool // "schema\x00table"
	byName    map[string]bool // bare table names
	regexps   []*regexp.Regexp
	defaultDB string

	// whereText is the caller's predicate, baked verbatim into the subquery
	// template's WHERE clause.
	whereText string

	normalize func(string) string // optional dialect text normalizer

	// tmplSubquery is the AST subquery template used by the rewrite. It is built
	// lazily, once per rewriter, with the predicate already baked into its
	// WHERE clause.
	tmplSubquery map[string]any
}

// Option configures an ApplyRowFilter call.
type Option func(*options)

type options struct {
	defaultDB   string
	tableNames  []string
	tableRegexp []string
	dialect     string
}

// WithDefaultDB sets the schema used to resolve unqualified table names against
// a schema-qualified scope (typically the session's current database).
func WithDefaultDB(db string) Option { return func(o *options) { o.defaultDB = db } }

// WithTableNames restricts the WHERE expression to the given tables. Names may
// be bare ("orders") or schema-qualified ("sales.orders").
func WithTableNames(names ...string) Option {
	return func(o *options) { o.tableNames = append(o.tableNames, names...) }
}

// WithTableRegexp restricts the WHERE expression to tables whose name (as
// written) matches any of the given Go regular expressions. Composes additively
// with WithTableNames.
func WithTableRegexp(patterns ...string) Option {
	return func(o *options) { o.tableRegexp = append(o.tableRegexp, patterns...) }
}

// WithDialect selects the SQL dialect: one of mysql, starrocks, postgres, trino.
func WithDialect(d string) Option { return func(o *options) { o.dialect = d } }

// WithSelfCheck is retained for backward compatibility and is now a no-op: the
// rewritten SQL is always re-parsed and the call fails closed (ErrInternal) if
// it is not valid, so self-checking can no longer be disabled.
//
// Deprecated: validity is always checked; this option has no effect.
func WithSelfCheck(bool) Option { return func(*options) {} }

// ApplyRowFilter rewrites sql (a single SELECT/UNION) so that every reference to
// an in-scope physical table is wrapped in a derived table pre-filtered by
// whereClause:
//
//	select * from a  ->  SELECT * FROM (SELECT * FROM a WHERE <whereClause>) AS a
//
// whereClause is a boolean SQL expression spliced verbatim, so the caller is
// responsible for any value binding or escaping inside it. The statement is
// rebuilt from its AST, so the output is normalized (re-formatted) rather than
// byte-identical to the input.
//
// The native SQL engine is loaded automatically on first use (see Init), so no
// setup is required. Errors classify via errors.Is against ErrParse,
// ErrUnsupported and ErrInternal; configuration mistakes (empty whereClause,
// unknown dialect, bad table regexp, …) return a plain error.
func ApplyRowFilter(sql, whereClause string, opts ...Option) (string, error) {
	client, err := defaultClient()
	if err != nil {
		return "", err
	}
	r, err := compile(client, whereClause, opts...)
	if err != nil {
		return "", err
	}
	return r.rewrite(sql)
}

// compile validates whereClause and the options and builds a reusable rewriter.
func compile(client *polyglot.Client, whereClause string, opts ...Option) (*rewriter, error) {
	if client == nil {
		return nil, errors.New("easysql: nil polyglot client")
	}
	if strings.TrimSpace(whereClause) == "" {
		return nil, errors.New("easysql: where clause must not be empty")
	}

	cfg := options{dialect: "mysql"}
	for _, o := range opts {
		o(&cfg)
	}

	pg, ok := dialectToPolyglot[cfg.dialect]
	if !ok {
		return nil, fmt.Errorf("easysql: unknown dialect %q", cfg.dialect)
	}

	r := &rewriter{
		client:    client,
		dialect:   cfg.dialect,
		pg:        pg,
		byKey:     map[string]bool{},
		byName:    map[string]bool{},
		defaultDB: strings.ToLower(strings.TrimSpace(cfg.defaultDB)),
	}

	if cfg.tableNames != nil {
		r.scopeSet = true
		for _, n := range cfg.tableNames {
			schema, table := splitName(n)
			if table == "" {
				continue
			}
			if schema != "" {
				r.byKey[strings.ToLower(schema)+"\x00"+strings.ToLower(table)] = true
			} else {
				r.byName[strings.ToLower(table)] = true
			}
		}
	}
	for _, pat := range cfg.tableRegexp {
		r.scopeSet = true
		if strings.TrimSpace(pat) == "" {
			return nil, errors.New("easysql: table regexp requires a non-empty pattern")
		}
		rx, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("easysql: invalid table name pattern %q: %w", pat, err)
		}
		r.regexps = append(r.regexps, rx)
	}
	if r.scopeSet && len(r.byKey) == 0 && len(r.byName) == 0 && len(r.regexps) == 0 {
		return nil, errors.New("easysql: a table scope option was provided but matched no tables")
	}

	if cfg.dialect == "starrocks" {
		r.normalize = starrocksNormalizer
	}

	if err := r.compileWhere(whereClause); err != nil {
		return nil, err
	}
	return r, nil
}

// compileWhere validates whereClause (it must be a boolean expression) and
// stores its trimmed text for verbatim splicing.
func (r *rewriter) compileWhere(whereClause string) error {
	raw, err := r.client.ParseOne("SELECT 1 WHERE "+whereClause, r.pg)
	if err != nil {
		return fmt.Errorf("easysql: invalid where clause %q: %w", whereClause, err)
	}
	var node map[string]any
	if err := sonic.Unmarshal(raw, &node); err != nil {
		return err
	}
	this, ok := dig(node, "select", "where_clause", "this")
	if !ok || this == nil {
		return fmt.Errorf("easysql: invalid where clause %q: not a boolean expression", whereClause)
	}
	r.whereText = strings.TrimSpace(whereClause)
	return nil
}

// prepare runs the steps shared by parsing and decision-making: dialect
// normalization, the input guard, parsing, single-supported-statement
// validation, and the scope-aware collection of table references. It returns the
// parsed statement list (mutated in place by the rewrite) and the per-table
// decisions.
func (r *rewriter) prepare(sql string) ([]any, []*tableDecision, error) {
	if r.normalize != nil {
		sql = r.normalize(sql)
	}

	// Reject pathologically large or deeply nested input before touching the
	// FFI: polyglot's native recursive-descent parser can stack-overflow and
	// crash the whole process on deep expression nesting (which Go cannot
	// recover from). Fail closed instead.
	if err := guardInput(sql); err != nil {
		return nil, nil, err
	}

	raw, err := r.client.Parse(sql, r.pg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	var stmts []any
	if err := sonic.Unmarshal(raw, &stmts); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	stmts = dropNils(stmts)
	if len(stmts) != 1 {
		return nil, nil, fmt.Errorf("%w: expected exactly one statement, got %d", ErrUnsupported, len(stmts))
	}
	if !isSupportedRoot(stmts[0]) {
		return nil, nil, fmt.Errorf("%w: only SELECT statements are supported", ErrUnsupported)
	}

	var decs []*tableDecision
	r.collect(stmts[0], nil, &decs)
	return stmts, decs, nil
}

const tmplPlaceholder = "__easysql_ph__"

// rewrite rewrites sql by transforming the AST -- replacing each in-scope
// physical-table node with a filtered-subquery node -- and rendering the result
// back to SQL with the engine's generator.
func (r *rewriter) rewrite(sql string) (string, error) {
	stmts, decs, err := r.prepare(sql)
	if err != nil {
		return "", err
	}

	strip := map[string]bool{}
	wrapped := 0
	var tmpl map[string]any
	for _, d := range decs {
		if !d.wrap {
			continue
		}
		if tmpl == nil {
			if tmpl, err = r.subqueryTemplate(); err != nil {
				return "", err
			}
		}
		r.wrapTableNode(d.node, tmpl)
		wrapped++
		if d.strip {
			strip[d.stripKey] = true
		}
	}

	// Nothing in scope: a genuine no-op. Return the input unchanged instead of
	// round-tripping it through the generator -- that would reformat untouched
	// SQL for no reason and, for pathological input, the generator cannot always
	// reproduce valid SQL.
	if wrapped == 0 {
		return sql, nil
	}

	if len(strip) > 0 {
		stripSchemaAST(stmts[0], strip)
	}

	gen, err := r.client.Generate(mustMarshal(stmts), r.pg)
	if err != nil || len(gen) == 0 {
		return "", fmt.Errorf("%w: generate failed: %v", ErrInternal, err)
	}
	res := gen[0]

	// Mandatory validity gate: never return SQL that does not parse. This turns
	// any transform defect into a classified ErrInternal (fail closed) instead
	// of emitting malformed SQL.
	if _, err := r.client.ParseOne(res, r.pg); err != nil {
		return "", fmt.Errorf("%w: rewritten SQL failed to re-parse: %v\nrewritten: %s",
			ErrInternal, err, res)
	}
	return res, nil
}

// subqueryTemplate returns (and caches) the AST node for a filtered subquery
// with the predicate already baked into its WHERE clause:
//
//	(SELECT * FROM __ph__ WHERE <predicate>) AS __ph__
//
// Building it by parsing once (rather than hand-assembling JSON) guarantees the
// node carries every field the engine expects. The inner FROM table and the
// subquery alias are substituted per call in wrapTableNode.
func (r *rewriter) subqueryTemplate() (map[string]any, error) {
	if r.tmplSubquery != nil {
		return r.tmplSubquery, nil
	}
	tmplSQL := "SELECT * FROM (SELECT * FROM " + tmplPlaceholder + " WHERE " +
		r.whereText + ") AS " + tmplPlaceholder
	raw, err := r.client.ParseOne(tmplSQL, r.pg)
	if err != nil {
		return nil, fmt.Errorf("%w: building subquery template: %v", ErrInternal, err)
	}
	var node map[string]any
	if err := sonic.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	exprs, ok := dig(node, "select", "from", "expressions")
	if !ok {
		return nil, fmt.Errorf("%w: malformed subquery template", ErrInternal)
	}
	list, ok := exprs.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%w: malformed subquery template", ErrInternal)
	}
	item, ok := list[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: malformed subquery template", ErrInternal)
	}
	sub, ok := item["subquery"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: malformed subquery template", ErrInternal)
	}
	r.tmplSubquery = sub
	return sub, nil
}

// wrapTableNode converts an AST table node ({"table": ...}) in place into a
// filtered-subquery node ({"subquery": ...}). The original table (with its alias
// stripped) becomes the subquery's inner FROM table, and the subquery inherits
// the table's alias (or, when there is none, its name).
func (r *rewriter) wrapTableNode(node map[string]any, tmpl map[string]any) {
	orig, _ := node["table"].(map[string]any)
	if orig == nil {
		return
	}

	// Alias for the derived table: the explicit alias when present, otherwise
	// the table's own name identifier.
	var aliasNode any
	if a, ok := orig["alias"].(map[string]any); ok && a != nil {
		aliasNode = deepCopyJSON(a)
	} else {
		aliasNode = deepCopyJSON(orig["name"])
	}

	// Inner table = the original, minus its alias (the alias moves outward).
	innerTable, _ := deepCopyJSON(orig).(map[string]any)
	innerTable["alias"] = nil

	sub, _ := deepCopyJSON(tmpl).(map[string]any)
	sub["alias"] = aliasNode
	if sel, ok := dig(sub, "this", "select"); ok {
		if selMap, ok := sel.(map[string]any); ok {
			if from, ok := selMap["from"].(map[string]any); ok {
				if exprs, ok := from["expressions"].([]any); ok && len(exprs) > 0 {
					exprs[0] = map[string]any{"table": innerTable}
				}
			}
		}
	}

	delete(node, "table")
	node["subquery"] = sub
}

// stripSchemaAST rewrites three-part column references schema.table.col -> table.col
// for every (schema, table) in strip, so they keep resolving against the derived
// table aliased by the bare table name. In the AST a three-part reference is a
// Dot over a two-part Column:
//
//	{"dot": {"field": <col>, "this": {"column": {"name": <table>, "table": <schema>}}}}
//
// which becomes a plain two-part column {"column": {"name": <col>, "table": <table>}}.
func stripSchemaAST(node any, strip map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		if dn, ok := v["dot"].(map[string]any); ok && stripDot(v, dn, strip) {
			return // replaced in place; nothing deeper to strip here
		}
		for _, child := range v {
			stripSchemaAST(child, strip)
		}
	case []any:
		for _, child := range v {
			stripSchemaAST(child, strip)
		}
	}
}

// stripDot tries to collapse a schema.table.col Dot node (held in v under "dot")
// into a table.col Column. It reports whether it replaced the node.
func stripDot(v, dn map[string]any, strip map[string]bool) bool {
	this, _ := dn["this"].(map[string]any)
	if this == nil {
		return false
	}
	col, _ := this["column"].(map[string]any)
	if col == nil {
		return false
	}
	schema := identName(col["table"]) // the "sales" in sales.orders
	table := identName(col["name"])   // the "orders" in sales.orders
	if schema == "" || table == "" {
		return false
	}
	if !strip[strings.ToLower(schema)+"\x00"+strings.ToLower(table)] {
		return false
	}
	newCol := map[string]any{
		"join_mark":         false,
		"name":              dn["field"], // the ".col" identifier
		"table":             col["name"], // the "orders" identifier
		"trailing_comments": []any{},
	}
	delete(v, "dot")
	v["column"] = newCol
	return true
}

// deepCopyJSON returns a deep copy of a value decoded from JSON (maps, slices
// and scalars), so a template can be reused without aliasing.
func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = deepCopyJSON(val)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, val := range x {
			s[i] = deepCopyJSON(val)
		}
		return s
	default:
		return x
	}
}

// matches reports whether the table wrapper v is an in-scope physical table to
// filter. CTE references are handled by the scope-aware walk, not here.
func (r *rewriter) matches(v map[string]any) bool {
	t, _ := v["table"].(map[string]any)
	name := identName(t["name"])
	if name == "" {
		return false
	}
	nameL := strings.ToLower(name)
	schemaW := strings.ToLower(identName(t["schema"]))

	if schemaW == "" && nameL == "dual" {
		return false
	}
	if !r.scopeSet {
		return true
	}
	schemaR := schemaW
	if schemaR == "" {
		schemaR = r.defaultDB
	}
	if schemaR != "" && r.byKey[schemaR+"\x00"+nameL] {
		return true
	}
	if r.byName[nameL] {
		return true
	}
	for _, rx := range r.regexps {
		if rx.MatchString(name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// AST collection: which table references to wrap (source order)
// ---------------------------------------------------------------------------

// tableDecision describes one physical-table reference found in the AST.
type tableDecision struct {
	wrap     bool           // wrap this reference in a filtered subquery
	strip    bool           // outer schema.table.col refs must drop the schema
	stripKey string         // "schema\x00table" for stripping
	node     map[string]any // the AST {"table":...} node, mutated in place when wrapped
}

// collect walks the AST in source order, appending one tableDecision per
// physical-table reference (including CTE references, which are recorded but
// never wrapped). scope is the stack of CTE-name sets in effect, used to tell
// CTE references apart from real tables.
func (r *rewriter) collect(node any, scope []map[string]bool, out *[]*tableDecision) {
	switch v := node.(type) {
	case map[string]any:
		if body := queryBody(v); body != nil {
			r.collectQuery(body, scope, out)
			return
		}
		if t, _ := v["table"].(map[string]any); t != nil && identName(t["name"]) != "" {
			*out = append(*out, r.decide(v, scope))
			return
		}
		for _, k := range sortedKeys(v) {
			r.collect(v[k], scope, out)
		}
	case []any:
		for _, child := range v {
			r.collect(child, scope, out)
		}
	}
}

// collectQuery visits a SELECT or set-operation body in source order. CTE
// definition bodies are visited under the outer scope (a non-recursive CTE does
// not shadow a real table of the same name inside its own definition); the rest
// of the query sees the CTE names in scope.
func (r *rewriter) collectQuery(body map[string]any, scope []map[string]bool, out *[]*tableDecision) {
	if _, isSet := body["left"]; isSet {
		newScope := r.pushCTEs(body, scope, out)
		r.collect(body["left"], newScope, out)
		r.collect(body["right"], newScope, out)
		r.collect(body["order_by"], newScope, out)
		return
	}

	newScope := r.pushCTEs(body, scope, out)
	r.collect(body["hint"], newScope, out)
	r.collect(body["expressions"], newScope, out) // SELECT list (scalar subqueries)
	r.collect(body["from"], newScope, out)
	if joins, ok := body["joins"].([]any); ok {
		for _, j := range joins {
			if jm, ok := j.(map[string]any); ok {
				r.collect(jm["this"], newScope, out) // joined table first
				r.collect(jm["on"], newScope, out)
				r.collect(jm["using"], newScope, out)
			}
		}
	}
	for _, k := range []string{
		"where_clause", "group_by", "having", "qualify", "windows",
		"distinct_on", "sort_by", "order_by", "lateral_views", "connect",
	} {
		r.collect(body[k], newScope, out)
	}
}

// pushCTEs visits CTE definition bodies (outer scope) and returns scope extended
// with the CTE names declared by body.
func (r *rewriter) pushCTEs(body map[string]any, scope []map[string]bool, out *[]*tableDecision) []map[string]bool {
	w, ok := body["with"].(map[string]any)
	if !ok {
		return scope
	}
	if list, ok := w["ctes"].([]any); ok {
		for _, e := range list {
			if cte, ok := e.(map[string]any); ok {
				r.collect(cte["this"], scope, out)
			}
		}
	}
	names := cteNames(w)
	if len(names) > 0 {
		return append(append([]map[string]bool{}, scope...), names)
	}
	return scope
}

// decide builds the tableDecision for a physical-table node v.
func (r *rewriter) decide(v map[string]any, scope []map[string]bool) *tableDecision {
	t := v["table"].(map[string]any)
	name := strings.ToLower(identName(t["name"]))
	schema := identName(t["schema"])

	d := &tableDecision{node: v}
	if inScope(name, scope) {
		return d // CTE reference: never wrapped
	}
	d.wrap = r.matches(v)
	if !d.wrap {
		return d
	}
	// A derived-table alias is synthesized from the bare name when the table has
	// no explicit alias, so schema-qualified column references (schema.table.col)
	// must drop the schema to keep resolving.
	if _, hasAlias := t["alias"].(map[string]any); !hasAlias && schema != "" {
		d.strip = true
		d.stripKey = strings.ToLower(schema) + "\x00" + name
	}
	return d
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Input guard
// ---------------------------------------------------------------------------

const (
	// maxInputBytes bounds total work and rules out adversarial giant inputs.
	maxInputBytes = 1 << 20 // 1 MiB
	// maxBracketDepth is a conservative cap on the nesting of grouping
	// brackets. Polyglot's native recursive-descent parser stack-overflows on
	// deeply nested expressions -- e.g. function calls (...), or {...}/[...]
	// constructs -- and the crash (observed > ~150 here) cannot be recovered in
	// Go. The cap stays well below the crash point yet far above any sane query.
	maxBracketDepth = 64
)

// guardInput fails closed (ErrUnsupported) on input that could crash or stall
// the native parser. It is a heuristic scan over the raw text (it does not skip
// string/comment contents) that bounds the nesting of all grouping bracket
// families, which can only cause a safe false rejection of bizarre input, never
// an unsafe rewrite.
func guardInput(sql string) error {
	if len(sql) > maxInputBytes {
		return fmt.Errorf("%w: input too large (%d bytes)", ErrUnsupported, len(sql))
	}
	depth, max := 0, 0
	for _, c := range sql {
		switch c {
		case '(', '[', '{':
			depth++
			if depth > max {
				max = depth
			}
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	if max > maxBracketDepth {
		return fmt.Errorf("%w: input nesting too deep (%d levels)", ErrUnsupported, max)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var setOpKeys = []string{"union", "intersect", "except"}

// queryBody returns the inner object of a SELECT or set-operation node, which
// is where a WITH clause and child queries live, or nil otherwise.
func queryBody(v map[string]any) map[string]any {
	if s, ok := v["select"].(map[string]any); ok {
		return s
	}
	for _, k := range setOpKeys {
		if m, ok := v[k].(map[string]any); ok {
			return m
		}
	}
	return nil
}

func isSupportedRoot(node any) bool {
	m, ok := node.(map[string]any)
	if !ok {
		return false
	}
	return queryBody(m) != nil
}

func cteNames(w map[string]any) map[string]bool {
	names := map[string]bool{}
	list, ok := w["ctes"].([]any)
	if !ok {
		return names
	}
	for _, e := range list {
		if cte, ok := e.(map[string]any); ok {
			if n := identName(cte["alias"]); n != "" {
				names[strings.ToLower(n)] = true
			}
		}
	}
	return names
}

func inScope(nameLower string, scope []map[string]bool) bool {
	for _, set := range scope {
		if set[nameLower] {
			return true
		}
	}
	return false
}

// identName returns the name of an identifier node ({"name": "...", ...}).
func identName(v any) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["name"].(string); ok {
			return s
		}
	}
	return ""
}

func splitName(name string) (schema, table string) {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, "."); i >= 0 {
		return strings.TrimSpace(name[:i]), strings.TrimSpace(name[i+1:])
	}
	return "", name
}

func dig(m map[string]any, keys ...string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = cm[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func dropNils(in []any) []any {
	out := in[:0]
	for _, x := range in {
		if x != nil {
			out = append(out, x)
		}
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, _ := sonic.Marshal(v)
	return b
}

// ---------------------------------------------------------------------------
// Dialect text normalization
// ---------------------------------------------------------------------------

var bracketHintRE = regexp.MustCompile(`(?i)\[\s*(broadcast|shuffle|bucket_shuffle|colocate|replicated)\s*\]`)

// starrocksNormalizer removes StarRocks join distribution hints (which the
// parser cannot read) from code segments only, leaving string literals, quoted
// identifiers and comments untouched. The hint is advisory (it affects the
// plan, not the result), so dropping it is a safe, lossy transform.
func starrocksNormalizer(sql string) string {
	return mapCodeSegments(sql, func(code string) string {
		return bracketHintRE.ReplaceAllString(code, "")
	})
}

// mapCodeSegments applies fn to portions of sql outside strings, quoted
// identifiers and comments.
func mapCodeSegments(sql string, fn func(string) string) string {
	var out strings.Builder
	var code strings.Builder
	flush := func() {
		if code.Len() > 0 {
			out.WriteString(fn(code.String()))
			code.Reset()
		}
	}
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			flush()
			j := i + 1
			for j < n {
				if sql[j] == '\\' && c != '`' && j+1 < n {
					j += 2
					continue
				}
				if sql[j] == c {
					if j+1 < n && sql[j+1] == c { // doubled quote escape
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			if j > n {
				j = n
			}
			out.WriteString(sql[i:j])
			i = j
		case c == '-' && i+2 < n && sql[i+1] == '-' && sql[i+2] == ' ':
			flush()
			j := i
			for j < n && sql[j] != '\n' {
				j++
			}
			out.WriteString(sql[i:j])
			i = j
		case c == '#':
			flush()
			j := i
			for j < n && sql[j] != '\n' {
				j++
			}
			out.WriteString(sql[i:j])
			i = j
		case c == '/' && i+1 < n && sql[i+1] == '*':
			flush()
			j := i + 2
			for j+1 < n && !(sql[j] == '*' && sql[j+1] == '/') {
				j++
			}
			j += 2
			if j > n {
				j = n
			}
			out.WriteString(sql[i:j])
			i = j
		default:
			code.WriteByte(c)
			i++
		}
	}
	flush()
	return out.String()
}
