package analyzer

import (
	"errors"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// fakeCatalog is an in-memory Catalog.
type fakeCatalog map[string]*catalog.RelationInfo

func (f fakeCatalog) Lookup(name string) (*catalog.RelationInfo, error) {
	if info, ok := f[name]; ok {
		return info, nil
	}
	return nil, catalog.ErrNotFound
}

var cat = fakeCatalog{
	"t": {OID: 16384, Name: "t", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "a", Type: tuple.Int4, NotNull: true},
		tuple.Attr{Name: "b", Type: tuple.Text},
		tuple.Attr{Name: "c", Type: tuple.Bool},
	)},
	"u": {OID: 16385, Name: "u", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "a", Type: tuple.Int4},
		tuple.Attr{Name: "d", Type: tuple.Int8},
	)},
}

func analyze(t *testing.T, src string) (query.Stmt, error) {
	t.Helper()
	stmts, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("Parse(%q): %d statements, want 1", src, len(stmts))
	}
	return Analyze(stmts[0], cat)
}

func mustAnalyze(t *testing.T, src string) query.Stmt {
	t.Helper()
	q, err := analyze(t, src)
	if err != nil {
		t.Fatalf("Analyze(%q): %v", src, err)
	}
	return q
}

// golden pairs a statement with the dump of its bound form.
var golden = []struct{ src, want string }{
	// Constants and literal typing.
	{"select 1", `
Select
  Target: ?column? int4 := 1`},
	{"select 1, 'a', true, null", `
Select
  Target: ?column? int4 := 1
  Target: ?column? text := 'a'
  Target: ?column? bool := TRUE
  Target: ?column? text := NULL::text`},
	{"select 2147483647, 2147483648, -2147483648, -2147483649, 9223372036854775807, -9223372036854775808", `
Select
  Target: ?column? int4 := 2147483647
  Target: ?column? int8 := 2147483648::int8
  Target: ?column? int4 := -2147483648
  Target: ?column? int8 := -2147483649::int8
  Target: ?column? int8 := 9223372036854775807::int8
  Target: ?column? int8 := -9223372036854775808::int8`},
	{"select 'it''s'", `
Select
  Target: ?column? text := 'it''s'`},

	// Range table, star, aliases, target names.
	{"select * from t", `
Select
  From: t (16384)
  Target: a int4 := t.a
  Target: b text := t.b
  Target: c bool := t.c`},
	{"select t2.b, a as x, a + 1 y, 1 from t as t2", `
Select
  From: t AS t2 (16384)
  Target: b text := t2.b
  Target: x int4 := t2.a
  Target: y int4 := (t2.a + 1)
  Target: ?column? int4 := 1`},
	{"select *, u.d from t, u", `
Select
  From: t (16384), u (16385)
  Target: a int4 := t.a
  Target: b text := t.b
  Target: c bool := t.c
  Target: a int4 := u.a
  Target: d int8 := u.d
  Target: d int8 := u.d`},
	{`select "A".a, "A".b as "B" from t as "A"`, `
Select
  From: t AS "A" (16384)
  Target: a int4 := "A".a
  Target: B text := "A".b`},

	// Arithmetic and widening.
	{"select a + 1, a * 2 - 3, a / 2, -a, -(a + 1), -5 from t", `
Select
  From: t (16384)
  Target: ?column? int4 := (t.a + 1)
  Target: ?column? int4 := ((t.a * 2) - 3)
  Target: ?column? int4 := (t.a / 2)
  Target: ?column? int4 := (-t.a)
  Target: ?column? int4 := (-(t.a + 1))
  Target: ?column? int4 := -5`},
	{"select t.a + u.d, u.d + 1, t.a + 3000000000, -u.d from t, u", `
Select
  From: t (16384), u (16385)
  Target: ?column? int8 := (t.a::int8 + u.d)
  Target: ?column? int8 := (u.d + 1::int8)
  Target: ?column? int8 := (t.a::int8 + 3000000000::int8)
  Target: ?column? int8 := (-u.d)`},

	// Comparisons.
	{"select t.a = 1, b <> 'x', c = true, t.a < u.d, 1 >= 2, 'a' <= 'b' from t, u", `
Select
  From: t (16384), u (16385)
  Target: ?column? bool := (t.a = 1)
  Target: ?column? bool := (t.b <> 'x')
  Target: ?column? bool := (t.c = TRUE)
  Target: ?column? bool := (t.a::int8 < u.d)
  Target: ?column? bool := (1 >= 2)
  Target: ?column? bool := ('a' <= 'b')`},

	// Untyped literals take the type of the other operand.
	{"select t.a = '5', u.d = ' -7 ', c = 't', c = 'off', 'YES' = c, b = 'x', t.a + '1' from t, u", `
Select
  From: t (16384), u (16385)
  Target: ?column? bool := (t.a = 5)
  Target: ?column? bool := (u.d = -7::int8)
  Target: ?column? bool := (t.c = TRUE)
  Target: ?column? bool := (t.c = FALSE)
  Target: ?column? bool := (TRUE = t.c)
  Target: ?column? bool := (t.b = 'x')
  Target: ?column? int4 := (t.a + 1)`},
	{"select t.a = null, null = b, null is null, 'x' is not null, u.d = null from t, u", `
Select
  From: t (16384), u (16385)
  Target: ?column? bool := (t.a = NULL::int4)
  Target: ?column? bool := (NULL::text = t.b)
  Target: ?column? bool := (NULL::text IS NULL)
  Target: ?column? bool := ('x' IS NOT NULL)
  Target: ?column? bool := (u.d = NULL::int8)`},

	// Boolean logic; nested same-operator applications flatten.
	{"select not c, c and a > 1 and b = 'x', c or a = 1 or a = 2, c and (c and c), (c or c) and c, 'true' and c from t", `
Select
  From: t (16384)
  Target: ?column? bool := (NOT t.c)
  Target: ?column? bool := (t.c AND (t.a > 1) AND (t.b = 'x'))
  Target: ?column? bool := (t.c OR (t.a = 1) OR (t.a = 2))
  Target: ?column? bool := (t.c AND t.c AND t.c)
  Target: ?column? bool := ((t.c OR t.c) AND t.c)
  Target: ?column? bool := (TRUE AND t.c)`},
	{"select a is null, (a + 1) is not null, c is null is null from t", `
Select
  From: t (16384)
  Target: ?column? bool := (t.a IS NULL)
  Target: ?column? bool := ((t.a + 1) IS NOT NULL)
  Target: ?column? bool := ((t.c IS NULL) IS NULL)`},

	// WHERE, ORDER BY, LIMIT.
	{"select a from t where a > 1 and b is not null order by b desc, a limit 10", `
Select
  From: t (16384)
  Target: a int4 := t.a
  Where: ((t.a > 1) AND (t.b IS NOT NULL))
  Order: t.b DESC, t.a ASC
  Limit: 10::int8`},
	{"select a from t where 'true' limit '3'", `
Select
  From: t (16384)
  Target: a int4 := t.a
  Where: TRUE
  Limit: 3::int8`},
	{"select a from t where c", `
Select
  From: t (16384)
  Target: a int4 := t.a
  Where: t.c`},
	{"select a as x, b from t order by 2, x desc, a + 1, 'k'", `
Select
  From: t (16384)
  Target: x int4 := t.a
  Target: b text := t.b
  Order: t.b ASC, t.a DESC, (t.a + 1) ASC, 'k' ASC`},
	{"select a, a from t order by a", `
Select
  From: t (16384)
  Target: a int4 := t.a
  Target: a int4 := t.a
  Order: t.a ASC`},
	{"select a + 1 as b from t order by b", `
Select
  From: t (16384)
  Target: b int4 := (t.a + 1)
  Order: (t.a + 1) ASC`},
	{"select 1 limit 2 + 3", `
Select
  Target: ?column? int4 := 1
  Limit: (2 + 3)::int8`},

	// Joins flatten into the range table and the qual.
	{"select t.a, u.d from t join u on t.a = u.a", `
Select
  From: t (16384), u (16385)
  Target: a int4 := t.a
  Target: d int8 := u.d
  Where: (t.a = u.a)`},
	{"select t.a from t join u on t.a = u.a join t as t2 on t2.a = u.a where t.c", `
Select
  From: t (16384), u (16385), t AS t2 (16384)
  Target: a int4 := t.a
  Where: ((t.a = u.a) AND (t2.a = u.a) AND t.c)`},
	{"select 1 from t, u join t t2 on u.a = t2.a where t.a = 1 and u.a = 2", `
Select
  From: t (16384), u (16385), t AS t2 (16384)
  Target: ?column? int4 := 1
  Where: ((u.a = t2.a) AND (t.a = 1) AND (u.a = 2))`},

	// INSERT.
	{"insert into t values (1, 'x', true)", `
Insert t (16384)
  Row: 1, 'x', TRUE`},
	{"insert into t (c, a) values (false, 2), ('t', '3')", `
Insert t (16384)
  Row: 2, NULL::text, FALSE
  Row: 3, NULL::text, TRUE`},
	{"insert into t values (1)", `
Insert t (16384)
  Row: 1, NULL::text, NULL::bool`},
	{"insert into u values (1, 2), (3, 3000000000), (4, null)", `
Insert u (16385)
  Row: 1, 2::int8
  Row: 3, 3000000000::int8
  Row: 4, NULL::int8`},
	{"insert into t values (1 + 1, 'a', 1 > 0)", `
Insert t (16384)
  Row: (1 + 1), 'a', (1 > 0)`},
	// UPDATE and DELETE.
	{"update t set a = a + 1, b = null where c", `
Update t (16384)
  Set: a := (t.a + 1)
  Set: b := NULL::text
  Where: t.c`},
	{"update u set d = 1, a = '2'", `
Update u (16385)
  Set: d := 1::int8
  Set: a := 2`},
	{"delete from t", `
Delete t (16384)`},
	{"delete from t where a = '1' or b is null", `
Delete t (16384)
  Where: ((t.a = 1) OR (t.b IS NULL))`},

	// DDL and transaction control pass through.
	{"create table x (id int4 primary key, name text not null, ok bool)", `
CreateTable x (id int4 NOT NULL PRIMARY KEY, name text NOT NULL, ok bool)`},
	{"create table x (a int8)", `
CreateTable x (a int8)`},
	{"drop table x", `
DropTable x`},
	{"begin", `
Begin`},
	{"commit", `
Commit`},
	{"rollback", `
Rollback`},
	{"explain select a from t where a = 1", `
Explain
  Select
    From: t (16384)
    Target: a int4 := t.a
    Where: (t.a = 1)`},
	{"explain delete from t", `
Explain
  Delete t (16384)`},
}

