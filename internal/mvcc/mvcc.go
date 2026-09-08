// Package mvcc decides which tuple versions a transaction sees: a
// snapshot of the running transactions, and the visibility rules that
// combine a tuple's xmin and xmax with the snapshot and the commit log.
// See chapters/17-mvcc.
package mvcc

import (
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/tuple"
	"github.com/raphi011/build-postgres/internal/txn"
)

// Log is what a snapshot asks about a transaction it did not see
// finish: the verdict, and a wait for one still running. *txn.Manager
// implements it.
type Log interface {
	Status(xid tuple.XID) (txn.Status, error)
	Wait(xid tuple.XID)
}

// Snapshot is the set of transactions whose effects are visible: those
// that had committed when it was taken, plus the snapshot's own. Every
// ID below Xmin has finished, every ID at or above Xmax had not started,
// and Xip lists the IDs in between that were running, in ascending
// order, the snapshot's own excluded. A committed transaction in Xip or
// at or above Xmax is invisible: it committed after the snapshot.
// PostgreSQL: SnapshotData in snapshot.h.
type Snapshot struct {
	Xmin tuple.XID
	Xmax tuple.XID
	Xip  []tuple.XID
	// Xid is the transaction that took the snapshot; its own writes are
	// visible to it. InvalidXID outside a transaction.
	Xid tuple.XID
	// Log answers for the transactions the snapshot saw finish.
	Log Log
}

// Take returns a snapshot of m's running transactions for the
// transaction xid.
// PostgreSQL: GetSnapshotData in procarray.c.
func Take(m *txn.Manager, xid tuple.XID) *Snapshot {
	panic("not implemented")
}

// String renders the snapshot as pg_current_snapshot does: xmin:xmax:xip,
// the running IDs separated by commas.
func (s *Snapshot) String() string {
	panic("not implemented")
}

// InProgress reports whether xid was running, or had not started, when
// the snapshot was taken, so that its effects are invisible whatever it
// did since. The snapshot's own transaction is never in progress.
// PostgreSQL: XidInMVCCSnapshot in snapmgr.c.
func (s *Snapshot) InProgress(xid tuple.XID) bool {
	panic("not implemented")
}

// Visible reports whether t is visible to the snapshot: its inserting
// transaction is the snapshot's own or committed before the snapshot,
// and its deleting transaction, if any, is neither. Hint bits are read
// first and set on t when the log is consulted; the log is consulted
// only for a transaction the snapshot saw finish, so its verdict is
// final.
// PostgreSQL: HeapTupleSatisfiesMVCC in heapam_visibility.c.
func (s *Snapshot) Visible(t tuple.Tuple) (bool, error) {
	panic("not implemented")
}

// Dirty reports whether t is visible to a unique check, which must see
// what running transactions are doing: a tuple inserted or deleted by
// one is reported with wait set to its ID, and the caller waits for it
// and asks again. Otherwise the answer is what the log says, own and
// committed transactions visible, aborted ones not, regardless of the
// snapshot's bounds.
// PostgreSQL: HeapTupleSatisfiesDirty in heapam_visibility.c.
func (s *Snapshot) Dirty(t tuple.Tuple) (visible bool, wait tuple.XID, err error) {
	panic("not implemented")
}

// Modify says whether the snapshot's transaction may delete or update
// the tuple t at tid now. The snapshot's bounds play no part: what
// matters is the state of the transactions in xmin and xmax at this
// moment, so that a row updated after the snapshot is reported as
// TMUpdated rather than deleted from under the caller.
// PostgreSQL: HeapTupleSatisfiesUpdate in heapam_visibility.c.
func (s *Snapshot) Modify(t tuple.Tuple, tid tuple.TID) (heap.TM, error) {
	panic("not implemented")
}

// Wait blocks until the transaction xid has committed or aborted.
// PostgreSQL: XactLockTableWait in lmgr.c.
func (s *Snapshot) Wait(xid tuple.XID) {
	panic("not implemented")
}

var _ heap.Snapshot = (*Snapshot)(nil)
