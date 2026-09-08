// Package session runs SQL text against a data directory: parse, analyze,
// plan, execute, and format the result the way psql does. See
// chapters/11-executor.
package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/planner"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/sql/analyzer"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// NFrames is the size of a session's buffer pool.
const NFrames = 256

// Session is one connection to a data directory. It is not safe for
// concurrent use; chapter 17 adds concurrent sessions.
type Session struct {
	store *smgr.DataDir
	pool  *bufmgr.Pool
	cat   *catalog.Catalog
	xid   tuple.XID
}

// Open opens the data directory at dir, bootstrapping it first if it has
// no control file. Any other smgr or catalog error is returned as is.
// PostgreSQL: InitPostgres in postinit.c.
func Open(dir string) (*Session, error) {
	store, err := smgr.Open(dir)
	if err != nil {
		return nil, err
	}
	pool := bufmgr.New(store, NFrames)
	cat, err := catalog.Open(pool)
	if errors.Is(err, catalog.ErrNotBootstrapped) {
		cat, err = catalog.Bootstrap(pool)
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Session{store: store, pool: pool, cat: cat, xid: tuple.FrozenXID}, nil
}

// Catalog returns the session's catalog.
func (s *Session) Catalog() *catalog.Catalog { return s.cat }

// Close flushes every dirty page and closes the data directory. Returns
// the first error of the two.
func (s *Session) Close() error {
	err := s.pool.FlushAll()
	if cerr := s.store.Close(); err == nil {
		err = cerr
	}
	return err
}

// Exec runs every statement in sql in order and returns their results.
// It stops at the first error, returning the results so far and the
// error, which is always a *Error: Query is the whole of sql, Pos is set
// for lexer, parser, and analyzer errors, and Err is the original error.
// PostgreSQL: exec_simple_query in postgres.c.
func (s *Session) Exec(sql string) ([]*Result, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, wrap(err, sql)
	}
	var results []*Result
	for _, stmt := range stmts {
		q, err := analyzer.Analyze(stmt, s.cat)
		if err != nil {
			return results, wrap(err, sql)
		}
		r, err := s.run(q)
		if err != nil {
			return results, wrap(err, sql)
		}
		results = append(results, r)
	}
	return results, nil
}

// run executes one bound statement.
func (s *Session) run(q query.Stmt) (*Result, error) {
	switch q := q.(type) {
	case *query.CreateTable:
		if _, err := s.cat.CreateTable(q.Name, q.Desc, s.xid); err != nil {
			return nil, ddlError(err, q.Name, q.Desc)
		}
		return &Result{Tag: "CREATE TABLE"}, nil
	case *query.DropTable:
		if err := s.cat.DropTable(q.Name, s.xid); err != nil {
			return nil, ddlError(err, q.Name, nil)
		}
		return &Result{Tag: "DROP TABLE"}, nil
	case *query.Begin:
		return &Result{Tag: "BEGIN"}, nil
	case *query.Commit:
		return &Result{Tag: "COMMIT"}, nil
	case *query.Rollback:
		return &Result{Tag: "ROLLBACK"}, nil
	case *query.Explain:
		p, err := planner.Plan(q.Stmt)
		if err != nil {
			return nil, err
		}
		r := &Result{Tag: "EXPLAIN", Columns: []Column{{"QUERY PLAN", tuple.Text}}}
		for _, line := range plan.Explain(p) {
			r.Rows = append(r.Rows, []tuple.Datum{line})
		}
		return r, nil
	}
	p, err := planner.Plan(q)
	if err != nil {
		return nil, err
	}
	rows, n, err := executor.Exec(p, &executor.Env{Pool: s.pool, XID: s.xid})
	if err != nil {
		return nil, err
	}
	switch q := q.(type) {
	case *query.Select:
		r := &Result{Tag: "SELECT " + strconv.Itoa(n), Columns: []Column{}}
		for _, t := range q.Targets {
			r.Columns = append(r.Columns, Column{t.Name, t.Expr.Type()})
		}
		for _, row := range rows {
			vals := make([]tuple.Datum, len(row.Values))
			for i, v := range row.Values {
				if !row.Nulls[i] {
					vals[i] = v
				}
			}
			r.Rows = append(r.Rows, vals)
		}
		return r, nil
	case *query.Insert:
		return &Result{Tag: "INSERT 0 " + strconv.Itoa(n)}, nil
	case *query.Update:
		return &Result{Tag: "UPDATE " + strconv.Itoa(n)}, nil
	case *query.Delete:
		return &Result{Tag: "DELETE " + strconv.Itoa(n)}, nil
	}
	return nil, fmt.Errorf("session: unexpected statement %T", q)
}

