// Package txn allocates transaction IDs and records in the commit log
// whether each transaction committed or aborted. See
// chapters/16-transactions.
package txn

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/raphi011/build-postgres/internal/catalog"
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
	ctl, err := catalog.ReadControl(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		return nil, err
	}
	return &Manager{
		dir:      dir,
		next:     ctl.NextXID,
		running:  map[tuple.XID]bool{},
		segments: map[uint32]*os.File{},
	}, nil
}

// Close closes the commit log files and forgets the transactions still in
// progress: their handles return ErrFinished from then on, and after a
// reopen they read as aborted.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.running)
	var first error
	for seg, f := range m.segments {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(m.segments, seg)
	}
	return first
}

// NextXID returns the transaction ID the next Begin will hand out.
func (m *Manager) NextXID() tuple.XID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next
}

// Begin starts a transaction with the next transaction ID. The control
// file is rewritten with the ID after it before the transaction is
// returned, so IDs never repeat across restarts.
// PostgreSQL: GetNewTransactionId in varsup.c, StartTransaction in xact.c.
func (m *Manager) Begin() (*Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	xid := m.next
	err := catalog.UpdateControl(m.dir, func(c *catalog.Control) {
		c.NextXID = xid + 1
	})
	if err != nil {
		return nil, err
	}
	m.next = xid + 1
	m.running[xid] = true
	return &Transaction{XID: xid, m: m}, nil
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
	return t.finish(Committed, true)
}

// Abort records the transaction as aborted. The log is not synced: a
// transaction that is not recorded as committed counts as aborted
// anyway. Returns ErrFinished if the transaction has already finished.
// PostgreSQL: RecordTransactionAbort in xact.c, TransactionIdAbortTree
// in transam.c.
func (t *Transaction) Abort() error {
	return t.finish(Aborted, false)
}

func (t *Transaction) finish(status Status, sync bool) error {
	m := t.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running[t.XID] {
		return ErrFinished
	}
	if err := m.set(t.XID, status, sync); err != nil {
		return err
	}
	delete(m.running, t.XID)
	return nil
}

// Status reports the state of a transaction. InvalidXID is Aborted and
// BootstrapXID and FrozenXID are Committed without consulting the log.
// A transaction that is not in progress in this Manager and has no
// commit or abort recorded is Aborted: it was in progress when the
// process died. Returns ErrFutureXID for an ID Begin has not handed out.
// PostgreSQL: TransactionLogFetch in transam.c, TransactionIdIsInProgress
// in procarray.c.
func (m *Manager) Status(xid tuple.XID) (Status, error) {
	switch xid {
	case tuple.InvalidXID:
		return Aborted, nil
	case tuple.BootstrapXID, tuple.FrozenXID:
		return Committed, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if xid >= m.next {
		return 0, ErrFutureXID
	}
	if m.running[xid] {
		return InProgress, nil
	}
	b, err := m.get(xid)
	if err != nil {
		return 0, err
	}
	if b == InProgress {
		return Aborted, nil
	}
	return b, nil
}

// segment returns the open file of the segment holding xid, opening or
// creating it on first use. m.mu must be held.
func (m *Manager) segment(xid tuple.XID) (*os.File, error) {
	seg := uint32(xid) / XactsPerSegment
	if f, ok := m.segments[seg]; ok {
		return f, nil
	}
	name := filepath.Join(m.dir, Dir, fmt.Sprintf("%04X", seg))
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	m.segments[seg] = f
	return f, nil
}

// offset returns the byte of xid within its segment and the shift of its
// two bits within that byte.
func offset(xid tuple.XID) (off int64, shift uint) {
	x := uint32(xid) % XactsPerSegment
	return int64(x / XactsPerByte), uint(x%XactsPerByte) * BitsPerXact
}

// get reads the two bits of xid; a byte past the end of the segment file
// reads as zero. m.mu must be held.
func (m *Manager) get(xid tuple.XID) (Status, error) {
	f, err := m.segment(xid)
	if err != nil {
		return 0, err
	}
	off, shift := offset(xid)
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil && err != io.EOF {
		return 0, err
	}
	return Status(b[0]>>shift) & 3, nil
}

// set writes the two bits of xid, extending the segment file if needed,
// and syncs the file when sync is set. m.mu must be held.
func (m *Manager) set(xid tuple.XID, s Status, sync bool) error {
	f, err := m.segment(xid)
	if err != nil {
		return err
	}
	off, shift := offset(xid)
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil && err != io.EOF {
		return err
	}
	b[0] = b[0]&^(3<<shift) | byte(s)<<shift
	if _, err := f.WriteAt(b[:], off); err != nil {
		return err
	}
	if sync {
		return f.Sync()
	}
	return nil
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
