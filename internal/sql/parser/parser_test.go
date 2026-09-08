package parser

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
)

// at builds a position for expected values.
func at(offset, line, column int) lexer.Pos {
	return lexer.Pos{Offset: offset, Line: line, Column: column}
}

// parseOne parses src, which must hold exactly one statement.
func parseOne(t *testing.T, src string) ast.Stmt {
	t.Helper()
	stmts, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("Parse(%q): %d statements, want 1", src, len(stmts))
	}
	return stmts[0]
}

// golden pairs a statement with its canonical rendering. Every case is
// also re-parsed from its rendering in TestRoundTrip.
var golden = []struct{ src, want string }{
	// CREATE TABLE
	{"create table t (a int4)", "CREATE TABLE t (a int4)"},
	{"CREATE TABLE T (A INT)", "CREATE TABLE t (a int4)"},
	{"create table t (a integer, b int8, c bigint, d bool, e boolean, f text)",
		"CREATE TABLE t (a int4, b int8, c int8, d bool, e bool, f text)"},
	{"create table t (id int4 primary key, name text not null)",
		"CREATE TABLE t (id int4 PRIMARY KEY, name text NOT NULL)"},
	{"create table t (id int4 not null primary key)", "CREATE TABLE t (id int4 NOT NULL PRIMARY KEY)"},
	{"create table t (id int4 primary key not null)", "CREATE TABLE t (id int4 NOT NULL PRIMARY KEY)"},
	{`create table "Users" ("Id" int4, "select" text)`, `CREATE TABLE "Users" ("Id" int4, "select" text)`},
	// DROP TABLE
	{"drop table t", "DROP TABLE t"},
	{"DROP TABLE \"T\"", `DROP TABLE "T"`},
	// CREATE INDEX, DROP INDEX
	{"create index i on t (a)", "CREATE INDEX i ON t (a)"},
	{"CREATE UNIQUE INDEX I ON T (A)", "CREATE UNIQUE INDEX i ON t (a)"},
	{`create index "I" on "T" ("A")`, `CREATE INDEX "I" ON "T" ("A")`},
	{"drop index i", "DROP INDEX i"},
	// INSERT
	{"insert into t values (1)", "INSERT INTO t VALUES (1)"},
	{"insert into t (a, b) values (1, 'x')", "INSERT INTO t (a, b) VALUES (1, 'x')"},
	{"insert into t values (1, 'x'), (-2, null), (true, false)",
		"INSERT INTO t VALUES (1, 'x'), ((-2), NULL), (TRUE, FALSE)"},
	{"insert into t (a) values (1 + 2 * 3)", "INSERT INTO t (a) VALUES ((1 + (2 * 3)))"},
	{"insert into t values ('it''s')", "INSERT INTO t VALUES ('it''s')"},
	// SELECT
	{"select 1", "SELECT 1"},
	{"select 1, 'a', true, null", "SELECT 1, 'a', TRUE, NULL"},
	{"select * from t", "SELECT * FROM t"},
	{"select *, a from t", "SELECT *, a FROM t"},
	{"select a, b as c, d e from t", "SELECT a, b AS c, d AS e FROM t"},
	{"select t.a, u.b from t, u", "SELECT t.a, u.b FROM t, u"},
	{"select a from t as u", "SELECT a FROM t AS u"},
	{"select a from t u", "SELECT a FROM t AS u"},
	{"select a from t where a = 1", "SELECT a FROM t WHERE (a = 1)"},
	{"select a from t order by a", "SELECT a FROM t ORDER BY a"},
	{"select a from t order by a asc", "SELECT a FROM t ORDER BY a"},
	{"select a from t order by a desc, b", "SELECT a FROM t ORDER BY a DESC, b"},
	{"select a from t order by a + 1", "SELECT a FROM t ORDER BY (a + 1)"},
	{"select a from t limit 10", "SELECT a FROM t LIMIT 10"},
	{"select a from t where b > 1 order by c desc limit 5",
		"SELECT a FROM t WHERE (b > 1) ORDER BY c DESC LIMIT 5"},
	{"select a from t join u on t.id = u.id", "SELECT a FROM t JOIN u ON (t.id = u.id)"},
	{"select a from t as x join u as y on x.id = y.id join v on v.id = y.id",
		"SELECT a FROM t AS x JOIN u AS y ON (x.id = y.id) JOIN v ON (v.id = y.id)"},
	{"select a from t join u on t.id = u.id, v", "SELECT a FROM t JOIN u ON (t.id = u.id), v"},
	{"select a from t, u join v on u.id = v.id", "SELECT a FROM t, u JOIN v ON (u.id = v.id)"},
	{"select 1 where false", "SELECT 1 WHERE FALSE"},
	{`select "A", "T"."b" from "T"`, `SELECT "A", "T".b FROM "T"`},
	{"select a from t where s = 'o''brien'", "SELECT a FROM t WHERE (s = 'o''brien')"},
	// UPDATE
	{"update t set a = 1", "UPDATE t SET a = 1"},
	{"update t set a = 1, b = a + 1 where id = 3", "UPDATE t SET a = 1, b = (a + 1) WHERE (id = 3)"},
	{"update t set a = null where b is null", "UPDATE t SET a = NULL WHERE (b IS NULL)"},
	// DELETE
	{"delete from t", "DELETE FROM t"},
	{"delete from t where id = 3", "DELETE FROM t WHERE (id = 3)"},
	// Transactions
	{"begin", "BEGIN"},
	{"commit", "COMMIT"},
	{"rollback", "ROLLBACK"},
	// EXPLAIN
	{"explain select a from t", "EXPLAIN SELECT a FROM t"},
	{"explain insert into t values (1)", "EXPLAIN INSERT INTO t VALUES (1)"},
	{"explain update t set a = 1", "EXPLAIN UPDATE t SET a = 1"},
	{"explain delete from t", "EXPLAIN DELETE FROM t"},
	// Case and whitespace
	{"SeLeCt A FrOm T", "SELECT a FROM t"},
	{"select\n\ta\nfrom\tt -- comment\nwhere /* c */ a = 1", "SELECT a FROM t WHERE (a = 1)"},
	{"select a from t;", "SELECT a FROM t"},
}

