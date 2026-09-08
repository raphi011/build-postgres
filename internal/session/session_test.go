package session

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/index"
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
	exec(t, s, "create unique index t_a_key on t (a)")
	exec(t, s, "create index i on t (b)")
	exec(t, s, "create index t_c on t (c)")
	exec(t, s, "create table p (a int4 primary key)")
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
		{"create index j on nope (a)", `relation "nope" does not exist`, 0, 0, analyzer.ErrUndefinedTable},
		{"create index j on t (nope)", `column "nope" does not exist`, 0, 0, analyzer.ErrUndefinedColumn},
		{"create index j on i (a)", `"i" is an index`, 0, 0, analyzer.ErrWrongObjectType},
		{"create index t_a_key on t (b)", `relation "t_a_key" already exists`, 0, 0, catalog.ErrExists},
		{"create index t on t (b)", `relation "t" already exists`, 0, 0, catalog.ErrExists},
		{"create index j on pg_class (oid)", `permission denied: "pg_class" is a system catalog`, 0, 0, catalog.ErrSystemTable},
		{"create table q (a int4 primary key, b int4 primary key)", `multiple primary keys for table "q" are not allowed`, 0, 0, analyzer.ErrTableDefinition},
		{"drop index nope", `index "nope" does not exist`, 0, 0, catalog.ErrNotFound},
		{"drop index t", `"t" is not an index`, 0, 0, catalog.ErrWrongObjectType},
		{"drop table i", `"i" is not a table`, 0, 0, catalog.ErrWrongObjectType},
		{"drop index p_pkey", `cannot drop index p_pkey because constraint p_pkey on table p requires it`, 0, 0, catalog.ErrDependentObjects},
		{"select * from i", `"i" is an index`, 1, 15, analyzer.ErrWrongObjectType},
		{"insert into i values (1)", `"i" is an index`, 1, 13, analyzer.ErrWrongObjectType},
		{"insert into t values (1, 2, 'y')", `duplicate key value violates unique constraint "t_a_key"`, 0, 0, index.ErrUniqueViolation},
		{"insert into t values (2, 2, '" + strings.Repeat("y", 2700) + "')", `index row size 2712 exceeds btree version 4 maximum 2704 for index "t_c"`, 0, 0, index.ErrTooLarge},
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
		{"footer after the rows", Result{Title: `Table "t"`, NoCount: true,
			Columns: []Column{{"Column", tuple.Text}, {"Type", tuple.Text}},
			Rows:    [][]tuple.Datum{{"a", "integer"}},
			Footer:  []string{"Indexes:", `    "t_pkey" PRIMARY KEY, btree (a)`}},
			lines(`    Table "t"`,
				` Column |  Type   `,
				`--------+---------`,
				` a      | integer`,
				`Indexes:`,
				`    "t_pkey" PRIMARY KEY, btree (a)`,
				``)},
		{"footer after the count", Result{Columns: []Column{{"a", tuple.Int4}}, Rows: [][]tuple.Datum{{int32(1)}},
			Footer: []string{"note"}},
			lines(` a `,
				`---`,
				` 1`,
				`(1 row)`,
				`note`,
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
		`Indexes:`,
		`    "t_pkey" PRIMARY KEY, btree (a)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("\\d t =\n%s\nwant\n%s", got, want)
	}
	// More indexes: the primary key first, then by name; unique ones say so.
	exec(t, s, "create unique index u on t (d)")
	exec(t, s, "create index i on t (b)")
	r, _ = s.Describe("t")
	if got, want := strings.Join(r.Footer, "\n"), "Indexes:\n    \"t_pkey\" PRIMARY KEY, btree (a)\n    \"i\" btree (b)\n    \"u\" UNIQUE, btree (d)"; got != want {
		t.Errorf("\\d t footer =\n%s\nwant\n%s", got, want)
	}
	r, _ = s.Describe("Mixed")
	if r.Footer != nil {
		t.Errorf("\\d of a table without indexes has footer %q", r.Footer)
	}
	// \d of an index.
	r, err = s.Describe("t_pkey")
	if err != nil {
		t.Fatal(err)
	}
	want = lines(`            Index "t_pkey"`,
		` Column |  Type   | Key? | Definition `,
		`--------+---------+------+------------`,
		` a      | integer | yes  | a`,
		`primary key, btree, for table "t"`,
		``)
	if got := r.String(); got != want {
		t.Errorf("\\d t_pkey =\n%s\nwant\n%s", got, want)
	}
	r, _ = s.Describe("u")
	if got := r.String(); !strings.HasSuffix(got, "unique, btree, for table \"t\"\n\n") || !strings.Contains(got, ` d      | bigint | yes  | d`) {
		t.Errorf("\\d u =\n%s", got)
	}
	r, _ = s.Describe("i")
	if got := r.String(); !strings.HasSuffix(got, "btree, for table \"t\"\n\n") || strings.Contains(got, "unique") {
		t.Errorf("\\d i =\n%s", got)
	}
	r, err = s.Describe("pg_class")
	if err != nil || len(r.Rows) != 5 || r.Title != `Table "pg_class"` || r.Footer != nil {
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
	// Indexes are not listed.
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

func TestIndexes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := open(t, dir)
	exec(t, s, "create table t (a int4, b text)")
	exec(t, s, "insert into t values (1, 'x'), (1, 'y'), (2, null), (3, null)")

	// A unique index cannot be built over duplicates, and a failed build
	// leaves no index behind.
	_, err := s.Exec("create unique index t_a_key on t (a)")
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, index.ErrUniqueViolation) || e.Msg != `could not create unique index "t_a_key"` {
		t.Fatalf("unique index over duplicates: %#v", err)
	}
	if r := exec(t, s, "select relname from pg_class where relname = 't_a_key'"); len(r.Rows) != 0 {
		t.Errorf("failed index left a pg_class row: %v", r.Rows)
	}
	if r, _ := s.Describe("t"); r.Footer != nil {
		t.Errorf("failed index left a footer: %q", r.Footer)
	}
	// NULLs are no duplicates, so a unique index on b builds.
	if r := exec(t, s, "create unique index t_b_key on t (b)"); r.Tag != "CREATE INDEX" || r.Columns != nil {
		t.Errorf("create index: %+v", r)
	}
	if r := exec(t, s, "create index t_a on t (a)"); r.Tag != "CREATE INDEX" {
		t.Errorf("create index tag %q", r.Tag)
	}

	// The catalogs describe the indexes.
	r := exec(t, s, "select oid, relname, relkind from pg_class where oid >= 16384 order by oid")
	wantRows := [][]tuple.Datum{{int32(16384), "t", "r"}, {int32(16386), "t_b_key", "i"}, {int32(16387), "t_a", "i"}}
	if !reflect.DeepEqual(r.Rows, wantRows) {
		t.Errorf("pg_class rows %v, want %v", r.Rows, wantRows)
	}
	r = exec(t, s, "select indexrelid, indrelid, indkey, indisunique, indisprimary from pg_index order by indexrelid")
	wantRows = [][]tuple.Datum{{int32(16386), int32(16384), int32(2), true, false}, {int32(16387), int32(16384), int32(1), false, false}}
	if !reflect.DeepEqual(r.Rows, wantRows) {
		t.Errorf("pg_index rows %v, want %v", r.Rows, wantRows)
	}

	// The unique index is enforced from now on.
	if _, err := s.Exec("insert into t values (4, 'x')"); !errors.Is(err, index.ErrUniqueViolation) {
		t.Errorf("duplicate insert: %v", err)
	}
	if r := exec(t, s, "insert into t values (4, null), (5, 'z')"); r.Tag != "INSERT 0 2" {
		t.Errorf("insert tag %q", r.Tag)
	}
	if _, err := s.Exec("update t set b = 'z' where a = 1"); !errors.Is(err, index.ErrUniqueViolation) {
		t.Errorf("update to a taken key: %v", err)
	}
	if r := exec(t, s, "update t set b = 'z' where a = 5"); r.Tag != "UPDATE 1" {
		t.Errorf("update to own key: %q", r.Tag)
	}
	exec(t, s, "delete from t where a = 5")
	if r := exec(t, s, "insert into t values (6, 'z')"); r.Tag != "INSERT 0 1" {
		t.Errorf("reinsert of a deleted key: %q", r.Tag)
	}
	if r := exec(t, s, "drop index t_b_key"); r.Tag != "DROP INDEX" || r.Columns != nil {
		t.Errorf("drop index: %+v", r)
	}
	exec(t, s, "insert into t values (7, 'z')")

	// PRIMARY KEY makes <table>_pkey; a taken name fails the whole CREATE
	// TABLE, as PostgreSQL rolls it back.
	exec(t, s, "create index p_pkey on t (a)")
	_, err = s.Exec("create table p (a int4 primary key)")
	if !errors.As(err, &e) || !errors.Is(err, catalog.ErrExists) || e.Msg != `relation "p_pkey" already exists` {
		t.Fatalf("create table with a taken pkey name: %#v", err)
	}
	if _, err := s.Exec("select * from p"); !errors.Is(err, analyzer.ErrUndefinedTable) {
		t.Errorf("table p exists after the failed create: %v", err)
	}
	exec(t, s, "drop index p_pkey")
	exec(t, s, "create table p (a int4 primary key, b text)")
	exec(t, s, "insert into p values (1, 'x')")
	if _, err := s.Exec("insert into p values (1, 'y')"); !errors.As(err, &e) || e.Msg != `duplicate key value violates unique constraint "p_pkey"` {
		t.Errorf("duplicate primary key: %#v", err)
	}
	if _, err := s.Exec("insert into p values (null, 'y')"); !errors.Is(err, analyzer.ErrNotNull) {
		t.Errorf("NULL primary key: %v", err)
	}

	// Everything survives a restart, and DROP TABLE takes the indexes.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	if _, err := s.Exec("insert into p values (1, 'z')"); !errors.Is(err, index.ErrUniqueViolation) {
		t.Errorf("duplicate after reopen: %v", err)
	}
	r, _ = s.Describe("t")
	if got := strings.Join(r.Footer, "\n"); got != "Indexes:\n    \"t_a\" btree (a)" {
		t.Errorf("\\d t after reopen: %q", got)
	}
	exec(t, s, "drop table t")
	exec(t, s, "drop table p")
	r = exec(t, s, "select relname from pg_class where oid >= 16384")
	if len(r.Rows) != 0 {
		t.Errorf("relations left after drops: %v", r.Rows)
	}
	if r := exec(t, s, "select indexrelid from pg_index"); len(r.Rows) != 0 {
		t.Errorf("pg_index rows left after drops: %v", r.Rows)
	}
}

func TestExplain(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4, b text)")
	r := exec(t, s, "explain (costs off) select a from t where a > 1 order by b limit 2")
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
	exec(t, s, "explain (costs off) insert into t values (1, 'x')")
	if r := exec(t, s, "select * from t"); len(r.Rows) != 0 {
		t.Error("EXPLAIN INSERT inserted")
	}
	if _, err := s.Exec("explain select * from nope"); !errors.Is(err, analyzer.ErrUndefinedTable) {
		t.Errorf("explain of bad query: %v", err)
	}
}

// Chapter 14: statistics and costs.

func TestExplainCosts(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4 primary key, b text)")
	// Never analysed: 10 pages assumed, and the index is chosen for an
	// equality.
	r := exec(t, s, "explain select * from t")
	want := lines(`                      QUERY PLAN                      `,
		`------------------------------------------------------`,
		` Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)`,
		`(1 row)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("explain =\n%s\nwant\n%s", got, want)
	}
	r = exec(t, s, "explain select b from t where a = 1")
	want = lines(`                            QUERY PLAN                            `,
		`------------------------------------------------------------------`,
		` Index Scan using t_pkey on t  (cost=0.15..24.26 rows=6 width=32)`,
		`   Index Cond: (t.a = 1)`,
		`(2 rows)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("explain index =\n%s\nwant\n%s", got, want)
	}
	// After ANALYZE of a small table the sequential scan is cheaper.
	exec(t, s, "insert into t values (1, 'x'), (2, 'y'), (3, 'z')")
	exec(t, s, "analyze t")
	r = exec(t, s, "explain select b from t where a = 1")
	want = lines(`                    QUERY PLAN                    `,
		`--------------------------------------------------`,
		` Seq Scan on t  (cost=0.00..1.04 rows=1 width=32)`,
		`   Filter: (t.a = 1)`,
		`(2 rows)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("explain after analyze =\n%s\nwant\n%s", got, want)
	}
	if r := exec(t, s, "explain (costs off) select b from t where a = 1"); !reflect.DeepEqual(r.Rows, [][]tuple.Datum{{"Seq Scan on t"}, {"  Filter: (t.a = 1)"}}) {
		t.Errorf("costs off: %v", r.Rows)
	}
}

