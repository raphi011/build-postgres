package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/planner"
	"github.com/raphi011/build-postgres/internal/sql/analyzer"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/tuple"
)

func open(t *testing.T, dir string) *Session {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newSession(t *testing.T) *Session {
	t.Helper()
	return open(t, filepath.Join(t.TempDir(), "data"))
}

// exec runs one statement that must succeed and returns its result.
func exec(t *testing.T, s *Session, sql string) *Result {
	t.Helper()
	res, err := s.Exec(sql)
	if err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
	if len(res) != 1 {
		t.Fatalf("Exec(%q): %d results, want 1", sql, len(res))
	}
	return res[0]
}

// lines joins with newlines; it keeps golden output with trailing
// spaces readable.
func lines(ls ...string) string {
	return strings.Join(ls, "\n") + "\n"
}

func TestExec(t *testing.T) {
	s := newSession(t)
	if r := exec(t, s, "create table t (a int4 primary key, b text)"); r.Tag != "CREATE TABLE" || r.Columns != nil {
		t.Errorf("create: %+v", r)
	}
	if r := exec(t, s, "insert into t values (1, 'one'), (2, null), (3, 'three')"); r.Tag != "INSERT 0 3" {
		t.Errorf("insert tag %q", r.Tag)
	}
	r := exec(t, s, "select a, b, a * 2 as d from t where a <> 2 order by a desc")
	if r.Tag != "SELECT 2" {
		t.Errorf("select tag %q", r.Tag)
	}
	wantCols := []Column{{"a", tuple.Int4}, {"b", tuple.Text}, {"d", tuple.Int4}}
	if !reflect.DeepEqual(r.Columns, wantCols) {
		t.Errorf("columns %v, want %v", r.Columns, wantCols)
	}
	wantRows := [][]tuple.Datum{{int32(3), "three", int32(6)}, {int32(1), "one", int32(2)}}
	if !reflect.DeepEqual(r.Rows, wantRows) {
		t.Errorf("rows %v, want %v", r.Rows, wantRows)
	}
	r = exec(t, s, "select b from t where a = 2")
	if r.Tag != "SELECT 1" || len(r.Rows) != 1 || r.Rows[0][0] != nil {
		t.Errorf("null row: %+v", r)
	}
	if r := exec(t, s, "update t set b = 'two' where a = 2"); r.Tag != "UPDATE 1" || r.Columns != nil {
		t.Errorf("update: %+v", r)
	}
	if r := exec(t, s, "delete from t where a > 1"); r.Tag != "DELETE 2" {
		t.Errorf("delete tag %q", r.Tag)
	}
	if r := exec(t, s, "select * from t"); r.Tag != "SELECT 1" || !reflect.DeepEqual(r.Rows, [][]tuple.Datum{{int32(1), "one"}}) {
		t.Errorf("after dml: %+v", r)
	}
	if r := exec(t, s, "select * from t where a = 99"); r.Tag != "SELECT 0" || len(r.Rows) != 0 || len(r.Columns) != 2 {
		t.Errorf("empty select: %+v", r)
	}
	for _, sql := range []string{"begin", "commit", "rollback"} {
		if r := exec(t, s, sql); r.Tag != strings.ToUpper(sql) || r.Columns != nil {
			t.Errorf("%s: %+v", sql, r)
		}
	}
	if r := exec(t, s, "select 1 + 1, 'x', true, null"); r.Tag != "SELECT 1" ||
		!reflect.DeepEqual(r.Rows, [][]tuple.Datum{{int32(2), "x", true, nil}}) {
		t.Errorf("select without from: %+v", r)
	}
	if r := exec(t, s, "drop table t"); r.Tag != "DROP TABLE" {
		t.Errorf("drop tag %q", r.Tag)
	}
	if _, err := s.Exec("select * from t"); err == nil {
		t.Error("select after drop succeeded")
	}
}

func TestExecMany(t *testing.T) {
	s := newSession(t)
	res, err := s.Exec("select 1; select 2;; select 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[2].Rows[0][0] != int32(3) {
		t.Errorf("results = %+v", res)
	}
	res, err = s.Exec("")
	if err != nil || len(res) != 0 {
		t.Errorf("empty input: %v, %v", res, err)
	}
	// The results before an error are returned with it.
	res, err = s.Exec("select 1; select * from nope; select 3")
	if err == nil || len(res) != 1 {
		t.Errorf("results before error = %d, err %v", len(res), err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Pos != (lexer.Pos{Offset: 24, Line: 1, Column: 25}) {
		t.Errorf("error = %#v", err)
	}
}

func TestPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, s, "create table t (a int4, b text)")
	exec(t, s, "insert into t values (1, 'x'), (2, 'y')")
	exec(t, s, "delete from t where a = 1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	r := exec(t, s, "select * from t")
	if !reflect.DeepEqual(r.Rows, [][]tuple.Datum{{int32(2), "y"}}) {
		t.Errorf("after reopen: %v", r.Rows)
	}
	if _, err := s.Exec("create table t (a int4)"); !errors.Is(err, catalog.ErrExists) {
		t.Errorf("create existing after reopen: %v", err)
	}
}

func TestErrors(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4 not null, b int4, c text)")
	exec(t, s, "insert into t values (1, null, 'x')")
	cases := []struct {
		sql  string
		msg  string
		line int
		col  int
		err  error
	}{
		{"select * from nope", `relation "nope" does not exist`, 1, 15, analyzer.ErrUndefinedTable},
		{"select x from t", `column "x" does not exist`, 1, 8, analyzer.ErrUndefinedColumn},
		{"select a from t where c", `argument of WHERE must be type boolean, not type text`, 1, 23, analyzer.ErrTypeMismatch},
		{"selec 1", `syntax error at or near "selec"`, 1, 1, parser.ErrSyntax},
		{"select 1 +", `syntax error at end of input`, 1, 11, parser.ErrSyntax},
		{"select a from t where", `syntax error at end of input`, 1, 22, parser.ErrSyntax},
		{"select a from t\nwhere a = = 1", `syntax error at or near "="`, 2, 11, parser.ErrSyntax},
		{"create table x (a foo)", `type "foo" does not exist`, 1, 19, parser.ErrUnknownType},
		{"select 'abc", `unterminated quoted string`, 1, 8, lexer.ErrUnterminatedString},
		{"select 1 /* x", `unterminated /* comment`, 1, 10, lexer.ErrUnterminatedComment},
		{"create table t (a int4)", `relation "t" already exists`, 0, 0, catalog.ErrExists},
		{"create table x (a int4, a text)", `column "a" specified more than once`, 0, 0, catalog.ErrDuplicateColumn},
		{"drop table nope", `table "nope" does not exist`, 0, 0, catalog.ErrNotFound},
		{"drop table pg_class", `permission denied: "pg_class" is a system catalog`, 0, 0, catalog.ErrSystemTable},
		{"select 1 / 0", `division by zero`, 0, 0, expr.ErrDivisionByZero},
		{"select a from t where 2147483647 + a > 0", `integer out of range`, 0, 0, expr.ErrOutOfRange},
		{"select * from t limit -1", `LIMIT must not be negative`, 0, 0, executor.ErrInvalidRowCount},
		{"update t set a = b", `null value in column "a" of relation "t" violates not-null constraint`, 0, 0, executor.ErrNotNull},
		{"select * from t, t as u", `joins are not supported until chapter 15`, 0, 0, planner.ErrJoin},
	}
	for _, c := range cases {
		_, err := s.Exec(c.sql)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("Exec(%q): %v (%T), want *Error", c.sql, err, err)
			continue
		}
		if e.Msg != c.msg || e.Error() != c.msg {
			t.Errorf("Exec(%q): message %q, want %q", c.sql, e.Msg, c.msg)
		}
		if e.Pos.Line != c.line || e.Pos.Column != c.col {
			t.Errorf("Exec(%q): position %d:%d, want %d:%d", c.sql, e.Pos.Line, e.Pos.Column, c.line, c.col)
		}
		if !errors.Is(err, c.err) {
			t.Errorf("Exec(%q): %v does not unwrap to %v", c.sql, err, c.err)
		}
		if e.Query != c.sql {
			t.Errorf("Exec(%q): Query = %q", c.sql, e.Query)
		}
	}
}