func TestGolden(t *testing.T) {
	for _, c := range golden {
		got := parseOne(t, c.src).String()
		if got != c.want {
			t.Errorf("Parse(%q)\n got %s\nwant %s", c.src, got, c.want)
		}
	}
}

// exprGolden pairs an expression with its fully parenthesised rendering,
// which pins down precedence and associativity.
var exprGolden = []struct{ src, want string }{
	{"1", "1"},
	{"a", "a"},
	{"t.a", "t.a"},
	{"'x'", "'x'"},
	{"true", "TRUE"},
	{"null", "NULL"},
	{"(1)", "1"},
	{"((a))", "a"},
	// Arithmetic
	{"1 + 2", "(1 + 2)"},
	{"1 + 2 + 3", "((1 + 2) + 3)"},
	{"1 - 2 - 3", "((1 - 2) - 3)"},
	{"1 + 2 * 3", "(1 + (2 * 3))"},
	{"1 * 2 + 3", "((1 * 2) + 3)"},
	{"(1 + 2) * 3", "((1 + 2) * 3)"},
	{"1 * 2 / 3", "((1 * 2) / 3)"},
	{"8 / 4 / 2", "((8 / 4) / 2)"},
	// Unary minus binds tightest
	{"-1", "(-1)"},
	{"-a", "(-a)"},
	{"- - a", "(-(-a))"},
	{"-a * b", "((-a) * b)"},
	{"-a + b", "((-a) + b)"},
	{"a * -b", "(a * (-b))"},
	{"-(a + b)", "(-(a + b))"},
	{"-a is null", "((-a) IS NULL)"},
	{"1 - -1", "(1 - (-1))"},
	// Comparison below arithmetic
	{"a = b", "(a = b)"},
	{"a <> b", "(a <> b)"},
	{"a != b", "(a <> b)"},
	{"a < b", "(a < b)"},
	{"a <= b", "(a <= b)"},
	{"a > b", "(a > b)"},
	{"a >= b", "(a >= b)"},
	{"a + 1 = b * 2", "((a + 1) = (b * 2))"},
	// IS below comparison; postfix, so it chains
	{"a is null", "(a IS NULL)"},
	{"a is not null", "(a IS NOT NULL)"},
	{"a + 1 is null", "((a + 1) IS NULL)"},
	{"a = b is null", "((a = b) IS NULL)"},
	{"a = (b is null)", "(a = (b IS NULL))"},
	{"a is null = b", "((a IS NULL) = b)"},
	{"a is null is null", "((a IS NULL) IS NULL)"},
	{"a is null is not null", "((a IS NULL) IS NOT NULL)"},
	// NOT below IS and comparison, above AND
	{"not a", "(NOT a)"},
	{"not not a", "(NOT (NOT a))"},
	{"not a = b", "(NOT (a = b))"},
	{"not a is null", "(NOT (a IS NULL))"},
	{"not a and b", "((NOT a) AND b)"},
	{"a and not b", "(a AND (NOT b))"},
	{"not (a and b)", "(NOT (a AND b))"},
	// AND above OR, both left-associative
	{"a and b", "(a AND b)"},
	{"a or b", "(a OR b)"},
	{"a or b and c", "(a OR (b AND c))"},
	{"a and b or c", "((a AND b) OR c)"},
	{"a or b or c", "((a OR b) OR c)"},
	{"a and b and c", "((a AND b) AND c)"},
	{"(a or b) and c", "((a OR b) AND c)"},
	{"a or not b and c", "(a OR ((NOT b) AND c))"},
	{"a = 1 and b = 2 or c = 3", "(((a = 1) AND (b = 2)) OR (c = 3))"},
	{"a = 1 or b = 2 and not c is null", "((a = 1) OR ((b = 2) AND (NOT (c IS NULL))))"},
	// Mixed
	{"x + y * 2 > 10 and name <> 'x' or flag", "((((x + (y * 2)) > 10) AND (name <> 'x')) OR flag)"},
	{"t.a = u.b and t.c is not null", "((t.a = u.b) AND (t.c IS NOT NULL))"},
	{"1 + (2 + 3)", "(1 + (2 + 3))"},
	{"007", "007"},
	{"'a''b'", "'a''b'"},
	{`"Col"`, `"Col"`},
	{`"select"`, `"select"`},
}

