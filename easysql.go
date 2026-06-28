// Package easysql rewrites SELECT statements to enforce row-level access policies,
// built on the Polyglot SQL engine (a multi-dialect sqlglot-compatible parser
// exposed to Go over an FFI library).
//
// Given a boolean predicate, every reference to an in-scope physical table is
// turned into a filtered derived table:
//
//	SELECT * FROM a  ->  SELECT * FROM (SELECT * FROM a WHERE <predicate>) AS a
//
// Wrapping each table in its own pre-filtered subquery (instead of appending a
// single top-level WHERE) keeps the rewrite correct across JOIN / LEFT JOIN
// (no outer-join degradation), CTEs, subqueries and UNION, because each table
// is already filtered before it is joined or combined.
//
// Only SELECT and set operations (UNION / INTERSECT / EXCEPT) are supported;
// anything else is rejected (fail-closed).
//
// The transform is performed directly on Polyglot's JSON AST. Each
// ApplyRowFilter call parses the WHERE expression and a wrapper skeleton once,
// then deep-clones them per table, so the number of FFI calls does not grow with
// the table count, and the original identifier quoting is preserved by splicing
// the original table node verbatim.
package easysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

	selfCheck bool

	// Table scope. scopeSet becomes true once any scope option is supplied;
	// while it is false the WHERE expression applies to every table.
	scopeSet  bool
	byKey     map[string]bool // "schema\x00table"
	byName    map[string]bool // bare table names
	regexps   []*regexp.Regexp
	defaultDB string

	// Parsed-once templates (immutable; always deep-cloned before mutation).
	whereExpr any            // boolean expression JSON (where_clause.this)
	skeleton  map[string]any // {"subquery": {...}} wrapper JSON

	normalize func(string) string // optional dialect text normalizer
}

// Option configures an ApplyRowFilter call.
type Option func(*options)

type options struct {
	defaultDB   string
	tableNames  []string
	tableRegexp []string
	dialect     string
	selfCheck   bool
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

// WithSelfCheck re-parses the rewritten SQL and fails closed (ErrInternal) if
// it is invalid. Off by default for speed; enable it in CI/canary.
func WithSelfCheck(b bool) Option { return func(o *options) { o.selfCheck = b } }

// ApplyRowFilter rewrites sql (a single SELECT/UNION) so that every reference to
// an in-scope physical table is wrapped in a derived table pre-filtered by
// whereClause:
//
//	SELECT * FROM a  ->  SELECT * FROM (SELECT * FROM a WHERE <whereClause>) AS a
//
// whereClause is a boolean SQL expression spliced verbatim as an AST node, so
// the caller is responsible for any value binding or escaping inside it.
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
		selfCheck: cfg.selfCheck,
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

	if err := r.compileSkeleton(); err != nil {
		return nil, err
	}
	if err := r.compileWhere(whereClause); err != nil {
		return nil, err
	}
	return r, nil
}

// compileSkeleton parses the wrapper subquery once so it can be cloned per table.
func (r *rewriter) compileSkeleton() error {
	const skeletonSQL = "SELECT * FROM (SELECT * FROM zzz_tbl WHERE 1 = 1) AS zzz_alias"
	raw, err := r.client.ParseOne(skeletonSQL, r.pg)
	if err != nil {
		return fmt.Errorf("easysql: cannot build wrapper skeleton: %w", err)
	}
	var node map[string]any
	if err := sonic.Unmarshal(raw, &node); err != nil {
		return err
	}
	sub, ok := dig(node, "select", "from", "expressions")
	exprs, _ := sub.([]any)
	if !ok || len(exprs) == 0 {
		return errors.New("easysql: unexpected skeleton shape")
	}
	sq, ok := exprs[0].(map[string]any)
	if !ok || sq["subquery"] == nil {
		return errors.New("easysql: unexpected skeleton subquery shape")
	}
	r.skeleton = sq
	return nil
}

// compileWhere validates whereClause and caches its boolean expression.
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
	r.whereExpr = this
	return nil
}

// rewrite rewrites sql (a single SELECT/UNION), wrapping every in-scope physical
// table in a derived table pre-filtered by the compiled WHERE expression.
func (r *rewriter) rewrite(sql string) (string, error) {
	if r.normalize != nil {
		sql = r.normalize(sql)
	}

	raw, err := r.client.Parse(sql, r.pg)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrParse, err)
	}
	var stmts []any
	if err := sonic.Unmarshal(raw, &stmts); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInternal, err)
	}
	stmts = dropNils(stmts)
	if len(stmts) != 1 {
		return "", fmt.Errorf("%w: expected exactly one statement, got %d", ErrUnsupported, len(stmts))
	}
	if !isSupportedRoot(stmts[0]) {
		return "", fmt.Errorf("%w: only SELECT statements are supported", ErrUnsupported)
	}

	rc := &rewriteCtx{r: r, strip: map[string]bool{}}
	rc.walk(stmts[0], nil)
	rc.applyStrip(stmts[0])

	out, err := r.client.Generate(mustMarshal(stmts), r.pg)
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("%w: generate failed: %v", ErrInternal, err)
	}
	res := out[0]

	if r.selfCheck {
		if _, err := r.client.ParseOne(res, r.pg); err != nil {
			return "", fmt.Errorf("%w: rewritten SQL failed to re-parse: %v\nrewritten: %s",
				ErrInternal, err, res)
		}
	}
	return res, nil
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
// Per-rewrite traversal
// ---------------------------------------------------------------------------

