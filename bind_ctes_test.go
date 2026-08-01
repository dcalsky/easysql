package easysql

import (
	"errors"
	"strings"
	"testing"
)

func bindCTEsValid(t *testing.T, dialect, consumer string, bindings []CTEBinding) string {
	t.Helper()
	out, err := BindCTEs(consumer, bindings, WithBindCTEDialect(dialect))
	if err != nil {
		t.Fatalf("BindCTEs(%q): %v", consumer, err)
	}
	if _, err := testClient.ParseOne(out, dialectToPolyglot[dialect]); err != nil {
		t.Fatalf("bound SQL does not parse:\n out: %s\n err: %v", out, err)
	}
	return out
}

func TestBindCTEsBasic(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`SELECT * FROM foo WHERE amount > 100`,
		[]CTEBinding{{
			Name:  "foo",
			Query: `SELECT order_id, amount FROM orders WHERE status = 'paid'`,
		}},
	)

	const want = "WITH foo AS (SELECT order_id, amount FROM orders WHERE status = 'paid') SELECT * FROM foo WHERE amount > 100"
	if out != want {
		t.Fatalf("unexpected binding:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsJoinQueries(t *testing.T) {
	tests := []struct {
		name     string
		consumer string
		bindings []CTEBinding
		want     string
	}{
		{
			name: "consumer joins binding with physical table",
			consumer: `SELECT f.order_id, c.name
				FROM foo f
				JOIN customers c ON f.customer_id = c.id
				WHERE f.amount > 100`,
			bindings: []CTEBinding{{
				Name:  "foo",
				Query: `SELECT order_id, customer_id, amount FROM orders`,
			}},
			want: "WITH foo AS (SELECT order_id, customer_id, amount FROM orders) SELECT f.order_id, c.name FROM foo AS f JOIN customers AS c ON f.customer_id = c.id WHERE f.amount > 100",
		},
		{
			name: "consumer self joins binding",
			consumer: `SELECT employee.name, manager.name AS manager_name
				FROM foo employee
				LEFT JOIN foo manager ON employee.manager_id = manager.employee_id`,
			bindings: []CTEBinding{{
				Name:  "foo",
				Query: `SELECT employee_id, manager_id, name FROM employees`,
			}},
			want: "WITH foo AS (SELECT employee_id, manager_id, name FROM employees) SELECT employee.name, manager.name AS manager_name FROM foo AS employee LEFT JOIN foo AS manager ON employee.manager_id = manager.employee_id",
		},
		{
			name:     "binding query contains join",
			consumer: `SELECT * FROM foo WHERE customer_name IS NOT NULL`,
			bindings: []CTEBinding{{
				Name: "foo",
				Query: `SELECT o.order_id, c.name AS customer_name
					FROM orders o
					LEFT JOIN customers c ON o.customer_id = c.id`,
			}},
			want: "WITH foo AS (SELECT o.order_id, c.name AS customer_name FROM orders AS o LEFT JOIN customers AS c ON o.customer_id = c.id) SELECT * FROM foo WHERE customer_name IS NOT NULL",
		},
		{
			name: "consumer joins two bindings",
			consumer: `SELECT o.order_id, o.amount, p.paid_amount
				FROM orders_view o
				LEFT JOIN payments_view p ON o.order_id = p.order_id`,
			bindings: []CTEBinding{
				{Name: "orders_view", Query: `SELECT order_id, amount FROM orders`},
				{Name: "payments_view", Query: `SELECT order_id, paid_amount FROM payments`},
			},
			want: "WITH orders_view AS (SELECT order_id, amount FROM orders), payments_view AS (SELECT order_id, paid_amount FROM payments) SELECT o.order_id, o.amount, p.paid_amount FROM orders_view AS o LEFT JOIN payments_view AS p ON o.order_id = p.order_id",
		},
		{
			name:     "dependent binding joins earlier binding",
			consumer: `SELECT * FROM enriched`,
			bindings: []CTEBinding{
				{Name: "foo", Query: `SELECT id, dimension_id FROM source_table`},
				{
					Name: "enriched",
					Query: `SELECT f.id, d.label
						FROM foo f
						JOIN dimensions d ON f.dimension_id = d.id`,
				},
			},
			want: "WITH foo AS (SELECT id, dimension_id FROM source_table), enriched AS (SELECT f.id, d.label FROM foo AS f JOIN dimensions AS d ON f.dimension_id = d.id) SELECT * FROM enriched",
		},
		{
			name: "join lives in existing consumer CTE",
			consumer: `WITH matched AS (
					SELECT f.id, d.label
					FROM foo f
					CROSS JOIN dimensions d
				)
				SELECT * FROM matched`,
			bindings: []CTEBinding{{
				Name:  "foo",
				Query: `SELECT id FROM source_table`,
			}},
			want: "WITH foo AS (SELECT id FROM source_table), matched AS (SELECT f.id, d.label FROM foo AS f CROSS JOIN dimensions AS d) SELECT * FROM matched",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := bindCTEsValid(t, "trino", tt.consumer, tt.bindings)
			if out != tt.want {
				t.Fatalf("unexpected join binding:\nwant: %s\n got: %s", tt.want, out)
			}
		})
	}
}