func TestAnalyze(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := open(t, dir)
	exec(t, s, "create table t (a int4 primary key, b text)")
	exec(t, s, "insert into t values (1, 'x'), (2, 'y'), (3, 'z')")
	stats := func(name string) []tuple.Datum {
		t.Helper()
		r := exec(t, s, "select relpages, reltuples from pg_class where relname = '"+name+"'")
		if len(r.Rows) != 1 {
			t.Fatalf("pg_class rows for %s: %v", name, r.Rows)
		}
		return r.Rows[0]
	}
	if got := stats("t"); !reflect.DeepEqual(got, []tuple.Datum{int32(0), int64(0)}) {
		t.Errorf("before analyze: %v", got)
	}
	if r := exec(t, s, "analyze t"); r.Tag != "ANALYZE" || r.Columns != nil {
		t.Errorf("analyze: %+v", r)
	}
	// The table's pages and live tuples, and the index's pages (metapage
	// plus one leaf) with the same tuple count.
	if got := stats("t"); !reflect.DeepEqual(got, []tuple.Datum{int32(1), int64(3)}) {
		t.Errorf("after analyze: %v", got)
	}
	if got := stats("t_pkey"); !reflect.DeepEqual(got, []tuple.Datum{int32(2), int64(3)}) {
		t.Errorf("index after analyze: %v", got)
	}
	// Deleted tuples are not counted; the counts follow the data.
	exec(t, s, "delete from t where a = 2")
	exec(t, s, "insert into t values (4, 'w'), (5, 'v')")
	exec(t, s, "analyze t")
	if got := stats("t"); !reflect.DeepEqual(got, []tuple.Datum{int32(1), int64(4)}) {
		t.Errorf("after delete and insert: %v", got)
	}
	// ANALYZE of an index is skipped silently, as PostgreSQL does with a
	// warning; an unknown relation is an error; ANALYZE alone does every
	// table, the catalogs included.
	if r := exec(t, s, "analyze t_pkey"); r.Tag != "ANALYZE" {
		t.Errorf("analyze index: %+v", r)
	}
	if _, err := s.Exec("analyze nope"); !errors.Is(err, analyzer.ErrUndefinedTable) {
		t.Errorf("analyze nope: %v", err)
	}
	if r := exec(t, s, "analyze"); r.Tag != "ANALYZE" {
		t.Errorf("analyze all: %+v", r)
	}
	if got := stats("pg_class"); got[0] != int32(1) || got[1].(int64) < 5 {
		t.Errorf("pg_class after analyze: %v", got)
	}
	// Statistics persist.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	if got := stats("t"); !reflect.DeepEqual(got, []tuple.Datum{int32(1), int64(4)}) {
		t.Errorf("after reopen: %v", got)
	}
}

