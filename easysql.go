// Package easysql analyzes and structurally rewrites multi-dialect SQL using the
// Polyglot SQL engine (a sqlglot-compatible parser exposed to Go over an FFI
// library). Its rewrite operations include binding queries as CTEs, replacing
// table references, and enforcing row-level access policies.
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
	scopeSet     bool
	byCatalogKey map[string]bool // "catalog\x00schema\x00table"
	byKey        map[string]bool // "schema\x00table"
	byName       map[string]bool // bare table names
	regexps      []*regexp.Regexp
	defaultDB    string

	// whereText is the caller's predicate, parsed into the subquery template's
	// WHERE clause by Polyglot's builder engine.
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
// be bare ("orders"), schema-qualified ("sales.orders"), or catalog-qualified
// ("iceberg.sales.orders").
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
// whereClause is parsed as a boolean SQL expression in the selected dialect, so
// the caller remains responsible for binding or escaping values inside it. The
// statement is rebuilt from its AST, so the output is normalized (re-formatted)
// rather than byte-identical to the input.
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
	for i, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("easysql: row-filter option %d must not be nil", i)
		}
		o(&cfg)
	}

	pg, ok := dialectToPolyglot[cfg.dialect]
	if !ok {
		return nil, fmt.Errorf("easysql: unknown dialect %q", cfg.dialect)
	}

	r := &rewriter{
		client:       client,
		dialect:      cfg.dialect,
		pg:           pg,
		byCatalogKey: map[string]bool{},
		byKey:        map[string]bool{},
		byName:       map[string]bool{},
		defaultDB:    strings.ToLower(strings.TrimSpace(cfg.defaultDB)),
	}

	if cfg.tableNames != nil {
		r.scopeSet = true
		for _, n := range cfg.tableNames {
			catalog, schema, table := splitName(n)
			if table == "" {
				continue
			}
			if catalog != "" && schema != "" {
				r.byCatalogKey[strings.ToLower(catalog)+"\x00"+
					strings.ToLower(schema)+"\x00"+strings.ToLower(table)] = true
			} else if schema != "" {
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
	if r.scopeSet && len(r.byCatalogKey) == 0 && len(r.byKey) == 0 &&
		len(r.byName) == 0 && len(r.regexps) == 0 {
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
// stores its trimmed text for the immutable builder plan.
func (r *rewriter) compileWhere(whereClause string) error {
	// Bound predicate bytes independently of the statement. Native parsing
	// applies its own complexity and recursion limits to the complete input.
	if err := guardInput(whereClause); err != nil {
		return fmt.Errorf("easysql: unsafe where clause: %w", err)
	}
	raw, err := r.client.ParseOne("SELECT 1 WHERE "+whereClause, r.pg)
	if err != nil {
		return fmt.Errorf("easysql: invalid where clause %q: %w", whereClause, classifyParseError(err))
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return err
	}
	this, ok := dig(node, "select", "where_clause", "this")
	if !ok || this == nil {
		return fmt.Errorf("easysql: invalid where clause %q: not a boolean expression", whereClause)
	}
	r.whereText = strings.TrimSpace(whereClause)
	// Evaluate the subquery plan now so any builder-level incompatibility is
	// reported as a plain configuration error rather than later as an
	// ErrInternal from the rewrite.
	if _, err := r.subqueryTemplate(); err != nil {
		return fmt.Errorf("easysql: where clause %q cannot be safely applied as a filter predicate", whereClause)
	}
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

	// Enforce the byte budget before FFI; native parsing enforces depth limits.
	if err := guardInput(sql); err != nil {
		return nil, nil, err
	}

	raw, err := r.client.Parse(sql, r.pg)
	if err != nil {
		return nil, nil, classifyParseError(err)
	}
	var stmts []any
	if err := json.Unmarshal(raw, &stmts); err != nil {
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

	// Collect the in-scope physical-table references to wrap. Reuse the
	// RewriteTableReferences machinery for collision-free derived-table alias
	// assignment and for rebinding qualified column / star references onto those
	// aliases, so the two rewrite paths behave identically.
	var matches []*tableMatch
	for _, d := range decs {
		if !d.wrap {
			continue
		}
		table, _ := d.node["table"].(map[string]any)
		if table == nil {
			continue
		}
		matches = append(matches, newTableMatch(d.node, table, compiledTableRewrite{}))
	}

	// Nothing in scope: a genuine no-op. Return the input unchanged instead of
	// round-tripping it through the generator -- that would reformat untouched
	// SQL for no reason and, for pathological input, the generator cannot always
	// reproduce valid SQL.
	if len(matches) == 0 {
		return sql, nil
	}

	tmpl, err := r.subqueryTemplate()
	if err != nil {
		return "", err
	}

	// Assign a unique derived-table alias to every wrapped reference (so two
	// same-named tables in different schemas, or a table colliding with a CTE
	// name, do not produce duplicate FROM relations), then wrap each table in
	// its predicate-filtered subquery and rebind schema-/catalog-qualified
	// column and star references onto the assigned aliases.
	assignDerivedAliases(stmts[0], matches)
	rebindCtx := buildRebindContext(matches)
	for _, m := range matches {
		r.wrapTableNode(m.node, tmpl, m.alias)
	}
	rebindColumnRefs(stmts[0], rebindCtx, matches)

	gen, err := r.client.Generate(mustMarshal(stmts), r.pg)
	if err != nil || len(gen) == 0 {
		return "", fmt.Errorf("%w: generate failed: %v", ErrInternal, err)
	}
	res := gen[0]
	if err := guardInput(res); err != nil {
		return "", fmt.Errorf("easysql: generated rewritten SQL exceeds safe limits: %w", err)
	}

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
// Polyglot's immutable builder plan constructs the node in the shared Rust AST
// engine. The inner FROM table and subquery alias are substituted per call in
// wrapTableNode.
func (r *rewriter) subqueryTemplate() (map[string]any, error) {
	if r.tmplSubquery != nil {
		return r.tmplSubquery, nil
	}
	plan := polyglot.Select(polyglot.Star()).
		From(polyglot.Table(tmplPlaceholder)).
		Where(polyglot.Condition(r.whereText)).
		Subquery(tmplPlaceholder).
		ReadDialect(r.pg)
	sub, err := buildSubquery(r.client, plan, "build row-filter subquery template")
	if err != nil {
		return nil, err
	}
	r.tmplSubquery = sub
	return sub, nil
}

// wrapTableNode converts an AST table node ({"table": ...}) in place into a
// filtered-subquery node ({"subquery": ...}). The original table (with its alias
// stripped) becomes the subquery's inner FROM table, and the subquery is aliased
// with the collision-free alias assigned by assignDerivedAliases.
func (r *rewriter) wrapTableNode(node map[string]any, tmpl map[string]any, alias aliasRef) {
	orig, _ := node["table"].(map[string]any)
	if orig == nil {
		return
	}

	// Inner table = the original, minus its alias (the alias moves outward).
	innerTable, _ := deepCopyJSON(orig).(map[string]any)
	innerTable["alias"] = nil

	sub, _ := deepCopyJSON(tmpl).(map[string]any)
	sub["alias"] = newIdent(alias.name, alias.quoted)
	attachSubqueryColumnAliases(sub, orig)
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
	schema := identName(t["schema"])
	catalog := identName(t["catalog"])
	schemaW := strings.ToLower(schema)
	catalogW := strings.ToLower(catalog)

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
	if catalogW != "" && schemaW != "" &&
		r.byCatalogKey[catalogW+"\x00"+schemaW+"\x00"+nameL] {
		return true
	}
	if schemaR != "" && r.byKey[schemaR+"\x00"+nameL] {
		return true
	}
	if r.byName[nameL] {
		return true
	}
	fullName := name
	if schemaW != "" {
		fullName = schema + "." + name
	}
	if catalogW != "" {
		fullName = catalog + "." + fullName
	}
	for _, rx := range r.regexps {
		if rx.MatchString(name) || rx.MatchString(fullName) {
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
	wrap bool           // wrap this reference in a filtered subquery
	node map[string]any // the AST {"table":...} node, mutated in place when wrapped
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
	names := cteNames(w)

	// The scope under which the CTE definition bodies are visited. A
	// non-recursive CTE cannot shadow a real table of the same name inside its
	// own definition, so its body is visited under the OUTER scope. A RECURSIVE
	// WITH, however, makes the CTE names visible inside the bodies themselves
	// (self- and mutual references), so those references denote the CTE and must
	// NOT be wrapped as physical tables.
	bodyScope := scope
	if recursive, _ := w["recursive"].(bool); recursive && len(names) > 0 {
		bodyScope = append(append([]map[string]bool{}, scope...), names)
	}
	if list, ok := w["ctes"].([]any); ok {
		for _, e := range list {
			if cte, ok := e.(map[string]any); ok {
				r.collect(cte["this"], bodyScope, out)
			}
		}
	}
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
	catalog := identName(t["catalog"])

	d := &tableDecision{node: v}
	// Only an unqualified name can denote a CTE; a schema- or catalog-qualified
	// reference always refers to a physical table, even when a CTE shares its
	// bare name.
	if schema == "" && catalog == "" && inScope(name, scope) {
		return d // CTE reference: never wrapped
	}
	d.wrap = r.matches(v)
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

// maxInputBytes bounds total work independently of the native parser guard.
const maxInputBytes = 1 << 20 // 1 MiB

// Polyglot v0.11 protects parser recursion in every native parse entry point,
// including unary/IF chains without brackets. Keep only the API's byte budget
// here; scanning raw brackets incorrectly rejected quoted strings and comments.
func guardInput(sql string) error {
	if len(sql) > maxInputBytes {
		return fmt.Errorf("%w: input too large (%d bytes)", ErrUnsupported, len(sql))
	}
	return nil
}

// Native guard failures remain ErrUnsupported, as input-limit failures were
// before v0.11. Ordinary syntax errors retain ErrParse. The SDK exposes the
// guard's stable diagnostic code through Error.Message, not a Go sentinel.
func classifyParseError(err error) error {
	var native *polyglot.Error
	if errors.As(err, &native) && strings.Contains(native.Message, "E_GUARD_") {
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return fmt.Errorf("%w: %w", ErrParse, err)
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

func splitName(name string) (catalog, schema, table string) {
	parts := strings.Split(strings.TrimSpace(name), ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return "", "", ""
		}
	}
	switch len(parts) {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		return "", "", ""
	}
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
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
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
