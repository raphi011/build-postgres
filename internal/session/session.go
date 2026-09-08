// Package session runs SQL text against a data directory: parse, analyze,
// plan, execute, and format the result the way psql does. See
// chapters/11-executor.
package session

import (
	"errors"
	"sync/atomic"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/mvcc"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/tuple"
	"github.com/raphi011/build-postgres/internal/txn"
)

// NFrames is the size of a session's buffer pool.
const NFrames = 256

// ErrInFailedTransaction is the error of every statement but COMMIT and
// ROLLBACK after an error inside a transaction block (SQLSTATE 25P02).
var ErrInFailedTransaction = errors.New("session: current transaction is aborted")

// Warnings PostgreSQL issues for transaction control statements that
// do not match the session's state. Reported in Result.Warnings; the
// statement still succeeds.
const (
	WarnNoTransaction      = "there is no transaction in progress"
	WarnAlreadyTransaction = "there is already a transaction in progress"
)

// Cluster is one open data directory and what its sessions share: the
// store, the buffer pool, and the transaction manager. Sessions attach
// to it with Connect and may run concurrently.
// PostgreSQL: the postmaster's shared memory.
type Cluster struct {
	store *smgr.DataDir
	pool  *bufmgr.Pool
	txn   *txn.Manager
}

// OpenCluster opens the data directory at dir, bootstrapping it first if
// it has no control file. Any other smgr or catalog error is returned
// as is.
// PostgreSQL: PostmasterMain in postmaster.c.
func OpenCluster(dir string) (*Cluster, error) {
	panic("not implemented")
}

// Connect opens a session on the cluster with its own catalog cache and
// transaction state.
// PostgreSQL: InitPostgres in postinit.c.
func (c *Cluster) Connect() (*Session, error) {
	panic("not implemented")
}

// Close flushes every dirty page and closes the commit log and the data
// directory. Sessions still connected must not be used afterwards.
// Returns the first error.
func (c *Cluster) Close() error {
	panic("not implemented")
}

// Session is one connection to a cluster. A session is not safe for
// concurrent use itself; sessions on one cluster run concurrently, each
// seeing the others' transactions through its snapshots.
type Session struct {
	cluster *Cluster
	owner   bool // Close closes the cluster too
	cat     *catalog.Catalog

	// Transaction state: tx is the current transaction, nil between
	// statements outside a block; explicit is set between BEGIN and
	// COMMIT or ROLLBACK; failed is set after an error inside the block.
	// isolation is the block's level; snap is the snapshot statements
	// read with, taken per statement under READ COMMITTED and once per
	// transaction under REPEATABLE READ.
	tx        *txn.Transaction
	explicit  bool
	failed    bool
	isolation ast.Isolation
	snap      *mvcc.Snapshot
	waiting   atomic.Bool // blocked on another transaction
}

// Blocked reports whether the session is waiting for another session's
// transaction to finish. The isolation test runner uses it to tell a
// step that blocks from one that is still running.
// PostgreSQL: pg_isolation_test_session_is_blocked in regress.c.
func (s *Session) Blocked() bool {
	panic("not implemented")
}

// Open opens the data directory at dir as a cluster of its own,
// bootstrapping it first if it has no control file, and connects one
// session to it, which closes the cluster with Close. Any other smgr or
// catalog error is returned as is.
// PostgreSQL: InitPostgres in postinit.c.
func Open(dir string) (*Session, error) {
	panic("not implemented")
}

// Catalog returns the session's catalog.
func (s *Session) Catalog() *catalog.Catalog { return s.cat }

// Close aborts a transaction left open and, for a session opened with
// Open, closes the cluster. Returns the first error.
// PostgreSQL: ShutdownPostgres in postinit.c.
func (s *Session) Close() error {
	panic("not implemented")
}

// Exec runs every statement in sql in order and returns their results.
// It stops at the first error, returning the results so far and the
// error, which is always a *Error: Query is the whole of sql, Pos is set
// for lexer, parser, and analyzer errors, and Err is the original error.
//
// Each statement outside a transaction block runs in its own
// transaction, committed when it succeeds and aborted when it fails.
// BEGIN opens a block; an error inside it aborts the transaction and
// every later statement but COMMIT and ROLLBACK fails with
// ErrInFailedTransaction until one of them ends the block.
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
	Footer  []string // lines printed after the rows, as \d prints Indexes:
	// Warnings are the messages PostgreSQL reports at WARNING level
	// while running the statement; the statement still succeeded.
	Warnings []string
}

// String renders a result set in psql's aligned format, ending with the
// row count line, the Footer lines, and a blank line. It is empty when
// Columns is nil.
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