func TestBindCTEsJoinAcrossDialects(t *testing.T) {
	wantByDialect := map[string]string{
		"mysql":     "WITH foo AS (SELECT id, owner_id FROM source_table) SELECT f.id, u.name FROM foo AS f JOIN users AS u ON f.owner_id = u.id",
		"starrocks": "WITH foo AS (SELECT id, owner_id FROM source_table) SELECT f.id, u.name FROM foo f JOIN users u ON f.owner_id = u.id",
		"postgres":  "WITH foo AS (SELECT id, owner_id FROM source_table) SELECT f.id, u.name FROM foo AS f JOIN users AS u ON f.owner_id = u.id",
		"trino":     "WITH foo AS (SELECT id, owner_id FROM source_table) SELECT f.id, u.name FROM foo AS f JOIN users AS u ON f.owner_id = u.id",
	}
	for _, dialect := range []string{"mysql", "starrocks", "postgres", "trino"} {
		t.Run(dialect, func(t *testing.T) {
			out := bindCTEsValid(t, dialect,
				`SELECT f.id, u.name FROM foo f JOIN users u ON f.owner_id = u.id`,
				[]CTEBinding{{Name: "foo", Query: `SELECT id, owner_id FROM source_table`}},
			)
			want := wantByDialect[dialect]
			if out != want {
				t.Fatalf("unexpected %s join binding:\nwant: %s\n got: %s", dialect, want, out)
			}
		})
	}
}

func TestBindCTEsAdditionalQueryShapes(t *testing.T) {
	tests := []struct {
		name     string
		consumer string
		binding  CTEBinding
		want     string
	}{
		{
			name: `aggregate having order and limit`,
			consumer: `SELECT customer_id, SUM(amount) AS total_amount
				FROM foo
				GROUP BY customer_id
				HAVING SUM(amount) > 100
				ORDER BY total_amount DESC
				LIMIT 10`,
			binding: CTEBinding{Name: "foo", Query: `SELECT customer_id, amount FROM orders`},
			want:    "WITH foo AS (SELECT customer_id, amount FROM orders) SELECT customer_id, SUM(amount) AS total_amount FROM foo GROUP BY customer_id HAVING SUM(amount) > 100 ORDER BY total_amount DESC LIMIT 10",
		},
		{
			name:     `binding is set operation`,
			consumer: `SELECT id FROM foo WHERE id > 0`,
			binding: CTEBinding{
				Name:  "foo",
				Query: `SELECT id FROM live_records UNION ALL SELECT id FROM archived_records`,
			},
			want: "WITH foo AS (SELECT id FROM live_records UNION ALL SELECT id FROM archived_records) SELECT id FROM foo WHERE id > 0",
		},
		{
			name: `correlated exists references binding`,
			consumer: `SELECT f.order_id
				FROM foo f
				WHERE EXISTS (
					SELECT 1 FROM refunds r WHERE r.order_id = f.order_id
				)`,
			binding: CTEBinding{Name: "foo", Query: `SELECT order_id FROM orders`},
			want:    "WITH foo AS (SELECT order_id FROM orders) SELECT f.order_id FROM foo AS f WHERE EXISTS(SELECT 1 FROM refunds AS r WHERE r.order_id = f.order_id)",
		},
		{
			name:     `binding inside derived table`,
			consumer: `SELECT q.id FROM (SELECT id FROM foo WHERE id > 0) q`,
			binding:  CTEBinding{Name: "foo", Query: `SELECT id FROM source_table`},
			want:     "WITH foo AS (SELECT id FROM source_table) SELECT q.id FROM (SELECT id FROM foo WHERE id > 0) AS q",
		},
		{
			name:     `window function in binding`,
			consumer: `SELECT id FROM foo WHERE row_num = 1`,
			binding: CTEBinding{
				Name: "foo",
				Query: `SELECT id,
					ROW_NUMBER() OVER (PARTITION BY owner_id ORDER BY created_at DESC) AS row_num
					FROM events`,
			},
			want: "WITH foo AS (SELECT id, ROW_NUMBER() OVER (PARTITION BY owner_id ORDER BY created_at DESC) AS row_num FROM events) SELECT id FROM foo WHERE row_num = 1",
		},
		{
			name:     `binding may read same named physical table`,
			consumer: `SELECT id FROM foo`,
			binding:  CTEBinding{Name: "foo", Query: `SELECT id FROM foo WHERE active = TRUE`},
			want:     "WITH foo AS (SELECT id FROM foo WHERE active = TRUE) SELECT id FROM foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := bindCTEsValid(t, "trino", tt.consumer, []CTEBinding{tt.binding})
			if out != tt.want {
				t.Fatalf("unexpected query-shape binding:\nwant: %s\n got: %s", tt.want, out)
			}
		})
	}
}