func TestExprGolden(t *testing.T) {
	for _, c := range exprGolden {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Errorf("ParseExpr(%q): %v", c.src, err)
			continue
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseExpr(%q)\n got %s\nwant %s", c.src, got, c.want)
		}
	}
}

// TestRoundTrip checks that the canonical rendering parses back to itself.
func TestRoundTrip(t *testing.T) {
	for _, c := range golden {
		got := parseOne(t, c.want).String()
		if got != c.want {
			t.Errorf("Parse(%q) renders as %q", c.want, got)
		}
	}
	for _, c := range exprGolden {
		e, err := ParseExpr(c.want)
		if err != nil {
			t.Errorf("ParseExpr(%q): %v", c.want, err)
			continue
		}
		if got := e.String(); got != c.want {
			t.Errorf("ParseExpr(%q) renders as %q", c.want, got)
		}
	}
}

func TestTree(t *testing.T) {
	s := parseOne(t, "select a, t.b as c from t as x join u on x.id = u.id where a > 1 order by b desc limit 3")
	sel, ok := s.(*ast.Select)
	if !ok {
		t.Fatalf("got %T, want *ast.Select", s)
	}
	if len(sel.Items) != 2 {
		t.Fatalf("%d items", len(sel.Items))
	}
	if c, ok := sel.Items[0].Expr.(*ast.ColumnRef); !ok || c.Table != "" || c.Name != "a" || sel.Items[0].Alias != "" {
		t.Errorf("item 0: %#v alias %q", sel.Items[0].Expr, sel.Items[0].Alias)
	}
	if c, ok := sel.Items[1].Expr.(*ast.ColumnRef); !ok || c.Table != "t" || c.Name != "b" || sel.Items[1].Alias != "c" {
		t.Errorf("item 1: %#v alias %q", sel.Items[1].Expr, sel.Items[1].Alias)
	}
	if len(sel.From) != 1 {
		t.Fatalf("%d from items", len(sel.From))
	}
	j, ok := sel.From[0].(*ast.Join)
	if !ok {
		t.Fatalf("from: %T", sel.From[0])
	}
	if l, ok := j.Left.(*ast.TableRef); !ok || l.Name != "t" || l.Alias != "x" {
		t.Errorf("left: %#v", j.Left)
	}
	if r, ok := j.Right.(*ast.TableRef); !ok || r.Name != "u" || r.Alias != "" {
		t.Errorf("right: %#v", j.Right)
	}
	if on, ok := j.On.(*ast.BinaryExpr); !ok || on.Op != ast.Eq {
		t.Errorf("on: %#v", j.On)
	}
	if w, ok := sel.Where.(*ast.BinaryExpr); !ok || w.Op != ast.Gt {
		t.Errorf("where: %#v", sel.Where)
	}
	if len(sel.OrderBy) != 1 || !sel.OrderBy[0].Desc {
		t.Errorf("order by: %#v", sel.OrderBy)
	}
	if l, ok := sel.Limit.(*ast.IntLit); !ok || l.Text != "3" {
		t.Errorf("limit: %#v", sel.Limit)
	}
}