// TestPlansAgree: whichever scan the planner picks, the rows are the
// same. Random rows with duplicates and NULLs; every predicate is run
// once as written, where the index is eligible, and once as a + 0, which
// forces a sequential scan.
func TestPlansAgree(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4, b text)")
	exec(t, s, "create index t_a on t (a)")
	rng := rand.New(rand.NewSource(7))
	var values []string
	for i := 0; i < 300; i++ {
		a := "null"
		if rng.Intn(10) > 0 {
			a = strconv.Itoa(rng.Intn(50))
		}
		values = append(values, fmt.Sprintf("(%s, 'r%d')", a, i))
	}
	exec(t, s, "insert into t values "+strings.Join(values, ", "))
	for _, pred := range []string{"a = 7", "a > 40", "a >= 10 and a < 15", "a < 3 or a > 47", "a = 7 and b is not null", "1 < a and a <= 2"} {
		indexed := exec(t, s, "select * from t where "+pred+" order by b")
		seq := exec(t, s, "select * from t where "+regexp.MustCompile(`\ba\b`).ReplaceAllString(pred, "(a + 0)")+" order by b")
		if !reflect.DeepEqual(indexed.Rows, seq.Rows) {
			t.Errorf("%s: %d rows through the planner's choice, %d through a sequential scan", pred, len(indexed.Rows), len(seq.Rows))
		}
		if len(indexed.Rows) == 0 {
			t.Errorf("%s matched nothing; weak test", pred)
		}
	}
	// The equality really is planned as an index scan.
	r := exec(t, s, "explain (costs off) select * from t where a = 7")
	if len(r.Rows) == 0 || r.Rows[0][0] != "Index Scan using t_a on t" {
		t.Errorf("a = 7 planned as %v", r.Rows)
	}
}

