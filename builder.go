package easysql

import (
	"fmt"

	"github.com/bytedance/sonic"
	polyglot "github.com/tobilg/polyglot/packages/go"
)

// buildSubquery evaluates a Polyglot 0.9 immutable builder plan and returns the
// inner subquery payload from the resulting {"subquery": ...} AST node. Keeping
// this boundary in one place makes the native builder protocol responsible for
// constructing generator-compatible nodes while the rewrite code only performs
// the scope-aware substitutions that Polyglot's generic builder cannot express.
func buildSubquery(client *polyglot.Client, expression polyglot.Expression, context string) (map[string]any, error) {
	raw, err := client.Build(expression)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInternal, context, err)
	}
	var node map[string]any
	if err := sonic.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrInternal, context, err)
	}
	subquery, ok := node["subquery"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s returned a non-subquery AST", ErrInternal, context)
	}
	return subquery, nil
}