func TestGolden(t *testing.T) {
	for _, tc := range golden {
		want := strings.TrimPrefix(tc.want, "\n")
		q := mustAnalyze(t, tc.src)
		if got := q.String(); got != want {
			t.Errorf("Analyze(%q)\n got:\n%s\nwant:\n%s", tc.src, got, want)
		}
	}
}

func TestTypes(t *testing.T) {
	q := mustAnalyze(t, "select t.a, b, c, u.d, t.a + u.d, t.a = 1, t.a is null, not c, -t.a, null, 'x' from t, u").(*query.Select)
	want := []tuple.TypeID{
		tuple.Int4, tuple.Text, tuple.Bool, tuple.Int8, tuple.Int8,
		tuple.Bool, tuple.Bool, tuple.Bool, tuple.Int4, tuple.Text, tuple.Text,
	}
	if len(q.Targets) != len(want) {
		t.Fatalf("%d targets, want %d", len(q.Targets), len(want))
	}
	for i, tg := range q.Targets {
		if tg.Expr.Type() != want[i] {
			t.Errorf("target %d (%s): type %v, want %v", i, tg.Expr, tg.Expr.Type(), want[i])
		}
	}
}

func TestVars(t *testing.T) {
	q := mustAnalyze(t, "select u.d, x.b from t as x, u").(*query.Select)
	if len(q.Range) != 2 || q.Range[0].Alias != "x" || q.Range[0].Rel != cat["t"] || q.Range[1].Alias != "u" || q.Range[1].Rel != cat["u"] {
		t.Fatalf("range table: %+v", q.Range)
	}
	d, ok := q.Targets[0].Expr.(*query.Var)
	if !ok || d.Rel != 1 || d.Attr != 1 || d.Typ != tuple.Int8 || d.Alias != "u" || d.Column != "d" {
		t.Errorf("u.d = %#v", q.Targets[0].Expr)
	}
	b, ok := q.Targets[1].Expr.(*query.Var)
	if !ok || b.Rel != 0 || b.Attr != 1 || b.Typ != tuple.Text || b.Alias != "x" || b.Column != "b" {
		t.Errorf("x.b = %#v", q.Targets[1].Expr)
	}
}