func TestTooManyColumns(t *testing.T) {
	s := newSession(t)
	create := func(name string, n int) error {
		cols := make([]string, n)
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d bool", i)
		}
		_, err := s.Exec("create table " + name + " (" + strings.Join(cols, ", ") + ")")
		return err
	}
	if err := create("wide", catalog.MaxColumns); err != nil {
		t.Fatalf("create table with %d columns: %v", catalog.MaxColumns, err)
	}
	err := create("wider", catalog.MaxColumns+1)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, catalog.ErrTooManyColumns) {
		t.Fatalf("create table with %d columns = %v, want ErrTooManyColumns", catalog.MaxColumns+1, err)
	}
	if e.Msg != "tables can have at most 1600 columns" {
		t.Errorf("message %q", e.Msg)
	}
	if _, err := s.Exec("select c0 from wider"); !errors.Is(err, analyzer.ErrUndefinedTable) {
		t.Errorf("the rejected table exists: %v", err)
	}
}

func TestErrorReport(t *testing.T) {
	cases := []struct {
		err  Error
		want string
	}{
		{Error{Msg: `relation "nope" does not exist`, Pos: lexer.Pos{Offset: 14, Line: 1, Column: 15}, Query: "select * from nope;"},
			lines(`ERROR:  relation "nope" does not exist`,
				`LINE 1: select * from nope;`,
				`                      ^`)},
		{Error{Msg: `column "b" does not exist`, Pos: lexer.Pos{Offset: 12, Line: 2, Column: 3}, Query: "select a,\n  b from t;"},
			lines(`ERROR:  column "b" does not exist`,
				`LINE 2:   b from t;`,
				`          ^`)},
		{Error{Msg: `syntax error at end of input`, Pos: lexer.Pos{Offset: 8, Line: 1, Column: 9}, Query: "select 1"},
			lines(`ERROR:  syntax error at end of input`,
				`LINE 1: select 1`,
				`                ^`)},
		// Columns count characters, not bytes.
		{Error{Msg: `column "b" does not exist`, Pos: lexer.Pos{Offset: 13, Line: 1, Column: 13}, Query: "select 'ü', b"},
			lines(`ERROR:  column "b" does not exist`,
				`LINE 1: select 'ü', b`,
				`                    ^`)},
		{Error{Msg: `division by zero`, Query: "select 1/0"},
			lines(`ERROR:  division by zero`)},
	}
	for _, c := range cases {
		if got := c.err.Report(); got != c.want {
			t.Errorf("Report(%q) =\n%s\nwant\n%s", c.err.Msg, got, c.want)
		}
	}
}

