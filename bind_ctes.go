package easysql

import (
	"fmt"
	"maps"
	"strings"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// CTEBinding binds Name to Query as a common table expression.
//
// Name is an identifier value, not SQL text. BindCTEs quotes it for the
// selected dialect when necessary. Query must contain exactly one SELECT or set
// operation.
type CTEBinding struct {
	Name  string
	Query string
}

// BindCTEOption configures BindCTEs.
type BindCTEOption func(*bindCTEConfig)

type bindCTEConfig struct {
	dialect string
}

// WithBindCTEDialect selects the SQL dialect used by BindCTEs: one of mysql,
// starrocks, postgres, or trino. The default is trino.
func WithBindCTEDialect(dialect string) BindCTEOption {
	return func(o *bindCTEConfig) { o.dialect = dialect }
}

// BindCTEs adds bindings to the root WITH clause of consumerSQL.
//
// Bindings retain their input order and are placed before CTEs already declared
// by consumerSQL. Consequently, each binding may reference an earlier binding,
// and existing consumer CTEs may reference any binding. A binding name that
// duplicates another binding or an existing root consumer CTE is rejected.
//
// Both consumerSQL and every binding query must contain exactly one SELECT or
// set operation. Composition happens on parsed ASTs rather than by interpolating
// SQL text. The generated result is normalized, comments are dropped, and the
// result is re-parsed before it is returned. Failures classify through ErrParse,
// ErrUnsupported, and ErrInternal.
//
// An empty binding list is a byte-preserving no-op and does not validate the
// SQL or options.
func BindCTEs(consumerSQL string, bindings []CTEBinding, opts ...BindCTEOption) (string, error) {
	if len(bindings) == 0 {
		return consumerSQL, nil
	}

	cfg := bindCTEConfig{dialect: "trino"}
	for i, opt := range opts {
		if opt == nil {
			return "", fmt.Errorf("easysql: CTE binding option %d must not be nil", i)
		}
		opt(&cfg)
	}
	if strings.TrimSpace(cfg.dialect) == "" {
		cfg.dialect = "trino"
	}
	pg, ok := dialectToPolyglot[cfg.dialect]
	if !ok {
		return "", fmt.Errorf("easysql: unknown dialect %q", cfg.dialect)
	}

	totalInputBytes := len(consumerSQL)
	if totalInputBytes > maxInputBytes {
		return "", fmt.Errorf("%w: combined CTE input too large (more than %d bytes)", ErrUnsupported, maxInputBytes)
	}
	normalized := make([]CTEBinding, len(bindings))
	bindingNames := make(map[string]string, len(bindings))
	for i, binding := range bindings {
		name := strings.TrimSpace(binding.Name)
		if name == "" {
			return "", fmt.Errorf("easysql: CTE binding %d name must not be empty", i)
		}
		if strings.TrimSpace(binding.Query) == "" {
			return "", fmt.Errorf("easysql: CTE binding %q query must not be empty", name)
		}
		if len(binding.Query) > maxInputBytes-totalInputBytes {
			return "", fmt.Errorf("%w: combined CTE input too large (more than %d bytes)", ErrUnsupported, maxInputBytes)
		}
		totalInputBytes += len(binding.Query)
		// Preserve the API's established duplicate rule: simple identifier values
		// compare like unquoted SQL names (case-insensitively), even though the
		// generator quotes mixed case below to preserve an accepted name exactly.
		key := normName(name, !simpleIdentRe.MatchString(name))
		if _, exists := bindingNames[key]; exists {
			return "", fmt.Errorf("easysql: duplicate CTE binding %q", name)
		}
		bindingNames[key] = name
		normalized[i] = CTEBinding{Name: name, Query: binding.Query}
	}

	client, err := defaultClient()
	if err != nil {
		return "", err
	}

	normalize := func(sql string) string { return sql }
	if cfg.dialect == "starrocks" {
		normalize = starrocksNormalizer
	}

	consumer, err := parseBoundQuery(client, normalize(consumerSQL), pg, "consumer")
	if err != nil {
		return "", err
	}
	consumerBody := queryBody(consumer)
	if consumerBody == nil {
		return "", fmt.Errorf("%w: consumer query has no query body", ErrInternal)
	}

	if with, _ := consumerBody["with"].(map[string]any); with != nil {
		if existing, ok := with["ctes"].([]any); ok {
			for _, entry := range existing {
				cte, _ := entry.(map[string]any)
				if cte == nil {
					continue
				}
				name := identName(cte["alias"])
				if name == "" {
					continue
				}
				key := normName(name, identQuoted(cte["alias"]))
				if bindingName, conflict := bindingNames[key]; conflict {
					return "", fmt.Errorf("easysql: CTE binding %q conflicts with consumer CTE %q", bindingName, name)
				}
			}
		}
	}

	queries := make([]map[string]any, len(normalized))
	for i, binding := range normalized {
		query, err := parseBoundQuery(client, normalize(binding.Query), pg, fmt.Sprintf("CTE binding %q", binding.Name))
		if err != nil {
			return "", err
		}
		queries[i] = query
	}

	withTemplate, cteTemplate, err := bindCTETemplates(client, pg)
	if err != nil {
		return "", err
	}
	boundCTEs := make([]any, len(normalized))
	for i, binding := range normalized {
		cte := maps.Clone(cteTemplate)
		cte["alias"] = newIdent(binding.Name, needsQuote(binding.Name))
		cte["this"] = queries[i]
		boundCTEs[i] = cte
	}

	if existingWith, _ := consumerBody["with"].(map[string]any); existingWith != nil {
		existingCTEs, ok := existingWith["ctes"].([]any)
		if !ok {
			return "", fmt.Errorf("%w: malformed consumer WITH clause", ErrInternal)
		}
		combined := make([]any, 0, len(boundCTEs)+len(existingCTEs))
		combined = append(combined, boundCTEs...)
		combined = append(combined, existingCTEs...)
		existingWith["ctes"] = combined
	} else {
		with := maps.Clone(withTemplate)
		with["ctes"] = boundCTEs
		consumerBody["with"] = with
	}

	stripASTComments(consumer)
	raw, err := sonic.Marshal([]any{consumer})
	if err != nil {
		return "", fmt.Errorf("%w: marshal bound query: %v", ErrInternal, err)
	}
	generated, err := client.Generate(raw, pg)
	if err != nil || len(generated) != 1 {
		return "", fmt.Errorf("%w: generate bound query: %v", ErrInternal, err)
	}
	out := generated[0]
	if err := guardInput(out); err != nil {
		return "", fmt.Errorf("easysql: generated bound SQL exceeds safe limits: %w", err)
	}
	if _, err := client.ParseOne(out, pg); err != nil {
		return "", fmt.Errorf("%w: bound SQL failed to re-parse: %v\nbound: %s", ErrInternal, err, out)
	}
	return out, nil
}

// parseBoundQuery parses and validates one query input for BindCTEs.
func parseBoundQuery(client *polyglot.Client, sql, pg, label string) (map[string]any, error) {
	if err := guardInput(sql); err != nil {
		return nil, err
	}
	raw, err := client.Parse(sql, pg)
	if err != nil {
		return nil, fmt.Errorf("invalid %s SQL: %w", label, classifyParseError(err))
	}
	var statements []any
	if err := sonic.Unmarshal(raw, &statements); err != nil {
		return nil, fmt.Errorf("%w: decode %s SQL: %v", ErrInternal, label, err)
	}
	statements = dropNils(statements)
	if len(statements) != 1 {
		return nil, fmt.Errorf("%w: %s must contain exactly one statement, got %d", ErrUnsupported, label, len(statements))
	}
	query, ok := statements[0].(map[string]any)
	if !ok || !isSupportedRoot(query) {
		return nil, fmt.Errorf("%w: %s must be a SELECT or set operation", ErrUnsupported, label)
	}
	return query, nil
}

// bindCTETemplates returns parser-produced WITH and CTE nodes so generated ASTs
// carry every field expected by the selected dialect's generator.
func bindCTETemplates(client *polyglot.Client, pg string) (map[string]any, map[string]any, error) {
	raw, err := client.ParseOne("WITH __easysql_cte__ AS (SELECT 1) SELECT 1", pg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: build CTE template: %v", ErrInternal, err)
	}
	var statement map[string]any
	if err := sonic.Unmarshal(raw, &statement); err != nil {
		return nil, nil, fmt.Errorf("%w: decode CTE template: %v", ErrInternal, err)
	}
	body := queryBody(statement)
	if body == nil {
		return nil, nil, fmt.Errorf("%w: malformed CTE template query", ErrInternal)
	}
	with, ok := body["with"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%w: malformed CTE template WITH clause", ErrInternal)
	}
	ctes, ok := with["ctes"].([]any)
	if !ok || len(ctes) != 1 {
		return nil, nil, fmt.Errorf("%w: malformed CTE template entries", ErrInternal)
	}
	cte, ok := ctes[0].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%w: malformed CTE template entry", ErrInternal)
	}
	return with, cte, nil
}