func TestInsertColumns(t *testing.T) {
	ins := parseOne(t, "insert into t values (1)").(*ast.Insert)
	if ins.Columns != nil {
		t.Errorf("Columns = %#v, want nil when omitted", ins.Columns)
	}
	ins = parseOne(t, "insert into t (a) values (1), (2)").(*ast.Insert)
	if len(ins.Columns) != 1 || ins.Columns[0] != "a" || len(ins.Rows) != 2 {
		t.Errorf("got %#v", ins)
	}
}

func TestOptionalClausesAreNil(t *testing.T) {
	sel := parseOne(t, "select 1").(*ast.Select)
	if sel.From != nil || sel.Where != nil || sel.OrderBy != nil || sel.Limit != nil {
		t.Errorf("got %#v", sel)
	}
	upd := parseOne(t, "update t set a = 1").(*ast.Update)
	if upd.Where != nil {
		t.Errorf("update where: %#v", upd.Where)
	}
}

func TestPositions(t *testing.T) {
	sel := parseOne(t, "select a + b, -c\nfrom t where t.x is null").(*ast.Select)
	plus := sel.Items[0].Expr.(*ast.BinaryExpr)
	if plus.Pos() != at(9, 1, 10) {
		t.Errorf("+ at %+v", plus.Pos())
	}
	if plus.Left.Pos() != at(7, 1, 8) || plus.Right.Pos() != at(11, 1, 12) {
		t.Errorf("operands at %+v and %+v", plus.Left.Pos(), plus.Right.Pos())
	}
	neg := sel.Items[1].Expr.(*ast.UnaryExpr)
	if neg.Pos() != at(14, 1, 15) || neg.X.Pos() != at(15, 1, 16) {
		t.Errorf("- at %+v, operand at %+v", neg.Pos(), neg.X.Pos())
	}
	ref := sel.From[0].(*ast.TableRef)
	if ref.Loc != at(22, 2, 6) {
		t.Errorf("table at %+v", ref.Loc)
	}
	isNull := sel.Where.(*ast.IsNull)
	if isNull.Pos() != at(34, 2, 18) || isNull.X.Pos() != at(30, 2, 14) {
		t.Errorf("IS at %+v, operand at %+v", isNull.Pos(), isNull.X.Pos())
	}
}