// Chapter 15: joins.

func TestJoins(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4 primary key, b text)")
	exec(t, s, "create table u (a int4, c int8, d text)")
	exec(t, s, "insert into t values (1, 'x'), (2, 'y'), (3, 'z'), (4, null)")
	exec(t, s, "insert into u values (1, 10, 'p'), (1, 11, 'q'), (3, 30, 'r'), (null, 40, 's'), (5, 50, 't')")
	cases := []struct{ src, want string }{
		// Output columns follow the range table whatever the join order;
		// duplicate names are printed as they are.
		{"select * from t join u on t.a = u.a order by t.a, u.c", lines(
			` a | b | a | c  | d `,
			`---+---+---+----+---`,
			` 1 | x | 1 | 10 | p`,
			` 1 | x | 1 | 11 | q`,
			` 3 | z | 3 | 30 | r`,
			`(3 rows)`, ``)},
		{"select t.b, u.d from t, u where u.a = t.a and u.c > 10 order by 1", lines(
			` b | d `,
			`---+---`,
			` x | q`,
			` z | r`,
			`(2 rows)`, ``)},
		{"select t.a, u.a from t join u on t.a < u.a order by 1, 2", lines(
			` a | a `,
			`---+---`,
			` 1 | 3`,
			` 1 | 5`,
			` 2 | 3`,
			` 2 | 5`,
			` 3 | 5`,
			` 4 | 5`,
			`(6 rows)`, ``)},
		// A NULL key matches nothing; a cross join matches everything.
		{"select u1.c, u2.c from u as u1 join u as u2 on u1.a = u2.a and u1.c < u2.c order by 1, 2", lines(
			` c  | c  `,
			`----+----`,
			` 10 | 11`,
			`(1 row)`, ``)},
		{"select t.b from t join u on true where u.d = 's' order by 1", lines(
			` b `,
			`---`,
			` x`,
			` y`,
			` z`,
			` `,
			`(4 rows)`, ``)},
		{"select t.a as x, t2.a as y from t, t as t2 where t.a = t2.a + 1 order by 1", lines(
			` x | y `,
			`---+---`,
			` 2 | 1`,
			` 3 | 2`,
			` 4 | 3`,
			`(3 rows)`, ``)},
		{"select t.a, u.c, t2.b from t join u on t.a = u.a join t as t2 on t2.a = u.a + 1 order by 1, 2", lines(
			` a | c  | b `,
			`---+----+---`,
			` 1 | 10 | y`,
			` 1 | 11 | y`,
			` 3 | 30 | `,
			`(3 rows)`, ``)},
		{"select * from t join u on t.a = u.c", lines(
			` a | b | a | c | d `,
			`---+---+---+---+---`,
			`(0 rows)`, ``)},
	}
	for _, c := range cases {
		if got := exec(t, s, c.src).String(); got != c.want {
			t.Errorf("%s =\n%s\nwant\n%s", c.src, got, c.want)
		}
	}
	// The result reports the joined row count.
	if r := exec(t, s, "select 1 from t, u"); r.Tag != "SELECT 20" || len(r.Rows) != 20 {
		t.Errorf("cross join: %s, %d rows", r.Tag, len(r.Rows))
	}
	// Errors are the analyzer's.
	for _, c := range []struct {
		src string
		err error
	}{
		{"select a from t, u", analyzer.ErrAmbiguousColumn},
		{"select * from t join u on t.a", analyzer.ErrTypeMismatch},
		{"select * from t join t on true", analyzer.ErrDuplicateAlias},
		{"select * from t join nope on true", analyzer.ErrUndefinedTable},
	} {
		if _, err := s.Exec(c.src); !errors.Is(err, c.err) {
			t.Errorf("%s: %v, want %v", c.src, err, c.err)
		}
	}
}

