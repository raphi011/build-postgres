// Package txn allocates transaction IDs and records in the commit log
// whether each transaction committed or aborted. See
// chapters/16-transactions.
package txn

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/raphi011/build-postgres/internal/tuple"
)

// Status is the state of a transaction as the commit log records it.
// The values are the two-bit codes stored in pg_xact.
// PostgreSQL: TRANSACTION_STATUS_* in clog.h.
type Status byte

const (
	InProgress Status = 0
	Committed  Status = 1
	Aborted    Status = 2
)

func (s Status) String() string {
	switch s {
	case InProgress:
		return "in progress"
	case Committed:
		return "committed"
	case Aborted:
		return "aborted"
	}
	return fmt.Sprintf("Status(%d)", byte(s))
}

// Commit log layout: two bits per transaction, four transactions per
// byte, 8 KiB pages, 32 pages per segment file named after the segment
// number in four hex digits.
// PostgreSQL: CLOG_* in clog.c, SLRU_PAGES_PER_SEGMENT in slru.h.
const (
	BitsPerXact     = 2
	XactsPerByte    = 8 / BitsPerXact
	PageSize        = 8192
	XactsPerPage    = PageSize * XactsPerByte
	PagesPerSegment = 32
	XactsPerSegment = XactsPerPage * PagesPerSegment
	SegmentSize     = PageSize * PagesPerSegment
)

// Dir is the commit log directory under the data directory.
const Dir = "pg_xact"

var (
	ErrFinished  = errors.New("txn: transaction has already finished")
	ErrFutureXID = errors.New("txn: transaction ID has not been assigned yet")
)

// Manager hands out transaction IDs and owns the commit log. One per data
// directory. Safe for concurrent use.
// PostgreSQL: TransamVariables in varsup.c, the clog SLRU in clog.c.
type Manager struct {
	dir string

	mu       sync.Mutex
	next     tuple.XID
	running  map[tuple.XID]bool
	segments map[uint32]*os.File
}

// Transaction is one transaction between Begin and Commit or Abort.
type Transaction struct {
	XID tuple.XID
	m   *Manager
}

// Open opens the commit log of the data directory at dir, creating and
// syncing pg_xact/ if missing, and loads the next transaction ID from the
// control file. Returns catalog.ErrNotBootstrapped if there is no control
// file; catalog.ErrControlCorrupt if it is corrupt.
// PostgreSQL: StartupCLOG in clog.c, the nextXid part of StartupXLOG.
func Open(dir string) (*Manager, error) {
	panic("not implemented")
}

// Close closes the commit log files and forgets the transactions still in
// progress: their handles return ErrFinished from then on, and after a
// reopen they read as aborted.
func (m *Manager) Close() error {
	panic("not implemented")
}

// NextXID returns the transaction ID the next Begin will hand out.
func (m *Manager) NextXID() tuple.XID {
	panic("not implemented")
}

// Begin starts a transaction with the next transaction ID. The control
// file is rewritten with the ID after it before the transaction is
// returned, so IDs never repeat across restarts.
// PostgreSQL: GetNewTransactionId in varsup.c, StartTransaction in xact.c.
func (m *Manager) Begin() (*Transaction, error) {
	panic("not implemented")
}

// Commit records the transaction as committed and syncs the commit log
// before returning, so a commit survives a crash; the caller flushes the
// pages the transaction wrote first, since the log bit is the claim that
// they are there. The segment file is created and its directory synced on
// first use. Returns ErrFinished if the transaction has already committed
// or aborted.
// PostgreSQL: RecordTransactionCommit in xact.c, TransactionIdCommitTree
// in transam.c.
func (t *Transaction) Commit() error {
	panic("not implemented")
}

// Abort records the transaction as aborted. The log is not synced: a
// transaction that is not recorded as committed counts as aborted
// anyway. Returns ErrFinished if the transaction has already finished.
// PostgreSQL: RecordTransactionAbort in xact.c, TransactionIdAbortTree
// in transam.c.
func (t *Transaction) Abort() error {
	panic("not implemented")
}

// Status reports the state of a transaction. InvalidXID is Aborted and
// BootstrapXID and FrozenXID are Committed without consulting the log.
// A transaction that is not in progress in this Manager and has no
// commit or abort recorded is Aborted: it was in progress when the
// process died. Returns ErrFutureXID for an ID Begin has not handed out.
// PostgreSQL: TransactionLogFetch in transam.c, TransactionIdIsInProgress
// in procarray.c.
func (m *Manager) Status(xid tuple.XID) (Status, error) {
	panic("not implemented")
}

// Chapter 17: snapshots and transaction waits.

// Snapshot returns what a snapshot needs from the running set: xmin is
// the oldest running transaction ID (the next ID if none is running),
// xmax the next ID to be handed out, and xip the running IDs in
// ascending order. Every ID below xmin has finished; every ID at or
// above xmax had not started. The three are read under one lock, so
// they agree with each other.
// PostgreSQL: GetSnapshotData in procarray.c.
func (m *Manager) Snapshot() (xmin, xmax tuple.XID, xip []tuple.XID) {
	panic("not implemented")
}

// Wait blocks until the transaction xid has committed or aborted, or the
// manager is closed. It returns at once for an ID that is not running.
// PostgreSQL: XactLockTableWait in lmgr.c.
func (m *Manager) Wait(xid tuple.XID) {
	panic("not implemented")
}

// Waiting returns the number of goroutines blocked in Wait. The
// isolation test runner uses it to tell a step that blocks on another
// session from one that is still running.
// PostgreSQL: pg_isolation_test_session_is_blocked in regress.c.
func (m *Manager) Waiting() int {
	panic("not implemented")
}