func TestTableNamePositions(t *testing.T) {
	if ins := parseOne(t, "insert into t values (1)").(*ast.Insert); ins.Loc != at(12, 1, 13) {
		t.Errorf("insert table at %+v", ins.Loc)
	}
	if upd := parseOne(t, "update  t set a = 1").(*ast.Update); upd.Loc != at(8, 1, 9) {
		t.Errorf("update table at %+v", upd.Loc)
	}
	if del := parseOne(t, "delete from\nt").(*ast.Delete); del.Loc != at(12, 2, 1) {
		t.Errorf("delete table at %+v", del.Loc)
	}
}

func TestColumnPositions(t *testing.T) {
	ins := parseOne(t, "insert into t (a,\n  \"B\") values (1, 2)").(*ast.Insert)
	if want := []lexer.Pos{at(15, 1, 16), at(20, 2, 3)}; !reflect.DeepEqual(ins.ColumnLocs, want) {
		t.Errorf("insert columns at %+v, want %+v", ins.ColumnLocs, want)
	}
	if ins := parseOne(t, "insert into t values (1)").(*ast.Insert); ins.ColumnLocs != nil {
		t.Errorf("insert without column list has positions %+v", ins.ColumnLocs)
	}
	upd := parseOne(t, "update t set a = 1,\n b = 2").(*ast.Update)
	if upd.Set[0].Loc != at(13, 1, 14) || upd.Set[1].Loc != at(21, 2, 2) {
		t.Errorf("assignments at %+v, %+v", upd.Set[0].Loc, upd.Set[1].Loc)
	}
}