// ddlError words a catalog error as PostgreSQL does.
func ddlError(err error, name string, desc *tuple.Desc) error {
	switch {
	case errors.Is(err, catalog.ErrExists):
		return &Error{Msg: fmt.Sprintf("relation %q already exists", name), Err: err}
	case errors.Is(err, catalog.ErrNotFound):
		return &Error{Msg: fmt.Sprintf("table %q does not exist", name), Err: err}
	case errors.Is(err, catalog.ErrSystemTable):
		return &Error{Msg: fmt.Sprintf("permission denied: %q is a system catalog", name), Err: err}
	case errors.Is(err, catalog.ErrTooManyColumns):
		return &Error{Msg: fmt.Sprintf("tables can have at most %d columns", catalog.MaxColumns), Err: err}
	case errors.Is(err, catalog.ErrDuplicateColumn):
		seen := map[string]bool{}
		for _, a := range desc.Attrs {
			if seen[a.Name] {
				return &Error{Msg: fmt.Sprintf("column %q specified more than once", a.Name), Err: err}
			}
			seen[a.Name] = true
		}
	}
	return err
}

// wrap turns any error into a *Error for query.
func wrap(err error, sql string) error {
	var e *Error
	switch {
	case errors.As(err, &e):
		e.Query = sql
		return e
	}
	e = &Error{Err: err, Query: sql}
	var (
		lexErr  *lexer.Error
		parsErr *parser.Error
		anaErr  *analyzer.Error
		exprErr *expr.Error
		execErr *executor.Error
	)
	switch {
	case errors.As(err, &lexErr):
		e.Msg, e.Pos = lexErr.Err.Error(), lexErr.Pos
	case errors.As(err, &parsErr):
		e.Pos = parsErr.Pos
		switch {
		case errors.Is(err, parser.ErrUnknownType):
			e.Msg = "type " + parsErr.Found + " does not exist"
		case parsErr.Found == "end of input":
			e.Msg = "syntax error at end of input"
		default:
			e.Msg = "syntax error at or near " + parsErr.Found
		}
	case errors.As(err, &anaErr):
		e.Msg, e.Pos = anaErr.Msg, anaErr.Pos
	case errors.As(err, &exprErr):
		e.Msg = exprErr.Msg
	case errors.As(err, &execErr):
		e.Msg = execErr.Msg
	default:
		e.Msg = err.Error()
	}
	return e
}

// Describe returns the \d output for a table. Returns a *Error wrapping
// catalog.ErrNotFound, worded as psql does, for an unknown table.
// PostgreSQL: describeTableDetails in describe.c.
func (s *Session) Describe(name string) (*Result, error) {
	info, err := s.cat.Lookup(name)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, &Error{Msg: fmt.Sprintf("Did not find any relation named %q.", name), Err: err}
	}
	if err != nil {
		return nil, err
	}
	r := &Result{
		Title:   fmt.Sprintf("Table %q", name),
		NoCount: true,
		Columns: []Column{{"Column", tuple.Text}, {"Type", tuple.Text}, {"Nullable", tuple.Text}},
	}
	for _, a := range info.Desc.Attrs {
		nullable := ""
		if a.NotNull {
			nullable = "not null"
		}
		r.Rows = append(r.Rows, []tuple.Datum{a.Name, sqlTypeName(a.Type), nullable})
	}
	return r, nil
}

// sqlTypeName is the SQL name psql prints for a type.
func sqlTypeName(t tuple.TypeID) string {
	switch t {
	case tuple.Int4:
		return "integer"
	case tuple.Int8:
		return "bigint"
	case tuple.Bool:
		return "boolean"
	case tuple.Text:
		return "text"
	}
	return t.String()
}

// Tables returns the \dt output: every user table, sorted by name.
// PostgreSQL: listTables in describe.c.
func (s *Session) Tables() (*Result, error) {
	infos, err := s.cat.Tables()
	if err != nil {
		return nil, err
	}
	r := &Result{
		Title:   "List of relations",
		Columns: []Column{{"Name", tuple.Text}, {"Type", tuple.Text}},
		Rows:    [][]tuple.Datum{},
	}
	for _, info := range infos {
		if info.OID >= catalog.FirstUserOID {
			r.Rows = append(r.Rows, []tuple.Datum{info.Name, "table"})
		}
	}
	return r, nil
}

// Column is one column of a result set.
type Column struct {
	Name string
	Type tuple.TypeID
}

// Result is the outcome of one statement. Columns is nil for a statement
// that returns no rows. A NULL is a nil Datum.
type Result struct {
	Tag     string // command tag: SELECT 2, INSERT 0 1, CREATE TABLE
	Title   string // heading printed above \d output
	NoCount bool   // omit the (n rows) line, as \d does
	Columns []Column
	Rows    [][]tuple.Datum
	Footer  []string // lines printed after the rows, as \d prints Indexes:
}