func TestBindCTEsDoesNotShadowQualifiedPhysicalTable(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`SELECT * FROM foo.bar WHERE total_amount > 1000`,
		[]CTEBinding{{
			Name: "bar",
			Query: `
				WITH paid_orders AS (
					SELECT customer_id, amount
					FROM orders
					WHERE status = 'paid'
				)
				SELECT customer_id, SUM(amount) AS total_amount
				FROM paid_orders
				GROUP BY customer_id`,
		}},
	)

	const want = "WITH bar AS (WITH paid_orders AS (SELECT customer_id, amount FROM orders WHERE status = 'paid') SELECT customer_id, SUM(amount) AS total_amount FROM paid_orders GROUP BY customer_id) SELECT * FROM foo.bar WHERE total_amount > 1000"
	if out != want {
		t.Fatalf("qualified physical table must not be shadowed by a bare CTE name:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsPreservesSourceWithClause(t *testing.T) {
	const want = "WITH foo AS (WITH paid AS (SELECT order_id, amount FROM orders WHERE status = 'paid') SELECT order_id, amount FROM paid) SELECT * FROM foo"
	for _, dialect := range []string{"mysql", "starrocks", "postgres", "trino"} {
		t.Run(dialect, func(t *testing.T) {
			out := bindCTEsValid(t, dialect,
				`SELECT * FROM foo`,
				[]CTEBinding{{
					Name: "foo",
					Query: `WITH paid AS (
						SELECT order_id, amount FROM orders WHERE status = 'paid'
					)
					SELECT order_id, amount FROM paid`,
				}},
			)
			if out != want {
				t.Fatalf("source WITH was not preserved:\nwant: %s\n got: %s", want, out)
			}
		})
	}
}

func TestBindCTEsPrependsBindingsToConsumerWithClause(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`WITH large_orders AS (
			SELECT order_id FROM foo WHERE amount > 100
		)
		SELECT * FROM large_orders`,
		[]CTEBinding{{
			Name:  "foo",
			Query: `SELECT order_id, amount FROM orders`,
		}},
	)

	const want = "WITH foo AS (SELECT order_id, amount FROM orders), large_orders AS (SELECT order_id FROM foo WHERE amount > 100) SELECT * FROM large_orders"
	if out != want {
		t.Fatalf("binding must precede dependent consumer CTEs:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsPreservesRecursiveConsumerWithClause(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`WITH RECURSIVE seq AS (
			SELECT 1 AS n
			UNION ALL
			SELECT n + 1 FROM seq WHERE n < 3
		)
		SELECT foo.id, seq.n FROM foo CROSS JOIN seq`,
		[]CTEBinding{{Name: "foo", Query: `SELECT id FROM source_table`}},
	)

	const want = "WITH RECURSIVE foo AS (SELECT id FROM source_table), seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 3) SELECT foo.id, seq.n FROM foo CROSS JOIN seq"
	if out != want {
		t.Fatalf("recursive consumer WITH changed:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsKeepsBindingOrder(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`SELECT * FROM bar`,
		[]CTEBinding{
			{Name: "foo", Query: `SELECT id FROM source_table`},
			{Name: "bar", Query: `SELECT id FROM foo WHERE id > 0`},
		},
	)

	const want = "WITH foo AS (SELECT id FROM source_table), bar AS (SELECT id FROM foo WHERE id > 0) SELECT * FROM bar"
	if out != want {
		t.Fatalf("binding order changed:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsHandlesSetOperationConsumer(t *testing.T) {
	out := bindCTEsValid(t, "trino",
		`SELECT id FROM foo UNION ALL SELECT id FROM foo`,
		[]CTEBinding{{Name: "foo", Query: `SELECT id FROM source_table`}},
	)

	const want = "WITH foo AS (SELECT id FROM source_table) SELECT id FROM foo UNION ALL SELECT id FROM foo"
	if out != want {
		t.Fatalf("binding was not attached to set operation root:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsDoesNotInterpolateQueryText(t *testing.T) {
	out := bindCTEsValid(t, "postgres",
		`SELECT * FROM foo`,
		[]CTEBinding{{
			Name:  "foo",
			Query: "SELECT '); DROP TABLE secret; --' AS txt FROM source_table -- trailing comment",
		}},
	)

	const want = "WITH foo AS (SELECT '); DROP TABLE secret; --' AS txt FROM source_table) SELECT * FROM foo"
	if out != want {
		t.Fatalf("query text was not composed structurally:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsQuotesBindingName(t *testing.T) {
	out := bindCTEsValid(t, "postgres",
		`SELECT * FROM "daily orders"`,
		[]CTEBinding{{Name: "daily orders", Query: `SELECT id FROM orders`}},
	)

	const want = `WITH "daily orders" AS (SELECT id FROM orders) SELECT * FROM "daily orders"`
	if out != want {
		t.Fatalf("binding name was not quoted safely:\nwant: %s\n got: %s", want, out)
	}
}

func TestBindCTEsSupportsAllRewriteDialects(t *testing.T) {
	for _, dialect := range []string{"mysql", "starrocks", "postgres", "trino"} {
		t.Run(dialect, func(t *testing.T) {
			out := bindCTEsValid(t, dialect,
				`SELECT * FROM foo`,
				[]CTEBinding{{Name: "foo", Query: `SELECT id FROM source_table`}},
			)
			const want = "WITH foo AS (SELECT id FROM source_table) SELECT * FROM foo"
			if out != want {
				t.Fatalf("unexpected %s output:\nwant: %s\n got: %s", dialect, want, out)
			}
		})
	}
}

func TestBindCTEsNoBindingsPreservesInput(t *testing.T) {
	input := "  not even SQL  "
	out, err := BindCTEs(input, nil, WithBindCTEDialect("not-a-dialect"))
	if err != nil {
		t.Fatalf("empty binding list must be a no-op: %v", err)
	}
	if out != input {
		t.Fatalf("no-op changed input bytes: want %q, got %q", input, out)
	}
}

func TestBindCTEsRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		consumer string
		bindings []CTEBinding
		opts     []BindCTEOption
		contains string
	}{
		{
			name:     "empty name",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Query: `SELECT 1`}},
			contains: "name must not be empty",
		},
		{
			name:     "empty query",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo"}},
			contains: `binding "foo" query must not be empty`,
		},
		{
			name:     "duplicate binding",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{
				{Name: "foo", Query: `SELECT 1`},
				{Name: "FOO", Query: `SELECT 2`},
			},
			contains: `duplicate CTE binding "FOO"`,
		},
		{
			name:     "consumer collision",
			consumer: `WITH foo AS (SELECT 1) SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 2`}},
			contains: `CTE binding "foo" conflicts with consumer CTE`,
		},
		{
			name:     "unknown dialect",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 1`}},
			opts:     []BindCTEOption{WithBindCTEDialect("oracle")},
			contains: `unknown dialect "oracle"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BindCTEs(tt.consumer, tt.bindings, tt.opts...)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("want error containing %q, got %v", tt.contains, err)
			}
		})
	}
}

func TestBindCTEsClassifiesSQLFailures(t *testing.T) {
	tests := []struct {
		name     string
		consumer string
		bindings []CTEBinding
		target   error
	}{
		{
			name:     "invalid consumer",
			consumer: `SELECT FROM`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 1`}},
			target:   ErrParse,
		},
		{
			name:     "invalid binding query",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT FROM`}},
			target:   ErrParse,
		},
		{
			name:     "multiple consumer statements",
			consumer: `SELECT * FROM foo; SELECT 2`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 1`}},
			target:   ErrUnsupported,
		},
		{
			name:     "multiple binding statements",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 1; SELECT 2`}},
			target:   ErrUnsupported,
		},
		{
			name:     "non-query consumer",
			consumer: `DELETE FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `SELECT 1`}},
			target:   ErrUnsupported,
		},
		{
			name:     "non-query binding",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: `DELETE FROM source_table`}},
			target:   ErrUnsupported,
		},
		{
			name:     "combined input too large",
			consumer: `SELECT * FROM foo`,
			bindings: []CTEBinding{{Name: "foo", Query: strings.Repeat("x", maxInputBytes)}},
			target:   ErrUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BindCTEs(tt.consumer, tt.bindings, WithBindCTEDialect("trino"))
			if !errors.Is(err, tt.target) {
				t.Fatalf("want %v, got %v", tt.target, err)
			}
		})
	}
}