func TestMultipleStatements(t *testing.T) {
	cases := []struct {
		src  string
		want []string
	}{
		{"", nil},
		{";", nil},
		{" ; ; ", nil},
		{"select 1", []string{"SELECT 1"}},
		{"select 1;", []string{"SELECT 1"}},
		{"select 1;;", []string{"SELECT 1"}},
		{";select 1", []string{"SELECT 1"}},
		{"select 1; select 2", []string{"SELECT 1", "SELECT 2"}},
		{"begin; insert into t values (1); commit;", []string{"BEGIN", "INSERT INTO t VALUES (1)", "COMMIT"}},
	}
	for _, c := range cases {
		stmts, err := Parse(c.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.src, err)
			continue
		}
		var got []string
		for _, s := range stmts {
			got = append(got, s.String())
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("Parse(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestSyntaxErrors(t *testing.T) {
	cases := []struct {
		src      string
		pos      lexer.Pos
		expected string
		found    string
	}{
		{"selec 1", at(0, 1, 1), "statement", `"selec"`},
		{"1", at(0, 1, 1), "statement", `"1"`},
		{"select", at(6, 1, 7), "expression", "end of input"},
		{"select from t", at(7, 1, 8), "expression", `"from"`},
		{"select a from", at(13, 1, 14), "identifier", "end of input"},
		{"select a from 1", at(14, 1, 15), "identifier", `"1"`},
		{"select a from t t2 x", at(19, 1, 20), "end of statement", `"x"`},
		{"select a from t where", at(21, 1, 22), "expression", "end of input"},
		{"select a from t order a", at(22, 1, 23), "BY", `"a"`},
		{"select a from t limit", at(21, 1, 22), "expression", "end of input"},
		{"select a, from t", at(10, 1, 11), "expression", `"from"`},
		{"select a from t join u", at(22, 1, 23), "ON", "end of input"},
		{"select a from t join u where 1", at(23, 1, 24), "ON", `"where"`},
		{"select a from t,", at(16, 1, 17), "identifier", "end of input"},
		{"select * as x from t", at(9, 1, 10), "end of statement", `"as"`},
		{"select count(*) from t", at(12, 1, 13), "end of statement", `"("`},
		{"select a from t where a = 'x' 'y'", at(30, 1, 31), "end of statement", "'y'"},
		{"select a from t where a = b = c", at(28, 1, 29), "end of expression", `"="`},
		{"select a from t where a < b < c", at(28, 1, 29), "end of expression", `"<"`},
		{"select a from t where a is true", at(27, 1, 28), "NULL", `"true"`},
		{"select a from t where a is not", at(30, 1, 31), "NULL", "end of input"},
		{"select (1 + 2", at(13, 1, 14), ")", "end of input"},
		{"select (1 + 2))", at(14, 1, 15), "end of statement", `")"`},
		{"select 1 +", at(10, 1, 11), "expression", "end of input"},
		{"select 1 + * 2", at(11, 1, 12), "expression", `"*"`},
		{"select a.", at(9, 1, 10), "identifier", "end of input"},
		{"select a.1", at(9, 1, 10), "identifier", `"1"`},
		{"select 1.5", at(8, 1, 9), "end of statement", `"."`},
		{"select 1 2", at(9, 1, 10), "end of statement", `"2"`},
		{"select a from t where a = 1 select b", at(28, 1, 29), "end of statement", `"select"`},
		{"create table", at(12, 1, 13), "identifier", "end of input"},
		{"create", at(6, 1, 7), "TABLE or INDEX", "end of input"},
		{"create t (a int4)", at(7, 1, 8), "TABLE or INDEX", `"t"`},
		{"create table t", at(14, 1, 15), "(", "end of input"},
		{"create table t ()", at(16, 1, 17), "identifier", `")"`},
		{"create table t (a)", at(17, 1, 18), "type name", `")"`},
		{"create table t (a int4", at(22, 1, 23), ")", "end of input"},
		{"create table t (a int4,)", at(23, 1, 24), "identifier", `")"`},
		{"create table t (a int4 not)", at(26, 1, 27), "NULL", `")"`},
		{"create table t (a int4 primary)", at(30, 1, 31), "KEY", `")"`},
		{"create table t (a int4 unique)", at(23, 1, 24), ")", `"unique"`},
		{"create table t (a int4, primary key (a))", at(24, 1, 25), "identifier", `"primary"`},
		{"create unique t", at(14, 1, 15), "INDEX", `"t"`},
		{"create index on t (a)", at(13, 1, 14), "identifier", `"on"`},
		{"create index i t (a)", at(15, 1, 16), "ON", `"t"`},
		{"create index i on t", at(19, 1, 20), "(", "end of input"},
		{"create index i on t ()", at(21, 1, 22), "identifier", `")"`},
		{"create index i on t (a, b)", at(22, 1, 23), ")", `","`},
		{"create index i on t (a) where a > 1", at(24, 1, 25), "end of statement", `"where"`},
		{"drop t", at(5, 1, 6), "TABLE or INDEX", `"t"`},
		{"drop table", at(10, 1, 11), "identifier", "end of input"},
		{"drop index", at(10, 1, 11), "identifier", "end of input"},
		{"insert t values (1)", at(7, 1, 8), "INTO", `"t"`},
		{"insert into t", at(13, 1, 14), "VALUES", "end of input"},
		{"insert into t (a b) values (1)", at(17, 1, 18), ")", `"b"`},
		{"insert into t () values (1)", at(15, 1, 16), "identifier", `")"`},
		{"insert into t values", at(20, 1, 21), "(", "end of input"},
		{"insert into t values ()", at(22, 1, 23), "expression", `")"`},
		{"insert into t values (1),", at(25, 1, 26), "(", "end of input"},
		{"insert into t values (1) (2)", at(25, 1, 26), "end of statement", `"("`},
		{"insert into t select 1", at(14, 1, 15), "VALUES", `"select"`},
		{"update t a = 1", at(9, 1, 10), "SET", `"a"`},
		{"update t set a 1", at(15, 1, 16), "=", `"1"`},
		{"update t set a = 1,", at(19, 1, 20), "identifier", "end of input"},
		{"update t set a = 1 where", at(24, 1, 25), "expression", "end of input"},
		{"delete t", at(7, 1, 8), "FROM", `"t"`},
		{"delete from t where", at(19, 1, 20), "expression", "end of input"},
		{"begin transaction", at(6, 1, 7), "end of statement", `"transaction"`},
		{"commit 1", at(7, 1, 8), "end of statement", `"1"`},
		{"explain", at(7, 1, 8), "SELECT, INSERT, UPDATE, or DELETE", "end of input"},
		{"explain create table t (a int4)", at(8, 1, 9), "SELECT, INSERT, UPDATE, or DELETE", `"create"`},
		{"explain explain select 1", at(8, 1, 9), "SELECT, INSERT, UPDATE, or DELETE", `"explain"`},
		{"select 1; selec 2", at(10, 1, 11), "statement", `"selec"`},
		{"SELEC 1", at(0, 1, 1), "statement", `"SELEC"`},
		{"SELECT 1 FROM t ORDER Bye a", at(22, 1, 23), "BY", `"Bye"`},
		{`insert into t ("a" "B") values (1)`, at(19, 1, 20), ")", `""B""`},
		{`insert into t ("a" "x""y") values (1)`, at(19, 1, 20), ")", `""x""y""`},
		{"select 1\n\n  frm t", at(16, 3, 7), "end of statement", `"t"`},
		{"select ä, ö é 1", at(17, 1, 15), "end of statement", `"1"`},
	}
	for _, c := range cases {
		stmts, err := Parse(c.src)
		if stmts != nil {
			t.Errorf("Parse(%q) returned statements with error", c.src)
		}
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", c.src, err)
			continue
		}
		var pe *Error
		if !errors.As(err, &pe) {
			t.Errorf("Parse(%q): error is %T, want *Error", c.src, err)
			continue
		}
		if pe.Pos != c.pos || pe.Expected != c.expected || pe.Found != c.found {
			t.Errorf("Parse(%q): at %+v expected %q found %q\n want at %+v expected %q found %q",
				c.src, pe.Pos, pe.Expected, pe.Found, c.pos, c.expected, c.found)
		}
		msg := pe.Error()
		for _, part := range []string{"syntax error", "line 1", "expected " + c.expected, "found " + c.found} {
			if c.pos.Line != 1 && part == "line 1" {
				continue
			}
			if !strings.Contains(msg, part) {
				t.Errorf("Parse(%q): message %q lacks %q", c.src, msg, part)
			}
		}
	}
}

func TestUnknownType(t *testing.T) {
	cases := []struct {
		src  string
		pos  lexer.Pos
		name string
	}{
		{"create table t (a foo)", at(18, 1, 19), `"foo"`},
		{"create table t (a int4, b varchar)", at(26, 1, 27), `"varchar"`},
		{"create table t (a int2)", at(18, 1, 19), `"int2"`},
		{`create table t (a "INT4")`, at(18, 1, 19), `"INT4"`},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if !errors.Is(err, ErrUnknownType) {
			t.Errorf("Parse(%q) = %v, want ErrUnknownType", c.src, err)
			continue
		}
		var pe *Error
		if !errors.As(err, &pe) {
			t.Errorf("Parse(%q): error is %T", c.src, err)
			continue
		}
		if pe.Pos != c.pos || pe.Found != c.name {
			t.Errorf("Parse(%q): at %+v found %q, want at %+v found %q", c.src, pe.Pos, pe.Found, c.pos, c.name)
		}
		if !strings.Contains(pe.Error(), "type does not exist") {
			t.Errorf("Parse(%q): message %q", c.src, pe.Error())
		}
	}
}

func TestLexerErrorsPassThrough(t *testing.T) {
	cases := []struct {
		src  string
		want error
		pos  lexer.Pos
	}{
		{"select 'abc", lexer.ErrUnterminatedString, at(7, 1, 8)},
		{"select a from t where a = @", lexer.ErrBadChar, at(26, 1, 27)},
		{"select 1 /* c", lexer.ErrUnterminatedComment, at(9, 1, 10)},
		{"select 12abc", lexer.ErrTrailingJunk, at(7, 1, 8)},
	}
	for _, c := range cases {
		stmts, err := Parse(c.src)
		if stmts != nil {
			t.Errorf("Parse(%q) returned statements with error", c.src)
		}
		if !errors.Is(err, c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.src, err, c.want)
			continue
		}
		var le *lexer.Error
		if !errors.As(err, &le) || le.Pos != c.pos {
			t.Errorf("Parse(%q): %v (%T), want *lexer.Error at %+v", c.src, err, err, c.pos)
		}
		var pe *Error
		if errors.As(err, &pe) {
			t.Errorf("Parse(%q): lexical error wrapped in *Error", c.src)
		}
	}
}

