// Package session runs SQL text against a data directory: parse, analyze,
// plan, execute, and format the result the way psql does. See
// chapters/11-executor.
package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/index"
	"github.com/raphi011/build-postgres/internal/mvcc"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/planner"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/sql/analyzer"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/sql/query"
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
	store, err := smgr.Open(dir)
	if err != nil {
		return nil, err
	}
	pool := bufmgr.New(store, NFrames)
	_, err = catalog.Open(pool)
	if errors.Is(err, catalog.ErrNotBootstrapped) {
		_, err = catalog.Bootstrap(pool)
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	tm, err := txn.Open(dir)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Cluster{store: store, pool: pool, txn: tm}, nil
}

// Connect opens a session on the cluster with its own catalog cache and
// transaction state.
// PostgreSQL: InitPostgres in postinit.c.
func (c *Cluster) Connect() (*Session, error) {
	cat, err := catalog.Open(c.pool)
	if err != nil {
		return nil, err
	}
	s := &Session{cluster: c, cat: cat}
	cat.SetSnapshot(s.catalogSnapshot)
	return s, nil
}

// flush writes every dirty page and syncs the relation files. A commit
// calls it before recording its verdict: without a WAL the commit log
// bit is the only durable record of a commit (D11, D23), so everything
// it vouches for has to be on disk first. PostgreSQL flushes a few WAL
// records instead and leaves the pages for a checkpoint.
func (c *Cluster) flush() error {
	if err := c.pool.FlushAll(); err != nil {
		return err
	}
	return c.store.SyncAll()
}