// TestJoinPlans: the join method changes with what the planner knows
// about the tables.
func TestJoinPlans(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4 primary key, b text)")
	exec(t, s, "create table u (a int4 primary key, c int4)")
	explain := func(sql string) string {
		t.Helper()
		var b strings.Builder
		for _, row := range exec(t, s, "explain (costs off) "+sql).Rows {
			b.WriteString(row[0].(string) + "\n")
		}
		return b.String()
	}
	// Never analysed, both tables are taken to hold ten pages: hashing
	// the smaller side wins.
	join := "select * from t join u on t.a = u.a"
	if got := explain(join); got != lines("Hash Join", "  Hash Cond: (u.a = t.a)", "  ->  Seq Scan on u", "  ->  Hash", "        ->  Seq Scan on t") {
		t.Errorf("never analysed:\n%s", got)
	}
	if got := explain("select * from t, u"); got != lines("Nested Loop", "  ->  Seq Scan on u", "  ->  Materialize", "        ->  Seq Scan on t") {
		t.Errorf("cross join:\n%s", got)
	}
	// With two rows known to be in t, probing u's primary key per row is
	// cheaper than hashing u.
	exec(t, s, "insert into t values (1, 'x'), (2, 'y')")
	exec(t, s, "analyze t")
	if got := explain(join); got != lines("Nested Loop", "  ->  Seq Scan on t", "  ->  Index Scan using u_pkey on u", "        Index Cond: (u.a = t.a)") {
		t.Errorf("small outer:\n%s", got)
	}
	r := exec(t, s, "explain "+join)
	want := lines(`                              QUERY PLAN                              `,
		`----------------------------------------------------------------------`,
		` Nested Loop  (cost=0.15..17.39 rows=2 width=44)`,
		`   ->  Seq Scan on t  (cost=0.00..1.02 rows=2 width=36)`,
		`   ->  Index Scan using u_pkey on u  (cost=0.15..8.17 rows=1 width=8)`,
		`         Index Cond: (u.a = t.a)`,
		`(4 rows)`,
		``)
	if got := r.String(); got != want {
		t.Errorf("explain =\n%s\nwant\n%s", got, want)
	}
	// Both small and analysed: a hash join again.
	exec(t, s, "insert into u values (1, 10), (2, 20), (3, 30)")
	exec(t, s, "analyze")
	if got := explain(join); got != lines("Hash Join", "  Hash Cond: (u.a = t.a)", "  ->  Seq Scan on u", "  ->  Hash", "        ->  Seq Scan on t") {
		t.Errorf("both analysed:\n%s", got)
	}
	if got := exec(t, s, join+" order by t.a").String(); got != lines(
		` a | b | a | c  `,
		`---+---+---+----`,
		` 1 | x | 1 | 10`,
		` 2 | y | 2 | 20`,
		`(2 rows)`, ``) {
		t.Errorf("join =\n%s", got)
	}
}