func TestParseExprRequiresWholeInput(t *testing.T) {
	_, err := ParseExpr("1 + 2 3")
	var pe *Error
	if !errors.As(err, &pe) || pe.Expected != "end of input" || pe.Pos != at(6, 1, 7) {
		t.Errorf("got %v", err)
	}
	_, err = ParseExpr("")
	if !errors.As(err, &pe) || pe.Expected != "expression" {
		t.Errorf("empty: got %v", err)
	}
}

// TestExprDepthLimit checks that deep nesting is a parse error and not a
// stack overflow, which is fatal and unrecoverable.
func TestExprDepthLimit(t *testing.T) {
	deep := func(n int) string {
		return "select " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n)
	}
	if _, err := Parse(deep(MaxExprDepth - 2)); err != nil {
		t.Fatalf("Parse of %d parens: %v", MaxExprDepth-2, err)
	}
	for _, src := range []string{
		deep(MaxExprDepth + 1),
		// Spaced, since "--" would start a comment.
		"select " + strings.Repeat("- ", MaxExprDepth+1) + "1",
		"select " + strings.Repeat("not ", MaxExprDepth+1) + "true",
	} {
		_, err := Parse(src)
		if !errors.Is(err, ErrStackDepth) {
			t.Fatalf("Parse of deeply nested input = %v, want ErrStackDepth", err)
		}
		var pe *Error
		if !errors.As(err, &pe) || pe.Pos.Line == 0 {
			t.Fatalf("depth error is %T at %+v", err, pe)
		}
		if pe.Error() != fmt.Sprintf("stack depth limit exceeded at line %d, column %d", pe.Pos.Line, pe.Pos.Column) {
			t.Fatalf("depth error message %q", pe.Error())
		}
	}
	if _, err := ParseExpr(strings.Repeat("(", MaxExprDepth+1) + "1"); !errors.Is(err, ErrStackDepth) {
		t.Fatalf("ParseExpr of deeply nested input = %v, want ErrStackDepth", err)
	}
}

