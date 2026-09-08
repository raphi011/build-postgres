// Package executor runs plan trees with the iterator model: every node
// implements Open, Next, and Close and pulls rows from its input. See
// chapters/11-executor.
package executor

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Sentinel errors, one per PostgreSQL error class raised at execution.
var (
	ErrNotNull              = errors.New("not-null violation")    // 23502
	ErrInvalidRowCount      = errors.New("invalid row count")     // 2201W
	ErrSerializationFailure = errors.New("serialization failure") // 40001
)

// Error is an execution error with PostgreSQL's message text.
type Error struct {
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Row is what nodes pass up: the values plus, for rows read from a heap,
// the tuple's TID so that ModifyTable can find it again. A join's rows
// have no TID.
type Row struct {
	expr.Row
	TID tuple.TID
}

// Node is an executor node. Open prepares it, Next returns the next row
// and false when there are no more, Rescan restarts an open node so
// that Next yields its rows from the first again, and Close releases
// what Open took. Next must not be called before Open or after it
// returned false, unless Rescan came between. A node whose rows depend
// on an outer row (a parameterised IndexScan) reads it again on Rescan;
// a node that stores what it built (Materialize, the hash table of a
// HashJoin) keeps it.
// PostgreSQL: ExecProcNode and ExecReScan in execProcnode.c and
// execAmi.c.
type Node interface {
	Open() error
	Next() (Row, bool, error)
	Rescan() error
	Close() error
}

// Env is what nodes need from their surroundings: the pool, the
// transaction's ID for the tuples it writes, the snapshot its scans
// read with (nil for the rule of chapters 11 to 16: every tuple whose
// xmax is zero), and the isolation level, which decides what an UPDATE
// or DELETE does with a row another transaction changed first.
type Env struct {
	Pool      *bufmgr.Pool
	XID       tuple.XID
	Snapshot  heap.Snapshot
	Isolation ast.Isolation
}

// Build turns a plan tree into an executor tree. It does no I/O. Panics
// on a plan node it does not know, and on a HashJoin whose Inner is not
// a Hash.
// PostgreSQL: ExecInitNode in execProcnode.c.
func Build(p plan.Node, env *Env) Node {
	panic("not implemented")
}

// Exec runs p to completion: Open, every Next, Close. It returns the
// rows the root produced and the number of rows processed, which is the
// row count for a query and the number of rows written for ModifyTable.
// Close runs even after an error. Errors are the nodes' own: *Error
// wrapping ErrInvalidRowCount for a negative LIMIT, *Error wrapping
// ErrNotNull for a NULL written to a NOT NULL column, and expression
// errors unchanged.
// PostgreSQL: ExecutorStart, ExecutorRun, and ExecutorEnd in execMain.c.
func Exec(p plan.Node, env *Env) (rows []Row, processed int, err error) {
	panic("not implemented")
}