// TestJoinOracle: for random rows, the rows a join returns are the
// pairs of the cross product that satisfy the qual, whatever plan the
// planner picks; the qual is evaluated in Go as the oracle.
func TestJoinOracle(t *testing.T) {
	s := newSession(t)
	exec(t, s, "create table t (a int4, b text)")
	exec(t, s, "create table u (x int4 primary key, y text)")
	exec(t, s, "create index t_a on t (a)")
	rng := rand.New(rand.NewSource(15))
	var tvals, uvals []string
	for i := 0; i < 200; i++ {
		b := fmt.Sprintf("'s%d'", rng.Intn(5))
		if rng.Intn(8) == 0 {
			b = "null"
		}
		a := strconv.Itoa(rng.Intn(60))
		if rng.Intn(8) == 0 {
			a = "null"
		}
		tvals = append(tvals, fmt.Sprintf("(%s, %s)", a, b))
	}
	for x := 0; x < 50; x++ {
		uvals = append(uvals, fmt.Sprintf("(%d, 's%d')", x, rng.Intn(5)))
	}
	exec(t, s, "insert into t values "+strings.Join(tvals, ", "))
	exec(t, s, "insert into u values "+strings.Join(uvals, ", "))
	trows, urows := exec(t, s, "select * from t").Rows, exec(t, s, "select * from u").Rows
	eq := func(a, b tuple.Datum) bool { return a != nil && b != nil && a == b }
	lt := func(a, b tuple.Datum) bool { return a != nil && b != nil && a.(int32) < b.(int32) }
	preds := []struct {
		sql string
		ok  func(tr, ur []tuple.Datum) bool
	}{
		{"t.a = u.x", func(tr, ur []tuple.Datum) bool { return eq(tr[0], ur[0]) }},
		{"u.x = t.a and t.b = u.y", func(tr, ur []tuple.Datum) bool { return eq(tr[0], ur[0]) && eq(tr[1], ur[1]) }},
		{"t.a < u.x and u.x < t.a + 3", func(tr, ur []tuple.Datum) bool {
			return lt(tr[0], ur[0]) && ur[0] != nil && tr[0] != nil && ur[0].(int32) < tr[0].(int32)+3
		}},
		{"t.a = u.x or t.b = u.y", func(tr, ur []tuple.Datum) bool { return eq(tr[0], ur[0]) || eq(tr[1], ur[1]) }},
		{"t.a + 1 = u.x and t.b <> u.y", func(tr, ur []tuple.Datum) bool {
			return tr[0] != nil && ur[0] != nil && tr[0].(int32)+1 == ur[0].(int32) && tr[1] != nil && ur[1] != nil && tr[1] != ur[1]
		}},
	}
	check := func(sql string, ok func(tr, ur []tuple.Datum) bool) {
		t.Helper()
		var want []string
		for _, tr := range trows {
			for _, ur := range urows {
				if ok(tr, ur) {
					want = append(want, fmt.Sprint(tr, ur))
				}
			}
		}
		var got []string
		for _, row := range exec(t, s, "select * from t, u where "+sql).Rows {
			got = append(got, fmt.Sprint(row[:2], row[2:]))
		}
		sort.Strings(want)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %d rows, oracle %d", sql, len(got), len(want))
		}
		if len(want) == 0 || len(want) == len(trows)*len(urows) {
			t.Errorf("%s: %d of %d pairs; weak test", sql, len(want), len(trows)*len(urows))
		}
	}
	for _, p := range preds {
		check(p.sql, p.ok)
	}
	// With t analysed and filtered down to a row or two, the planner
	// probes u's primary key per outer row instead of hashing.
	exec(t, s, "analyze t")
	filtered := "t.a = u.x and t.b = 's0'"
	if r := exec(t, s, "explain (costs off) select * from t, u where "+filtered); len(r.Rows) < 4 ||
		r.Rows[0][0] != "Nested Loop" || r.Rows[3][0] != "  ->  Index Scan using u_pkey on u" {
		t.Errorf("%s planned as %v", filtered, r.Rows)
	}
	check(filtered, func(tr, ur []tuple.Datum) bool { return eq(tr[0], ur[0]) && tr[1] == "s0" })
}
