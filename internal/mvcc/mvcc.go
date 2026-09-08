// Package mvcc decides which tuple versions a transaction sees: a
// snapshot of the running transactions, and the visibility rules that
// combine a tuple's xmin and xmax with the snapshot and the commit log.
// See chapters/17-mvcc.
package mvcc

import (
	"slices"
	"strconv"
	"strings"

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
	xmin, xmax, xip := m.Snapshot()
	if i, ok := slices.BinarySearch(xip, xid); ok {
		xip = slices.Delete(xip, i, i+1)
	}
	return &Snapshot{Xmin: xmin, Xmax: xmax, Xip: xip, Xid: xid, Log: m}
}

// String renders the snapshot as pg_current_snapshot does: xmin:xmax:xip,
// the running IDs separated by commas.
func (s *Snapshot) String() string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(uint64(s.Xmin), 10))
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(uint64(s.Xmax), 10))
	b.WriteByte(':')
	for i, xid := range s.Xip {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(xid), 10))
	}
	return b.String()
}

// InProgress reports whether xid was running, or had not started, when
// the snapshot was taken, so that its effects are invisible whatever it
// did since. The snapshot's own transaction is never in progress.
// PostgreSQL: XidInMVCCSnapshot in snapmgr.c.
func (s *Snapshot) InProgress(xid tuple.XID) bool {
	if xid >= s.Xmax {
		return true
	}
	if xid < s.Xmin {
		return false
	}
	_, ok := slices.BinarySearch(s.Xip, xid)
	return ok
}

// setHint ORs bit into t's infomask.
func setHint(t tuple.Tuple, bit uint16) {
	t.SetInfomask(t.Infomask() | bit)
}

// status looks xid up in the log and records the verdict in t's hint
// bits: committed sets the committed bit, aborted the invalid bit. A
// running transaction sets nothing.
func (s *Snapshot) status(t tuple.Tuple, xid tuple.XID, committed, invalid uint16) (txn.Status, error) {
	st, err := s.Log.Status(xid)
	if err != nil {
		return 0, err
	}
	switch st {
	case txn.Committed:
		setHint(t, committed)
	case txn.Aborted:
		setHint(t, invalid)
	}
	return st, nil
}

// xminStatus reports the verdict on t's xmin: committed (or the
// snapshot's own), aborted, or in progress. It reads the hint bits
// first, then asks the log and sets them.
func (s *Snapshot) xminStatus(t tuple.Tuple) (txn.Status, error) {
	mask := t.Infomask()
	switch {
	case mask&tuple.XminCommitted != 0:
		return txn.Committed, nil
	case mask&tuple.XminInvalid != 0:
		return txn.Aborted, nil
	case t.Xmin() == s.Xid:
		return txn.Committed, nil
	}
	return s.status(t, t.Xmin(), tuple.XminCommitted, tuple.XminInvalid)
}

// xmaxStatus reports the verdict on t's xmax, which must be non-zero and
// not the snapshot's own: committed, aborted, or in progress. Hint bits
// first, then the log.
func (s *Snapshot) xmaxStatus(t tuple.Tuple) (txn.Status, error) {
	mask := t.Infomask()
	switch {
	case mask&tuple.XmaxCommitted != 0:
		return txn.Committed, nil
	case mask&tuple.XmaxInvalid != 0:
		return txn.Aborted, nil
	}
	return s.status(t, t.Xmax(), tuple.XmaxCommitted, tuple.XmaxInvalid)
}

// Visible reports whether t is visible to the snapshot: its inserting
// transaction is the snapshot's own or committed before the snapshot,
// and its deleting transaction, if any, is neither. Hint bits are read
// first and set on t when the log is consulted; the log is consulted
// only for a transaction the snapshot saw finish, so its verdict is
// final.
// PostgreSQL: HeapTupleSatisfiesMVCC in heapam_visibility.c.
func (s *Snapshot) Visible(t tuple.Tuple) (bool, error) {
	xmin := t.Xmin()
	if xmin != s.Xid {
		if t.Infomask()&tuple.XminCommitted == 0 && s.InProgress(xmin) {
			return false, nil
		}
		st, err := s.xminStatus(t)
		if err != nil || st != txn.Committed {
			return false, err
		}
		// Committed, and possibly hinted so by a later snapshot: was it
		// before ours?
		if s.InProgress(xmin) {
			return false, nil
		}
	}
	xmax := t.Xmax()
	switch {
	case xmax == tuple.InvalidXID:
		return true, nil
	case xmax == s.Xid:
		return false, nil
	case t.Infomask()&tuple.XmaxCommitted == 0 && s.InProgress(xmax):
		return true, nil
	}
	st, err := s.xmaxStatus(t)
	if err != nil || st != txn.Committed {
		return st != txn.Committed, err
	}
	return s.InProgress(xmax), nil
}

// Dirty reports whether t is visible to a unique check, which must see
// what running transactions are doing: a tuple inserted or deleted by
// one is reported with wait set to its ID, and the caller waits for it
// and asks again. Otherwise the answer is what the log says, own and
// committed transactions visible, aborted ones not, regardless of the
// snapshot's bounds.
// PostgreSQL: HeapTupleSatisfiesDirty in heapam_visibility.c.
func (s *Snapshot) Dirty(t tuple.Tuple) (visible bool, wait tuple.XID, err error) {
	st, err := s.xminStatus(t)
	if err != nil {
		return false, 0, err
	}
	switch st {
	case txn.Aborted:
		return false, 0, nil
	case txn.InProgress:
		return false, t.Xmin(), nil
	}
	xmax := t.Xmax()
	switch {
	case xmax == tuple.InvalidXID:
		return true, 0, nil
	case xmax == s.Xid:
		return false, 0, nil
	}
	st, err = s.xmaxStatus(t)
	if err != nil {
		return false, 0, err
	}
	switch st {
	case txn.InProgress:
		return true, xmax, nil
	case txn.Committed:
		return false, 0, nil
	}
	return true, 0, nil
}

// Modify says whether the snapshot's transaction may delete or update
// the tuple t at tid now. The snapshot's bounds play no part: what
// matters is the state of the transactions in xmin and xmax at this
// moment, so that a row updated after the snapshot is reported as
// TMUpdated rather than deleted from under the caller.
// PostgreSQL: HeapTupleSatisfiesUpdate in heapam_visibility.c.
func (s *Snapshot) Modify(t tuple.Tuple, tid tuple.TID) (heap.TM, error) {
	st, err := s.xminStatus(t)
	if err != nil {
		return 0, err
	}
	if st != txn.Committed {
		return heap.TMInvisible, nil
	}
	xmax := t.Xmax()
	switch {
	case xmax == tuple.InvalidXID:
		return heap.TMOk, nil
	case xmax == s.Xid:
		return heap.TMSelfModified, nil
	}
	st, err = s.xmaxStatus(t)
	if err != nil {
		return 0, err
	}
	switch st {
	case txn.InProgress:
		return heap.TMBeingModified, nil
	case txn.Aborted:
		return heap.TMOk, nil
	}
	if t.Ctid() == tid {
		return heap.TMDeleted, nil
	}
	return heap.TMUpdated, nil
}

// Wait blocks until the transaction xid has committed or aborted.
// PostgreSQL: XactLockTableWait in lmgr.c.
func (s *Snapshot) Wait(xid tuple.XID) {
	s.Log.Wait(xid)
}

var _ heap.Snapshot = (*Snapshot)(nil)