func FuzzParse(f *testing.F) {
	for _, c := range golden {
		f.Add(c.src)
	}
	for _, c := range exprGolden {
		f.Add("select " + c.src)
	}
	f.Add("select ((((((((((1))))))))))")
	f.Add("select 1 + + 1")
	f.Add("create table t (a foo)")
	f.Add("select 'abc")
	f.Fuzz(func(t *testing.T, src string) {
		stmts, err := Parse(src)
		if err != nil {
			if stmts != nil {
				t.Fatal("statements returned with error")
			}
			var pe *Error
			var le *lexer.Error
			switch {
			case errors.As(err, &pe):
				if pe.Pos.Offset < 0 || pe.Pos.Offset > len(src) || pe.Pos.Line < 1 || pe.Pos.Column < 1 {
					t.Fatalf("bad error position %+v", pe.Pos)
				}
			case errors.As(err, &le):
			default:
				t.Fatalf("error is %T: %v", err, err)
			}
			return
		}
		// The canonical form must parse back to itself.
		for _, s := range stmts {
			text := s.String()
			again, err := Parse(text)
			if err != nil {
				t.Fatalf("re-parse of %q: %v", text, err)
			}
			if len(again) != 1 || again[0].String() != text {
				t.Fatalf("re-parse of %q gave %v", text, again)
			}
		}
	})
}