// String renders a result set in psql's aligned format, ending with the
// row count line, the Footer lines, and a blank line. It is empty when
// Columns is nil.
// PostgreSQL: print_aligned_text in print.c.
func (r *Result) String() string {
	if r.Columns == nil {
		return ""
	}
	ncols := len(r.Columns)
	cells := make([][]string, len(r.Rows))
	widths := make([]int, ncols)
	for i, c := range r.Columns {
		widths[i] = utf8.RuneCountInString(c.Name)
	}
	for i, row := range r.Rows {
		cells[i] = make([]string, ncols)
		for j, v := range row {
			s := FormatDatum(v)
			cells[i][j] = s
			widths[j] = max(widths[j], utf8.RuneCountInString(s))
		}
	}

	var b strings.Builder
	if r.Title != "" {
		total := ncols - 1
		for _, w := range widths {
			total += w + 2
		}
		pad := max(0, (total-utf8.RuneCountInString(r.Title))/2)
		b.WriteString(strings.Repeat(" ", pad) + r.Title + "\n")
	}

	// Header, each name centred.
	b.WriteByte(' ')
	for i, c := range r.Columns {
		if i > 0 {
			b.WriteString(" | ")
		}
		pad := widths[i] - utf8.RuneCountInString(c.Name)
		b.WriteString(strings.Repeat(" ", pad/2) + c.Name + strings.Repeat(" ", pad-pad/2))
	}
	b.WriteString(" \n")

	// Separator.
	for i, w := range widths {
		if i > 0 {
			b.WriteByte('+')
		}
		b.WriteString(strings.Repeat("-", w+2))
	}
	b.WriteByte('\n')

	// Rows: numbers right-aligned, the rest left-aligned and the last
	// column unpadded.
	for _, row := range cells {
		b.WriteByte(' ')
		for j, s := range row {
			if j > 0 {
				b.WriteString(" | ")
			}
			pad := strings.Repeat(" ", widths[j]-utf8.RuneCountInString(s))
			switch {
			case r.Columns[j].Type == tuple.Int4 || r.Columns[j].Type == tuple.Int8:
				b.WriteString(pad + s)
			case j == ncols-1:
				b.WriteString(s)
			default:
				b.WriteString(s + pad)
			}
		}
		b.WriteByte('\n')
	}

	if !r.NoCount {
		if len(r.Rows) == 1 {
			b.WriteString("(1 row)\n")
		} else {
			b.WriteString("(" + strconv.Itoa(len(r.Rows)) + " rows)\n")
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// FormatDatum renders a value as psql prints it: booleans as t and f,
// NULL as the empty string.
func FormatDatum(v tuple.Datum) string {
	switch v := v.(type) {
	case nil:
		return ""
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		if v {
			return "t"
		}
		return "f"
	case string:
		return v
	}
	return fmt.Sprint(v)
}

// Error is an error from Exec: PostgreSQL's message, the position in
// Query when there is one (Pos.Line > 0), and the underlying error.
type Error struct {
	Msg   string
	Pos   lexer.Pos
	Query string
	Err   error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Report renders the error as psql does: the ERROR line, then when the
// error has a position, the source line and a caret under it.
// PostgreSQL: reportErrorPosition in fe-protocol3.c.
func (e *Error) Report() string {
	out := "ERROR:  " + e.Msg + "\n"
	if e.Pos.Line <= 0 {
		return out
	}
	lines := strings.Split(e.Query, "\n")
	var line string
	if e.Pos.Line <= len(lines) {
		line = lines[e.Pos.Line-1]
	}
	prefix := "LINE " + strconv.Itoa(e.Pos.Line) + ": "
	out += prefix + line + "\n"
	out += strings.Repeat(" ", len(prefix)+e.Pos.Column-1) + "^\n"
	return out
}

// Split cuts the first complete statement off src. A statement ends at
// the first semicolon outside strings, quoted identifiers, and comments;
// ok is false when src has none. Leading whitespace and -- comments are
// dropped from stmt, so positions in it count from the first token's
// line, as psql does; rest is everything after the semicolon.
// PostgreSQL: psql_scan in psqlscan.l.
func Split(src string) (stmt, rest string, ok bool) {
	// A hand-written scanner rather than the lexer: the lexer's error
	// is sticky, and an incomplete statement must not look like a
	// lexical error.
	end := -1
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\'', '"':
			q := src[i]
			j := strings.IndexByte(src[i+1:], q)
			if j < 0 {
				return "", "", false
			}
			i += j + 1
		case '-':
			if strings.HasPrefix(src[i:], "--") {
				j := strings.IndexByte(src[i:], '\n')
				if j < 0 {
					i = len(src)
				} else {
					i += j
				}
			}
		case '/':
			if !strings.HasPrefix(src[i:], "/*") {
				continue
			}
			depth := 1
			i += 2
			for depth > 0 {
				if i >= len(src) {
					return "", "", false
				}
				switch {
				case strings.HasPrefix(src[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(src[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			i--
		case ';':
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return "", "", false
	}
	stmt, rest = src[:end+1], src[end+1:]

	// Drop leading whitespace and -- comment lines.
	for {
		stmt = strings.TrimLeft(stmt, " \t\r\n")
		if !strings.HasPrefix(stmt, "--") {
			break
		}
		j := strings.IndexByte(stmt, '\n')
		if j < 0 {
			stmt = ""
			break
		}
		stmt = stmt[j+1:]
	}
	return stmt, rest, true
}