// Close flushes every dirty page and closes the commit log and the data
// directory. Sessions still connected must not be used afterwards.
// Returns the first error.
func (c *Cluster) Close() error {
	err := c.pool.FlushAll()
	if terr := c.txn.Close(); err == nil {
		err = terr
	}
	if cerr := c.store.Close(); err == nil {
		err = cerr
	}
	return err
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

// sessionLog is the session's view of the transaction manager: Wait
// records that the session is blocked, for Blocked.
type sessionLog struct {
	*txn.Manager
	s *Session
}

func (l sessionLog) Wait(xid tuple.XID) {
	l.s.waiting.Store(true)
	l.Manager.Wait(xid)
	l.s.waiting.Store(false)
}

// Blocked reports whether the session is waiting for another session's
// transaction to finish. The isolation test runner uses it to tell a
// step that blocks from one that is still running.
// PostgreSQL: pg_isolation_test_session_is_blocked in regress.c.
func (s *Session) Blocked() bool { return s.waiting.Load() }

// snapshot takes a snapshot for xid whose waits count as the session's.
func (s *Session) snapshot(xid tuple.XID) *mvcc.Snapshot {
	snap := mvcc.Take(s.cluster.txn, xid)
	snap.Log = sessionLog{s.cluster.txn, s}
	return snap
}

// Open opens the data directory at dir as a cluster of its own,
// bootstrapping it first if it has no control file, and connects one
// session to it, which closes the cluster with Close. Any other smgr or
// catalog error is returned as is.
// PostgreSQL: InitPostgres in postinit.c.
func Open(dir string) (*Session, error) {
	c, err := OpenCluster(dir)
	if err != nil {
		return nil, err
	}
	s, err := c.Connect()
	if err != nil {
		c.Close()
		return nil, err
	}
	s.owner = true
	return s, nil
}

// Catalog returns the session's catalog.
func (s *Session) Catalog() *catalog.Catalog { return s.cat }

// Close aborts a transaction left open and, for a session opened with
// Open, closes the cluster. Returns the first error.
// PostgreSQL: ShutdownPostgres in postinit.c.
func (s *Session) Close() error {
	var err error
	if s.tx != nil && !s.failed {
		err = s.tx.Abort()
	}
	if s.tx != nil {
		if ferr := s.endTransaction(false); err == nil {
			err = ferr
		}
	}
	if s.owner {
		if cerr := s.cluster.Close(); err == nil {
			err = cerr
		}
	}
	return err
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
// ErrInFailedTransaction until one of them ends the block. Every commit,
// implicit or explicit, flushes the cluster first, so a committed
// statement survives losing the process without a Close.
// PostgreSQL: exec_simple_query in postgres.c.
func (s *Session) Exec(sql string) ([]*Result, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, wrap(err, sql)
	}
	var results []*Result
	for _, stmt := range stmts {
		var r *Result
		var err error
		switch stmt.(type) {
		case *ast.Begin, *ast.Commit, *ast.Rollback:
			r, err = s.control(stmt)
		default:
			r, err = s.statement(stmt)
		}
		if err != nil {
			return results, wrap(err, sql)
		}
		results = append(results, r)
	}
	return results, nil
}

// statement runs one statement other than transaction control inside the
// current transaction, starting one if none is open, with a snapshot
// taken now under READ COMMITTED or at the transaction's first statement
// under REPEATABLE READ.
// PostgreSQL: start_xact_command and finish_xact_command in postgres.c,
// GetTransactionSnapshot in snapmgr.c.
func (s *Session) statement(stmt ast.Stmt) (*Result, error) {
	if s.failed {
		return nil, ErrInFailedTransaction
	}
	if s.tx == nil {
		tx, err := s.cluster.txn.Begin()
		if err != nil {
			return nil, err
		}
		s.tx = tx
	}
	if s.snap == nil || s.isolation < ast.RepeatableRead {
		s.snap = s.snapshot(s.tx.XID)
	}
	s.cat.Invalidate()
	q, err := analyzer.Analyze(stmt, s.cat)
	var r *Result
	if err == nil {
		r, err = s.run(q)
	}
	if err != nil {
		// The transaction is aborted at once; an explicit block stays
		// open, refusing everything until COMMIT or ROLLBACK.
		if aerr := s.tx.Abort(); aerr != nil {
			return nil, aerr
		}
		if s.explicit {
			s.failed = true
		} else if ferr := s.endTransaction(false); ferr != nil {
			return nil, ferr
		}
		return nil, err
	}
	if !s.explicit {
		if err := s.cluster.flush(); err != nil {
			return nil, err
		}
		if err := s.tx.Commit(); err != nil {
			return nil, err
		}
		if err := s.endTransaction(true); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// endTransaction returns the session to the idle state once the
// transaction's verdict is recorded, and removes the relation files it
// dropped (on commit) or created (on abort).
// PostgreSQL: CommitTransaction and AbortTransaction in xact.c.
func (s *Session) endTransaction(commit bool) error {
	s.tx, s.explicit, s.failed = nil, false, false
	s.isolation, s.snap = ast.DefaultIsolation, nil
	return s.cat.EndTransaction(commit)
}

// control runs BEGIN, COMMIT, or ROLLBACK.
// PostgreSQL: BeginTransactionBlock, EndTransactionBlock, and
// UserAbortTransactionBlock in xact.c.
func (s *Session) control(stmt ast.Stmt) (*Result, error) {
	switch stmt := stmt.(type) {
	case *ast.Begin:
		if s.failed {
			return nil, ErrInFailedTransaction
		}
		if s.explicit {
			return &Result{Tag: "BEGIN", Warnings: []string{WarnAlreadyTransaction}}, nil
		}
		tx, err := s.cluster.txn.Begin()
		if err != nil {
			return nil, err
		}
		s.tx, s.explicit = tx, true
		s.isolation = stmt.Isolation
		if s.isolation == ast.DefaultIsolation {
			s.isolation = ast.ReadCommitted
		}
		return &Result{Tag: "BEGIN"}, nil
	case *ast.Commit:
		if !s.explicit {
			return &Result{Tag: "COMMIT", Warnings: []string{WarnNoTransaction}}, nil
		}
		if s.failed {
			return &Result{Tag: "ROLLBACK"}, s.endTransaction(false)
		}
		if err := s.cluster.flush(); err != nil {
			return nil, err
		}
		if err := s.tx.Commit(); err != nil {
			return nil, err
		}
		return &Result{Tag: "COMMIT"}, s.endTransaction(true)
	default:
		if !s.explicit {
			return &Result{Tag: "ROLLBACK", Warnings: []string{WarnNoTransaction}}, nil
		}
		if !s.failed {
			if err := s.tx.Abort(); err != nil {
				return nil, err
			}
		}
		return &Result{Tag: "ROLLBACK"}, s.endTransaction(false)
	}
}

// catalogSnapshot is the snapshot catalog reads take: a fresh one, so
// that DDL other sessions committed is seen at once, even inside a
// REPEATABLE READ transaction, as PostgreSQL's catalog snapshot does.
// Outside a transaction (\d) it belongs to no transaction.
// PostgreSQL: GetCatalogSnapshot in snapmgr.c.
func (s *Session) catalogSnapshot() heap.Snapshot {
	xid := tuple.InvalidXID
	if s.tx != nil {
		xid = s.tx.XID
	}
	return s.snapshot(xid)
}

// xid returns the ID of the current transaction.
func (s *Session) xid() tuple.XID { return s.tx.XID }

// run executes one bound statement.
func (s *Session) run(q query.Stmt) (*Result, error) {
	switch q := q.(type) {
	case *query.CreateTable:
		oid, err := s.cat.CreateTable(q.Name, q.Desc, s.xid())
		if err != nil {
			return nil, ddlError(err, q.Name, q.Desc)
		}
		if q.PrimaryKey >= 0 {
			// The index is part of the same statement: if it cannot be
			// made, PostgreSQL's rollback removes the table too.
			pkey := q.Name + "_pkey"
			if _, err := s.cat.CreateIndex(pkey, oid, q.PrimaryKey, true, true, s.xid()); err != nil {
				if derr := s.cat.DropTable(q.Name, s.xid()); derr != nil {
					return nil, derr
				}
				return nil, indexError(err, pkey, q.Name)
			}
		}
		return &Result{Tag: "CREATE TABLE"}, nil
	case *query.DropTable:
		if err := s.cat.DropTable(q.Name, s.xid()); err != nil {
			return nil, ddlError(err, q.Name, nil)
		}
		return &Result{Tag: "DROP TABLE"}, nil
	case *query.CreateIndex:
		idx, err := s.cat.CreateIndex(q.Name, q.Rel.OID, q.Attr, q.Unique, false, s.xid())
		if err != nil {
			return nil, indexError(err, q.Name, q.Rel.Name)
		}
		rel, err := s.cat.Lookup(q.Rel.Name)
		if err == nil {
			err = index.Build(s.cluster.pool, rel, idx, s.snap)
		}
		if err != nil {
			// A failed build leaves nothing behind (D20).
			if derr := s.cat.DropIndex(q.Name, s.xid()); derr != nil {
				return nil, derr
			}
			return nil, err
		}
		return &Result{Tag: "CREATE INDEX"}, nil
	case *query.DropIndex:
		if err := s.cat.DropIndex(q.Name, s.xid()); err != nil {
			var table string
			if errors.Is(err, catalog.ErrDependentObjects) {
				idx, lerr := s.cat.LookupIndex(q.Name)
				if lerr != nil {
					return nil, lerr
				}
				rel, lerr := s.cat.LookupOID(idx.Rel)
				if lerr != nil {
					return nil, lerr
				}
				table = rel.Name
			}
			return nil, indexError(err, q.Name, table)
		}
		return &Result{Tag: "DROP INDEX"}, nil
	case *query.Explain:
		p, err := planner.Plan(q.Stmt)
		if err != nil {
			return nil, err
		}
		lines := plan.ExplainCosts(p)
		if q.CostsOff {
			lines = plan.Explain(p)
		}
		r := &Result{Tag: "EXPLAIN", Columns: []Column{{"QUERY PLAN", tuple.Text}}}
		for _, line := range lines {
			r.Rows = append(r.Rows, []tuple.Datum{line})
		}
		return r, nil
	case *query.Analyze:
		rels := []*catalog.RelationInfo{q.Rel}
		if q.Rel == nil {
			var err error
			if rels, err = s.cat.Tables(); err != nil {
				return nil, err
			}
		}
		for _, rel := range rels {
			if err := s.analyze(rel); err != nil {
				return nil, err
			}
		}
		return &Result{Tag: "ANALYZE"}, nil
	}
	p, err := planner.Plan(q)
	if err != nil {
		return nil, err
	}
	rows, n, err := executor.Exec(p, &executor.Env{Pool: s.cluster.pool, XID: s.xid(), Snapshot: s.snap, Isolation: s.isolation})
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

// analyze counts a table's pages and live tuples and records them, and
// the sizes of its indexes, in pg_class. Anything but a table is skipped,
// as PostgreSQL does with a warning.
// PostgreSQL: do_analyze_rel in analyze.c.
func (s *Session) analyze(rel *catalog.RelationInfo) error {
	if rel.Kind != catalog.RelKindTable {
		return nil
	}
	h := heap.Open(s.cluster.pool, rel.OID, rel.Desc)
	pages, err := h.NBlocks()
	if err != nil {
		return err
	}
	var tuples int64
	sc := h.Scan(s.snap)
	for sc.Next() {
		tuples++
	}
	sc.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	if err := s.cat.UpdateStats(rel.OID, int32(pages), tuples, s.xid()); err != nil {
		return err
	}
	for _, idx := range rel.Indexes {
		n, err := s.cluster.store.NBlocks(idx.OID)
		if err != nil {
			return err
		}
		if err := s.cat.UpdateStats(idx.OID, int32(n), tuples, s.xid()); err != nil {
			return err
		}
	}
	return nil
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
	case errors.Is(err, catalog.ErrWrongObjectType):
		return &Error{Msg: fmt.Sprintf("%q is not a table", name), Err: err}
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

// indexError words a catalog error from index DDL as PostgreSQL does.
// table is the indexed table, named by the system catalog message.
func indexError(err error, name, table string) error {
	switch {
	case errors.Is(err, catalog.ErrExists):
		return &Error{Msg: fmt.Sprintf("relation %q already exists", name), Err: err}
	case errors.Is(err, catalog.ErrNotFound):
		return &Error{Msg: fmt.Sprintf("index %q does not exist", name), Err: err}
	case errors.Is(err, catalog.ErrWrongObjectType):
		return &Error{Msg: fmt.Sprintf("%q is not an index", name), Err: err}
	case errors.Is(err, catalog.ErrDependentObjects):
		return &Error{Msg: fmt.Sprintf("cannot drop index %s because constraint %s on table %s requires it",
			name, name, table), Err: err}
	case errors.Is(err, catalog.ErrSystemTable):
		return &Error{Msg: fmt.Sprintf("permission denied: %q is a system catalog", table), Err: err}
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
		idxErr  *index.Error
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
	case errors.As(err, &idxErr):
		e.Msg = idxErr.Msg
	case errors.Is(err, ErrInFailedTransaction):
		e.Msg = "current transaction is aborted, commands ignored until end of transaction block"
	case errors.Is(err, heap.ErrAlreadyDeleted):
		e.Msg = "tuple concurrently updated"
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
	if info.Kind == catalog.RelKindIndex {
		return s.describeIndex(name)
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
	if len(info.Indexes) > 0 {
		r.Footer = append(r.Footer, "Indexes:")
		for _, idx := range info.Indexes {
			kind := ""
			switch {
			case idx.Primary:
				kind = "PRIMARY KEY, "
			case idx.Unique:
				kind = "UNIQUE, "
			}
			r.Footer = append(r.Footer, fmt.Sprintf("    %q %sbtree (%s)", idx.Name, kind, info.Desc.Attrs[idx.Attr].Name))
		}
	}
	return r, nil
}

// describeIndex returns the \d output for an index.
// PostgreSQL: describeOneTableDetails in describe.c.
func (s *Session) describeIndex(name string) (*Result, error) {
	idx, err := s.cat.LookupIndex(name)
	if err != nil {
		return nil, err
	}
	table, err := s.cat.LookupOID(idx.Rel)
	if err != nil {
		return nil, err
	}
	col := table.Desc.Attrs[idx.Attr]
	kind := ""
	switch {
	case idx.Primary:
		kind = "primary key, "
	case idx.Unique:
		kind = "unique, "
	}
	return &Result{
		Title:   fmt.Sprintf("Index %q", name),
		NoCount: true,
		Columns: []Column{{"Column", tuple.Text}, {"Type", tuple.Text}, {"Key?", tuple.Text}, {"Definition", tuple.Text}},
		Rows:    [][]tuple.Datum{{col.Name, sqlTypeName(col.Type), "yes", col.Name}},
		Footer:  []string{fmt.Sprintf("%sbtree, for table %q", kind, table.Name)},
	}, nil
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
		if info.OID >= catalog.FirstUserOID && info.Kind == catalog.RelKindTable {
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
	// Warnings are the messages PostgreSQL reports at WARNING level
	// while running the statement; the statement still succeeded.
	Warnings []string
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
	for _, line := range r.Footer {
		b.WriteString(line + "\n")
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