func TestConstValues(t *testing.T) {
	ins := mustAnalyze(t, "insert into u values ('5', '6'), (7, 8)").(*query.Insert)
	if ins.Rel.Rel != cat["u"] || ins.Rel.Alias != "u" {
		t.Fatalf("rel: %+v", ins.Rel)
	}
	want := [][]tuple.Datum{{int32(5), int64(6)}, {int32(7), int64(8)}}
	for i, row := range ins.Rows {
		for j, e := range row {
			c, ok := e.(*query.Const)
			if !ok || c.Null || c.Value != want[i][j] {
				t.Errorf("row %d col %d = %#v, want %#v", i, j, e, want[i][j])
			}
		}
	}
	sel := mustAnalyze(t, "select 'x', true, null, 3000000000 from t where c = 'no'").(*query.Select)
	for i, want := range []tuple.Datum{"x", true, nil, int64(3000000000)} {
		c := sel.Targets[i].Expr.(*query.Const)
		if c.Value != want || c.Null != (want == nil) {
			t.Errorf("target %d = %#v", i, c)
		}
	}
	if c := sel.Where.(*query.OpExpr).Right.(*query.Const); c.Value != false || c.Typ != tuple.Bool {
		t.Errorf("where const = %#v", c)
	}
}

func TestUpdateAttrs(t *testing.T) {
	u := mustAnalyze(t, "update t set c = true, a = 1").(*query.Update)
	if len(u.Set) != 2 || u.Set[0].Attr != 2 || u.Set[1].Attr != 0 {
		t.Fatalf("assignments: %+v", u.Set)
	}
	if u.Where != nil {
		t.Errorf("Where = %v, want nil", u.Where)
	}
}

