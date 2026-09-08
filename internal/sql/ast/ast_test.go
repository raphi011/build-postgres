package ast

import (
	"testing"

	"github.com/raphi011/build-postgres/internal/tuple"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"a":        "a",
		"users":    "users",
		"_x1$":     "_x1$",
		"straße":   "straße",
		"日本":       "日本",
		"Users":    `"Users"`,
		"Über":     "Über",
		"ÜBER":     `"ÜBER"`,
		"a b":      `"a b"`,
		"1a":       `"1a"`,
		"$a":       `"$a"`,
		"":         `""`,
		`a"b`:      `"a""b"`,
		"select":   `"select"`,
		"from":     `"from"`,
		"selected": "selected",
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteString(t *testing.T) {
	cases := map[string]string{
		"":     "''",
		"abc":  "'abc'",
		"it's": "'it''s'",
		"'":    "''''",
		`a\b`:  `'a\b'`,
	}
	for in, want := range cases {
		if got := QuoteString(in); got != want {
			t.Errorf("QuoteString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestOpStrings(t *testing.T) {
	bin := map[BinOp]string{
		Eq: "=", Ne: "<>", Lt: "<", Le: "<=", Gt: ">", Ge: ">=",
		Add: "+", Sub: "-", Mul: "*", Div: "/", And: "AND", Or: "OR",
	}
	for op, want := range bin {
		if got := op.String(); got != want {
			t.Errorf("BinOp(%d).String() = %q, want %q", int(op), got, want)
		}
	}
	if Neg.String() != "-" || Not.String() != "NOT" {
		t.Errorf("UnOp strings: %q %q", Neg.String(), Not.String())
	}
}

// TestString builds trees by hand so the printer is checked independently
// of the parser.
func TestString(t *testing.T) {
	a := &ColumnRef{Name: "a"}
	one := &IntLit{Text: "1"}
	cases := []struct {
		node Node
		want string
	}{
		{&CreateTable{Name: "t", Columns: []ColumnDef{
			{Name: "id", Type: tuple.Int4, NotNull: true, PrimaryKey: true},
			{Name: "name", Type: tuple.Text},
		}}, "CREATE TABLE t (id int4 NOT NULL PRIMARY KEY, name text)"},
		{&DropTable{Name: "T"}, `DROP TABLE "T"`},
		{&CreateIndex{Name: "i", Table: "t", Column: "a"}, "CREATE INDEX i ON t (a)"},
		{&CreateIndex{Name: "I", Table: "t", Column: "a", Unique: true}, `CREATE UNIQUE INDEX "I" ON t (a)`},
		{&DropIndex{Name: "i"}, "DROP INDEX i"},
		{&Insert{Table: "t", Rows: [][]Expr{{one, &NullLit{}}}}, "INSERT INTO t VALUES (1, NULL)"},
		{&Insert{Table: "t", Columns: []string{"a"}, Rows: [][]Expr{{one}, {&StrLit{Value: "x"}}}},
			"INSERT INTO t (a) VALUES (1), ('x')"},
		{&Select{Items: []SelectItem{{Expr: &Star{}}}}, "SELECT *"},
		{&Select{
			Items: []SelectItem{{Expr: a, Alias: "b"}},
			From: []TableExpr{&Join{
				Left:  &TableRef{Name: "t", Alias: "x"},
				Right: &TableRef{Name: "u"},
				On:    &BinaryExpr{Op: Eq, Left: &ColumnRef{Table: "x", Name: "id"}, Right: &ColumnRef{Table: "u", Name: "id"}},
			}},
			Where:   &IsNull{X: a, Not: true},
			OrderBy: []OrderItem{{Expr: a, Desc: true}, {Expr: one}},
			Limit:   one,
		}, "SELECT a AS b FROM t AS x JOIN u ON (x.id = u.id) WHERE (a IS NOT NULL) ORDER BY a DESC, 1 LIMIT 1"},
		{&Update{Table: "t", Set: []Assignment{{Column: "a", Value: one}}, Where: &BoolLit{Value: true}},
			"UPDATE t SET a = 1 WHERE TRUE"},
		{&Delete{Table: "t"}, "DELETE FROM t"},
		{&Begin{}, "BEGIN"},
		{&Commit{}, "COMMIT"},
		{&Rollback{}, "ROLLBACK"},
		{&Explain{Stmt: &Delete{Table: "t", Where: &BoolLit{}}}, "EXPLAIN DELETE FROM t WHERE FALSE"},
		{&Explain{Stmt: &Select{Items: []SelectItem{{Expr: one}}}, CostsOff: true}, "EXPLAIN (COSTS OFF) SELECT 1"},
		{&Analyze{}, "ANALYZE"},
		{&Analyze{Table: "T"}, `ANALYZE "T"`},
		{&UnaryExpr{Op: Neg, X: &UnaryExpr{Op: Not, X: a}}, "(-(NOT a))"},
		{&BinaryExpr{Op: And, Left: &BinaryExpr{Op: Or, Left: a, Right: a}, Right: &IsNull{X: one}},
			"((a OR a) AND (1 IS NULL))"},
	}
	for _, c := range cases {
		if got := c.node.String(); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