type rewriteCtx struct {
	r     *rewriter
	strip map[string]bool // "schema\x00table" pairs whose column refs lose the schema
}

// walk traverses the JSON AST. scope is a stack of CTE-name sets in effect at
// the current node, used to distinguish CTE references from physical tables.
func (rc *rewriteCtx) walk(node any, scope []map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		if body := queryBody(v); body != nil {
			rc.walkQuery(body, scope)
			return
		}
		if t, _ := v["table"].(map[string]any); t != nil && identName(t["name"]) != "" {
			name := strings.ToLower(identName(t["name"]))
			if inScope(name, scope) {
				return // CTE reference: never wrapped, never recursed
			}
			if rc.r.matches(v) {
				rc.wrap(v)
			}
			return // a table node has no children we need to descend into
		}
		for _, child := range v {
			rc.walk(child, scope)
		}
	case []any:
		for _, child := range v {
			rc.walk(child, scope)
		}
	}
}

// walkQuery handles a SELECT or set-operation body, pushing any CTE names it
// declares onto the scope before descending into the query (but not into its
// own CTE definition bodies, matching the rule that a non-recursive CTE does
// not shadow a real table of the same name inside its own definition).
func (rc *rewriteCtx) walkQuery(body map[string]any, scope []map[string]bool) {
	newScope := scope
	if w, ok := body["with"].(map[string]any); ok {
		names := cteNames(w)
		if list, ok := w["ctes"].([]any); ok {
			for _, e := range list {
				if cte, ok := e.(map[string]any); ok {
					rc.walk(cte["this"], scope) // definition body: outer scope only
				}
			}
		}
		if len(names) > 0 {
			newScope = append(append([]map[string]bool{}, scope...), names)
		}
	}
	for k, val := range body {
		if k == "with" {
			continue
		}
		rc.walk(val, newScope)
	}
}

// wrap replaces the table wrapper v in place with a filtered derived table.
func (rc *rewriteCtx) wrap(v map[string]any) {
	t := v["table"].(map[string]any)
	name := identName(t["name"])
	schemaW := identName(t["schema"])

	// Effective alias: reuse an explicit alias verbatim (preserving its
	// quoting); otherwise synthesize from the table name identifier.
	var aliasID any
	if a, ok := t["alias"].(map[string]any); ok {
		aliasID = deepClone(a)
	} else {
		aliasID = deepClone(t["name"])
		if schemaW != "" {
			// A derived table alias cannot be schema-qualified, so columns
			// written as schema.table.col must drop the schema.
			rc.strip[strings.ToLower(schemaW)+"\x00"+strings.ToLower(name)] = true
		}
	}

	// Inner table = original reference minus its alias (hints/partition/schema
	// travel with it into the subquery, and quoting is preserved verbatim).
	inner := deepCloneMap(v)
	inner["table"].(map[string]any)["alias"] = nil

	sub := deepCloneMap(rc.r.skeleton)
	sq := sub["subquery"].(map[string]any)
	sq["alias"] = aliasID

	innerSel := sq["this"].(map[string]any)["select"].(map[string]any)
	innerSel["from"].(map[string]any)["expressions"] = []any{inner}

	where := deepClone(rc.r.whereExpr)
	innerSel["where_clause"] = map[string]any{"this": where}

	for k := range v {
		delete(v, k)
	}
	for k, val := range sub {
		v[k] = val
	}
}

// applyStrip rewrites schema.table.col column references to table.col for every
// (schema, table) pair whose derived-table alias was synthesized from a bare
// name.
func (rc *rewriteCtx) applyStrip(node any) {
	if len(rc.strip) == 0 {
		return
	}
	stripDots(node, rc.strip)
}

func stripDots(node any, strip map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		if dot, ok := v["dot"].(map[string]any); ok {
			if this, ok := dot["this"].(map[string]any); ok {
				if col, ok := this["column"].(map[string]any); ok {
					schema := strings.ToLower(identName(col["table"]))
					table := strings.ToLower(identName(col["name"]))
					if schema != "" && table != "" && strip[schema+"\x00"+table] {
						newCol := map[string]any{"column": map[string]any{
							"name":              dot["field"], // real column identifier
							"table":             col["name"],  // table identifier
							"join_mark":         false,
							"trailing_comments": []any{},
						}}
						for k := range v {
							delete(v, k)
						}
						for k, val := range newCol {
							v[k] = val
						}
						return
					}
				}
			}
		}
		for _, child := range v {
			stripDots(child, strip)
		}
	case []any:
		for _, child := range v {
			stripDots(child, strip)
		}
	}
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

func deepClone(v any) any {
	b, _ := sonic.Marshal(v)
	var out any
	_ = sonic.Unmarshal(b, &out)
	return out
}

func deepCloneMap(v any) map[string]any {
	m, _ := deepClone(v).(map[string]any)
	return m
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