func TestCreateTable(t *testing.T) {
	c := mustAnalyze(t, "create table x (id int4 primary key, name text not null, ok bool)").(*query.CreateTable)
	want := tuple.NewDesc(
		tuple.Attr{Name: "id", Type: tuple.Int4, NotNull: true},
		tuple.Attr{Name: "name", Type: tuple.Text, NotNull: true},
		tuple.Attr{Name: "ok", Type: tuple.Bool},
	)
	if c.Name != "x" || c.PrimaryKey != 0 || len(c.Desc.Attrs) != 3 {
		t.Fatalf("got %+v", c)
	}
	for i, a := range c.Desc.Attrs {
		if a != want.Attrs[i] {
			t.Errorf("attr %d = %+v, want %+v", i, a, want.Attrs[i])
		}
	}
	if c := mustAnalyze(t, "create table x (a int8)").(*query.CreateTable); c.PrimaryKey != -1 {
		t.Errorf("PrimaryKey = %d, want -1", c.PrimaryKey)
	}
}

func TestErrors(t *testing.T) {
	// col is the 1-based column of the error position on line 1, or 0 when
	// the error carries no position.
	cases := []struct {
		src  string
		want error
		msg  string
		col  int
	}{
		// Names.
		{"select x from t", ErrUndefinedColumn, `column "x" does not exist`, 8},
		{"select t.x from t", ErrUndefinedColumn, `column t.x does not exist`, 8},
		{"select a from t where a = 1 and zz = 2", ErrUndefinedColumn, `column "zz" does not exist`, 33},
		{"select a from t, u", ErrAmbiguousColumn, `column reference "a" is ambiguous`, 8},
		{"select u.a from t", ErrUndefinedTable, `missing FROM-clause entry for table "u"`, 8},
		{"select t.a from t as x", ErrUndefinedTable, `invalid reference to FROM-clause entry for table "t"`, 8},
		{"select 1 from t, u join u as w on t.a = w.a", ErrUndefinedTable, `invalid reference to FROM-clause entry for table "t"`, 35},
		{"select 1 from nope", ErrUndefinedTable, `relation "nope" does not exist`, 15},
		{"select 1 from t join nope on true", ErrUndefinedTable, `relation "nope" does not exist`, 22},
		{"select a from t, t", ErrDuplicateAlias, `table name "t" specified more than once`, 18},
		{"select a from t as u, u", ErrDuplicateAlias, `table name "u" specified more than once`, 23},
		{"select a", ErrUndefinedColumn, `column "a" does not exist`, 8},
		{"select *", ErrSyntax, `SELECT * with no tables specified is not valid`, 8},
		{"select a from t order by 2", ErrInvalidColumnRef, `ORDER BY position 2 is not in select list`, 26},
		{"select a from t order by 0", ErrInvalidColumnRef, `ORDER BY position 0 is not in select list`, 26},
		{"select a as x, b as x from t order by x", ErrInvalidColumnRef, `ORDER BY "x" is ambiguous`, 39},
		{"select a from t limit a", ErrInvalidColumnRef, `argument of LIMIT must not contain variables`, 23},

		// Operators and types.
		{"select b + 1 from t", ErrUndefinedOperator, `operator does not exist: text + integer`, 10},
		{"select 1 - b from t", ErrUndefinedOperator, `operator does not exist: integer - text`, 10},
		{"select b > 1 from t", ErrUndefinedOperator, `operator does not exist: text > integer`, 10},
		{"select u.d = c from t, u", ErrUndefinedOperator, `operator does not exist: bigint = boolean`, 12},
		{"select c + c from t", ErrUndefinedOperator, `operator does not exist: boolean + boolean`, 10},
		{"select b * b from t", ErrUndefinedOperator, `operator does not exist: text * text`, 10},
		{"select 'a' + 'b'", ErrUndefinedOperator, `operator does not exist: text + text`, 12},
		{"select -b from t", ErrUndefinedOperator, `operator does not exist: - text`, 8},
		{"select -c from t", ErrUndefinedOperator, `operator does not exist: - boolean`, 8},
		{"select not a from t", ErrTypeMismatch, `argument of NOT must be type boolean, not type integer`, 12},
		{"select a and c from t", ErrTypeMismatch, `argument of AND must be type boolean, not type integer`, 8},
		{"select c or b from t", ErrTypeMismatch, `argument of OR must be type boolean, not type text`, 13},
		{"select a from t where a", ErrTypeMismatch, `argument of WHERE must be type boolean, not type integer`, 23},
		{"select a from t where b", ErrTypeMismatch, `argument of WHERE must be type boolean, not type text`, 23},
		{"select 1 from t join u on t.a where true", ErrTypeMismatch, `argument of JOIN/ON must be type boolean, not type integer`, 27},
		{"select a from t limit true", ErrTypeMismatch, `argument of LIMIT must be type bigint, not type boolean`, 23},
		{"select a from t limit 'x'", ErrInvalidInput, `invalid input syntax for type bigint: "x"`, 23},
		{"select a = 'x' from t", ErrInvalidInput, `invalid input syntax for type integer: "x"`, 12},
		{"select u.d = '1.5' from u", ErrInvalidInput, `invalid input syntax for type bigint: "1.5"`, 14},
		{"select c = 'maybe' from t", ErrInvalidInput, `invalid input syntax for type boolean: "maybe"`, 12},
		{"select c = 'o' from t", ErrInvalidInput, `invalid input syntax for type boolean: "o"`, 12},
		{"select c = 'of' from t", ErrInvalidInput, `invalid input syntax for type boolean: "of"`, 12},
		{"select c or 'x' from t", ErrInvalidInput, `invalid input syntax for type boolean: "x"`, 13},
		{"select a from t where 'nope'", ErrInvalidInput, `invalid input syntax for type boolean: "nope"`, 23},
		{"select a = '2147483648' from t", ErrOutOfRange, `value "2147483648" is out of range for type integer`, 12},
		{"select 99999999999999999999", ErrOutOfRange, `value "99999999999999999999" is out of range for type bigint`, 8},
		{"select -99999999999999999999", ErrOutOfRange, `value "-99999999999999999999" is out of range for type bigint`, 8},

		// INSERT.
		{"insert into nope values (1)", ErrUndefinedTable, `relation "nope" does not exist`, 13},
		{"insert into t values (1, 'x', true, 1)", ErrSyntax, `INSERT has more expressions than target columns`, 37},
		{"insert into t (a) values (1, 2)", ErrSyntax, `INSERT has more expressions than target columns`, 30},
		{"insert into t (a, b) values (1)", ErrSyntax, `INSERT has more target columns than expressions`, 19},
		{"insert into t (a, a) values (1, 2)", ErrDuplicateColumn, `column "a" specified more than once`, 19},
		{"insert into t (x) values (1)", ErrUndefinedColumn, `column "x" of relation "t" does not exist`, 16},
		{"insert into t values ('x', 'y', 'z')", ErrInvalidInput, `invalid input syntax for type integer: "x"`, 23},
		{"insert into t values (true, 'y', true)", ErrTypeMismatch, `column "a" is of type integer but expression is of type boolean`, 23},
		{"insert into t values (1, 2, true)", ErrTypeMismatch, `column "b" is of type text but expression is of type integer`, 26},
		{"insert into u values (3000000000, 1)", ErrTypeMismatch, `column "a" is of type integer but expression is of type bigint`, 23},
		{"insert into t values (null, 'x', true)", ErrNotNull, `null value in column "a" of relation "t" violates not-null constraint`, 0},
		{"insert into t (b) values ('x')", ErrNotNull, `null value in column "a" of relation "t" violates not-null constraint`, 0},
		{"insert into t values (a, 'x', true)", ErrUndefinedColumn, `column "a" does not exist`, 23},
		{"insert into t values (1, 'x', true), (2, 'y', 3)", ErrTypeMismatch, `column "c" is of type boolean but expression is of type integer`, 47},
		{"insert into t values (1), (2, 'x', true)", ErrSyntax, `VALUES lists must all be the same length`, 36},
		{"insert into t values (1, 'x', true), (2)", ErrSyntax, `VALUES lists must all be the same length`, 39},
		{"insert into t (a, b) values (1, 'x'), (2)", ErrSyntax, `VALUES lists must all be the same length`, 40},

		// UPDATE and DELETE.
		{"update nope set a = 1", ErrUndefinedTable, `relation "nope" does not exist`, 8},
		{"update t set x = 1", ErrUndefinedColumn, `column "x" of relation "t" does not exist`, 14},
		{"update t set a = 1, a = 2", ErrSyntax, `multiple assignments to same column "a"`, 0},
		{"update t set a = null", ErrNotNull, `null value in column "a" of relation "t" violates not-null constraint`, 0},
		{"update t set a = 'x'", ErrInvalidInput, `invalid input syntax for type integer: "x"`, 18},
		{"update t set b = 1", ErrTypeMismatch, `column "b" is of type text but expression is of type integer`, 18},
		{"update t set a = 1 where u.a = 1", ErrUndefinedTable, `missing FROM-clause entry for table "u"`, 26},
		{"update t set a = 1 where b", ErrTypeMismatch, `argument of WHERE must be type boolean, not type text`, 26},
		{"delete from nope", ErrUndefinedTable, `relation "nope" does not exist`, 13},
		{"delete from t where a", ErrTypeMismatch, `argument of WHERE must be type boolean, not type integer`, 21},
		{"delete from t where x = 1", ErrUndefinedColumn, `column "x" does not exist`, 21},

		// DDL.
		{"create table x (a int4 primary key, b int4 primary key)", ErrTableDefinition, `multiple primary keys for table "x" are not allowed`, 0},

		// EXPLAIN analyzes its statement.
		{"explain select x from t", ErrUndefinedColumn, `column "x" does not exist`, 16},
	}
	for _, tc := range cases {
		_, err := analyze(t, tc.src)
		if err == nil {
			t.Errorf("Analyze(%q): no error, want %v", tc.src, tc.want)
			continue
		}
		var aerr *Error
		if !errors.As(err, &aerr) {
			t.Errorf("Analyze(%q): %T %v, want *Error", tc.src, err, err)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("Analyze(%q): %v, want %v", tc.src, err, tc.want)
		}
		if aerr.Msg != tc.msg || err.Error() != tc.msg {
			t.Errorf("Analyze(%q): message %q, want %q", tc.src, aerr.Msg, tc.msg)
		}
		wantPos := lexer.Pos{}
		if tc.col > 0 {
			wantPos = lexer.Pos{Offset: tc.col - 1, Line: 1, Column: tc.col}
		}
		if aerr.Pos != wantPos {
			t.Errorf("Analyze(%q): position %+v, want %+v", tc.src, aerr.Pos, wantPos)
		}
	}
}

