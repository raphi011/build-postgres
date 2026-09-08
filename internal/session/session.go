// Package session runs SQL text against a data directory: parse, analyze,
// plan, execute, and format the result the way psql does. See
// chapters/11-executor.
package session

import (
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
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
	panic("not implemented")
}

// Catalog returns the session's catalog.
func (s *Session) Catalog() *catalog.Catalog { return s.cat }

// Close flushes every dirty page and closes the data directory. Returns
// the first error of the two.
func (s *Session) Close() error {
	panic("not implemented")
}

// Exec runs every statement in sql in order and returns their results.
// It stops at the first error, returning the results so far and the
// error, which is always a *Error: Query is the whole of sql, Pos is set
// for lexer, parser, and analyzer errors, and Err is the original error.
// PostgreSQL: exec_simple_query in postgres.c.
func (s *Session) Exec(sql string) ([]*Result, error) {
	panic("not implemented")
}

// Describe returns the \d output for a table. Returns a *Error wrapping
// catalog.ErrNotFound, worded as psql does, for an unknown table.
// PostgreSQL: describeTableDetails in describe.c.
func (s *Session) Describe(name string) (*Result, error) {
	panic("not implemented")
}

// Tables returns the \dt output: every user table, sorted by name.
// PostgreSQL: listTables in describe.c.
func (s *Session) Tables() (*Result, error) {
	panic("not implemented")
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
}

// String renders a result set in psql's aligned format, ending with the
// row count line and a blank line. It is empty when Columns is nil.
// PostgreSQL: print_aligned_text in print.c.
func (r *Result) String() string {
	panic("not implemented")
}

// FormatDatum renders a value as psql prints it: booleans as t and f,
// NULL as the empty string.
func FormatDatum(v tuple.Datum) string {
	panic("not implemented")
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
	panic("not implemented")
}

// Split cuts the first complete statement off src. A statement ends at
// the first semicolon outside strings, quoted identifiers, and comments;
// ok is false when src has none. Leading whitespace and -- comments are
// dropped from stmt, so positions in it count from the first token's
// line, as psql does; rest is everything after the semicolon.
// PostgreSQL: psql_scan in psqlscan.l.
func Split(src string) (stmt, rest string, ok bool) {
	panic("not implemented")
}