func TestResultString(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want string
	}{
		{"no columns", Result{Tag: "CREATE TABLE"}, ""},
		{"numbers right, text left, null empty",
			Result{Columns: []Column{{"a", tuple.Int4}, {"b", tuple.Text}}, Rows: [][]tuple.Datum{{int32(1), "x"}, {int32(22), nil}}},
			lines(` a  | b `,
				`----+---`,
				`  1 | x`,
				` 22 | `,
				`(2 rows)`,
				``)},
		{"one row", Result{Columns: []Column{{"?column?", tuple.Int4}}, Rows: [][]tuple.Datum{{int32(1)}}},
			lines(` ?column? `,
				`----------`,
				`        1`,
				`(1 row)`,
				``)},
		{"no rows", Result{Columns: []Column{{"a", tuple.Int4}, {"name", tuple.Text}}},
			lines(` a | name `,
				`---+------`,
				`(0 rows)`,
				``)},
		{"bool and int8", Result{Columns: []Column{{"ok", tuple.Bool}, {"big", tuple.Int8}},
			Rows: [][]tuple.Datum{{true, int64(-3000000000)}, {false, nil}, {nil, int64(0)}}},
			lines(` ok |     big     `,
				`----+-------------`,
				` t  | -3000000000`,
				` f  |            `,
				`    |           0`,
				`(3 rows)`,
				``)},
		{"header centred", Result{Columns: []Column{{"id", tuple.Int4}, {"n", tuple.Text}},
			Rows: [][]tuple.Datum{{int32(12345), "hello world"}}},
			lines(`  id   |      n      `,
				`-------+-------------`,
				` 12345 | hello world`,
				`(1 row)`,
				``)},
		{"multibyte width", Result{Columns: []Column{{"s", tuple.Text}, {"n", tuple.Int4}},
			Rows: [][]tuple.Datum{{"ü", int32(1)}, {"abc", int32(2)}}},
			lines(`  s  | n `,
				`-----+---`,
				` ü   | 1`,
				` abc | 2`,
				`(2 rows)`,
				``)},
		{"title", Result{Title: "List of relations", Columns: []Column{{"Name", tuple.Text}, {"Type", tuple.Text}},
			Rows: [][]tuple.Datum{{"t", "table"}}},
			lines(`List of relations`,
				` Name | Type  `,
				`------+-------`,
				` t    | table`,
				`(1 row)`,
				``)},
		{"title centred, no count", Result{Title: `Table "t"`, NoCount: true,
			Columns: []Column{{"Column", tuple.Text}, {"Type", tuple.Text}, {"Nullable", tuple.Text}},
			Rows:    [][]tuple.Datum{{"a", "integer", "not null"}, {"b", "text", ""}}},
			lines(`          Table "t"`,
				` Column |  Type   | Nullable `,
				`--------+---------+----------`,
				` a      | integer | not null`,
				` b      | text    | `,
				``)},
	}
	for _, c := range cases {
		if got := c.res.String(); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

func TestFormatDatum(t *testing.T) {
	cases := []struct {
		v    tuple.Datum
		want string
	}{
		{int32(0), "0"}, {int32(-7), "-7"}, {int64(1) << 40, "1099511627776"},
		{true, "t"}, {false, "f"}, {"", ""}, {"a b", "a b"}, {nil, ""},
	}
	for _, c := range cases {
		if got := FormatDatum(c.v); got != c.want {
			t.Errorf("FormatDatum(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		src, stmt, rest string
		ok              bool
	}{
		{"select 1; select 2", "select 1;", " select 2", true},
		{"  select 1;", "select 1;", "", true},
		{"select 1;\n", "select 1;", "\n", true},
		{"-- c\nselect 1;\n", "select 1;", "\n", true},
		{"\n\n  -- a\n -- b\nselect\n1;", "select\n1;", "", true},
		{"/* ; */ select 1;", "/* ; */ select 1;", "", true},
		{"select ';';", "select ';';", "", true},
		{`select "a;b" from t;`, `select "a;b" from t;`, "", true},
		{"select 1 -- ;\n;", "select 1 -- ;\n;", "", true},
		{";", ";", "", true},
		{"select 1", "", "", false},
		{"select ';", "", "", false},
		{"select 1 /* ; ", "", "", false},
		{"-- only a comment\n", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		stmt, rest, ok := Split(c.src)
		if stmt != c.stmt || rest != c.rest || ok != c.ok {
			t.Errorf("Split(%q) = %q, %q, %v; want %q, %q, %v", c.src, stmt, rest, ok, c.stmt, c.rest, c.ok)
		}
	}
}

func TestDescribe(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4 primary key, b text, c bool not null, d int8)")
	exec(t, s, `create table "Mixed" (x int4)`)
	r, err := s.Describe("t")
	if err != nil {
		t.Fatal(err)
	}
	want := lines(`          Table "t"`,
		` Column |  Type   | Nullable `,
		`--------+---------+----------`,
		` a      | integer | not null`,
		` b      | text    | `,
		` c      | boolean | not null`,
		` d      | bigint  | `,
		``)
	if got := r.String(); got != want {
		t.Errorf("\\d t =\n%s\nwant\n%s", got, want)
	}
	r, err = s.Describe("pg_class")
	if err != nil || len(r.Rows) != 5 || r.Title != `Table "pg_class"` {
		t.Errorf("\\d pg_class: %+v, %v", r, err)
	}
	_, err = s.Describe("nope")
	var e *Error
	if !errors.As(err, &e) || e.Msg != `Did not find any relation named "nope".` || !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("\\d nope: %#v", err)
	}

	r, err = s.Tables()
	if err != nil {
		t.Fatal(err)
	}
	want = lines(`List of relations`,
		` Name  | Type  `,
		`-------+-------`,
		` Mixed | table`,
		` t     | table`,
		`(2 rows)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("\\dt =\n%s\nwant\n%s", got, want)
	}
}

func TestExplain(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4, b text)")
	r := exec(t, s, "explain select a from t where a > 1 order by b limit 2")
	if r.Tag != "EXPLAIN" || !reflect.DeepEqual(r.Columns, []Column{{"QUERY PLAN", tuple.Text}}) {
		t.Errorf("explain result: %+v", r)
	}
	want := lines(`           QUERY PLAN            `,
		`---------------------------------`,
		` Limit`,
		`   ->  Sort`,
		`         Sort Key: t.b`,
		`         ->  Seq Scan on t`,
		`               Filter: (t.a > 1)`,
		`(5 rows)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("explain =\n%s\nwant\n%s", got, want)
	}
	// EXPLAIN plans without running: the table stays empty.
	exec(t, s, "explain insert into t values (1, 'x')")
	if r := exec(t, s, "select * from t"); len(r.Rows) != 0 {
		t.Error("EXPLAIN INSERT inserted")
	}
	if _, err := s.Exec("explain select * from nope"); !errors.Is(err, analyzer.ErrUndefinedTable) {
		t.Errorf("explain of bad query: %v", err)
	}
}