func TestPositionsSpanLines(t *testing.T) {
	_, err := analyze(t, "select a\nfrom t\nwhere  zz = 1")
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("got %v", err)
	}
	if want := (lexer.Pos{Offset: 23, Line: 3, Column: 8}); aerr.Pos != want {
		t.Errorf("position %+v, want %+v", aerr.Pos, want)
	}
}

func TestCatalogErrorsPassThrough(t *testing.T) {
	broken := errors.New("disk on fire")
	failing := catalogFunc(func(string) (*catalog.RelationInfo, error) { return nil, broken })
	stmts, err := parser.Parse("select 1 from t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(stmts[0], failing); !errors.Is(err, broken) {
		t.Errorf("got %v, want the catalog's error", err)
	}
}

type catalogFunc func(string) (*catalog.RelationInfo, error)

func (f catalogFunc) Lookup(name string) (*catalog.RelationInfo, error) { return f(name) }

// TestStmtKinds checks the bound type produced for each statement kind.
func TestStmtKinds(t *testing.T) {
	cases := []struct {
		src  string
		want query.Stmt
	}{
		{"select 1", &query.Select{}},
		{"insert into t values (1)", &query.Insert{}},
		{"update t set a = 1", &query.Update{}},
		{"delete from t", &query.Delete{}},
		{"create table x (a int4)", &query.CreateTable{}},
		{"drop table t", &query.DropTable{}},
		{"begin", &query.Begin{}},
		{"commit", &query.Commit{}},
		{"rollback", &query.Rollback{}},
		{"explain select 1", &query.Explain{}},
	}
	for _, tc := range cases {
		got := mustAnalyze(t, tc.src)
		if gotT, wantT := typeName(got), typeName(tc.want); gotT != wantT {
			t.Errorf("Analyze(%q) = %s, want %s", tc.src, gotT, wantT)
		}
	}
	// A statement the parser never produces is rejected, not ignored.
	if _, err := Analyze(nil, cat); err == nil {
		t.Error("Analyze(nil) = nil error")
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *query.Select:
		return "Select"
	case *query.Insert:
		return "Insert"
	case *query.Update:
		return "Update"
	case *query.Delete:
		return "Delete"
	case *query.CreateTable:
		return "CreateTable"
	case *query.DropTable:
		return "DropTable"
	case *query.Begin:
		return "Begin"
	case *query.Commit:
		return "Commit"
	case *query.Rollback:
		return "Rollback"
	case *query.Explain:
		return "Explain"
	}
	return "unknown"
}

var _ ast.Stmt = (*ast.Select)(nil)
