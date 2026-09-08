// Package heap stores tuples in the pages of a relation. See
// chapters/05-heap.
package heap

import (
	"errors"
	"fmt"
	"sync"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var (
	ErrNotFound       = errors.New("heap: tuple not found")
	ErrAlreadyDeleted = errors.New("heap: tuple already deleted")
	ErrTupleTooLarge  = errors.New("heap: tuple larger than a page")
	ErrInvisible      = errors.New("heap: attempted to update invisible tuple")
)

// Snapshot is a transaction's view of the others: which tuple versions
// it sees, and how it treats a tuple another transaction has touched.
// Chapter 17's mvcc.Snapshot implements it. A nil Snapshot is the rule
// of chapters 5 to 16: a tuple is live while its xmax is zero, whoever
// wrote it, and Delete refuses a tuple whose xmax is set.
// PostgreSQL: the HeapTupleSatisfies* functions in heapam_visibility.c.
type Snapshot interface {
	// Visible reports whether t is visible to the snapshot. t is a copy
	// of the heap tuple; the hint bits Visible sets in its infomask are
	// written back to the page by the caller.
	// PostgreSQL: HeapTupleSatisfiesMVCC.
	Visible(t tuple.Tuple) (bool, error)
	// Dirty reports whether t is visible to a unique check: it sees what
	// Visible sees and, in addition, what running transactions have
	// inserted or deleted, returning the ID to wait for in wait so that
	// the caller can retry once that transaction has finished.
	// PostgreSQL: HeapTupleSatisfiesDirty.
	Dirty(t tuple.Tuple) (visible bool, wait tuple.XID, err error)
	// Modify says whether the snapshot's transaction may delete or update
	// the tuple t at tid now: see TM.
	// PostgreSQL: HeapTupleSatisfiesUpdate.
	Modify(t tuple.Tuple, tid tuple.TID) (TM, error)
	// Wait blocks until the transaction xid has committed or aborted.
	// PostgreSQL: XactLockTableWait.
	Wait(xid tuple.XID)
}

// Any is the Snapshot that sees every tuple, deleted or not, so that a
// scan returns the whole relation; an index build reads through it.
// PostgreSQL: SnapshotAny.
var Any Snapshot = anySnapshot{}

type anySnapshot struct{}

func (anySnapshot) Visible(tuple.Tuple) (bool, error)          { return true, nil }
func (anySnapshot) Dirty(tuple.Tuple) (bool, tuple.XID, error) { return true, 0, nil }
func (anySnapshot) Modify(tuple.Tuple, tuple.TID) (TM, error)  { return TMOk, nil }
func (anySnapshot) Wait(tuple.XID)                             {}

// TM is the verdict of Snapshot.Modify on a tuple a transaction wants to
// delete or update.
// PostgreSQL: TM_Result in tableam.h.
type TM int

const (
	// TMOk: the tuple is live and nobody else holds it; go ahead.
	TMOk TM = iota
	// TMInvisible: the tuple's inserting transaction aborted or is still
	// running; the caller should never have seen it.
	TMInvisible
	// TMSelfModified: this transaction already deleted or updated it.
	TMSelfModified
	// TMUpdated: another transaction updated it and committed; the
	// tuple's ctid points at the new version.
	TMUpdated
	// TMDeleted: another transaction deleted it and committed.
	TMDeleted
	// TMBeingModified: another transaction has set xmax and is still
	// running; wait for it and look again.
	TMBeingModified
)

func (r TM) String() string {
	switch r {
	case TMOk:
		return "ok"
	case TMInvisible:
		return "invisible"
	case TMSelfModified:
		return "self-modified"
	case TMUpdated:
		return "updated"
	case TMDeleted:
		return "deleted"
	case TMBeingModified:
		return "being modified"
	}
	return fmt.Sprintf("TM(%d)", int(r))
}

// Relation is an open heap relation.
type Relation struct {
	pool *bufmgr.Pool
	oid  tuple.OID
	desc *tuple.Desc

	mu     sync.Mutex
	target tuple.BlockNumber // block the last insert went to
}

// Create makes a new, empty relation file and opens it.
// Returns smgr.ErrExists if the relation file is already there.
// PostgreSQL: heapam_relation_set_new_filelocator in heapam_handler.c.
func Create(pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) (*Relation, error) {
	panic("not implemented")
}

// Open returns a handle on an existing relation. It does no I/O.
func Open(pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) *Relation {
	panic("not implemented")
}

// OID returns the relation's object identifier.
func (r *Relation) OID() tuple.OID { return r.oid }

// Desc returns the relation's tuple descriptor.
func (r *Relation) Desc() *tuple.Desc { return r.desc }

// NBlocks returns the number of pages in the relation, as the pool's
// store reports it.
// PostgreSQL: RelationGetNumberOfBlocksInFork in bufmgr.c.
func (r *Relation) NBlocks() (tuple.BlockNumber, error) {
	panic("not implemented")
}

// Insert stores a copy of t stamped with xid and returns its TID.
// The copy gets xmin = xid, xmax cleared, every infomask bit but HasNull
// cleared, and ctid pointing at itself. It goes to the block of the last
// insert (on a fresh handle, the relation's last block); the relation is
// extended only when the tuple does not fit there. Returns
// ErrTupleTooLarge if len(t) > page.MaxItemSize, leaving the relation
// unchanged.
// PostgreSQL: heap_insert in heapam.c; RelationGetBufferForTuple in hio.c.
func (r *Relation) Insert(t tuple.Tuple, xid tuple.XID) (tuple.TID, error) {
	panic("not implemented")
}

// Fetch returns a copy of the tuple at tid, deleted or not, and whether
// snap sees it; with a nil snap, whether its xmax is zero. Hint bits the
// snapshot sets are written to the page.
// Returns ErrNotFound for a block past the end, an item number of zero or
// past the page's line pointers, or an unused line pointer.
// PostgreSQL: heap_fetch in heapam.c.
func (r *Relation) Fetch(tid tuple.TID, snap Snapshot) (t tuple.Tuple, visible bool, err error) {
	panic("not implemented")
}

// Delete stamps the tuple at tid with xmax = xid. With a snapshot the
// tuple may already carry the xmax of another transaction: Delete waits
// for one still running and then decides again, overwrites the xmax of
// one that aborted, and fails with ErrAlreadyDeleted for one that
// committed (Fetch shows whether the tuple was deleted or updated, by
// its ctid). A tuple xid itself deleted is ErrAlreadyDeleted too, and
// one whose inserting transaction did not commit is ErrInvisible. With a
// nil snapshot any non-zero xmax is ErrAlreadyDeleted. Returns
// ErrNotFound as Fetch does; a failed Delete leaves the tuple untouched.
// PostgreSQL: heap_delete in heapam.c.
func (r *Relation) Delete(tid tuple.TID, xid tuple.XID, snap Snapshot) error {
	panic("not implemented")
}

// Lock waits until no other transaction is changing the tuple at tid and
// reports whether xid may delete or update it now: nil, or the error
// Delete would return. It writes nothing, there being no row locks, so
// the Delete or Update that follows decides again; it lets a caller wait
// and check before doing work that must precede the write, such as the
// unique check.
// PostgreSQL: heap_lock_tuple in heapam.c.
func (r *Relation) Lock(tid tuple.TID, xid tuple.XID, snap Snapshot) error {
	panic("not implemented")
}

// Update deletes the tuple at tid as Delete does, inserts t, and points
// the old version's ctid at the new one, returning the new TID. Returns
// ErrTupleTooLarge for t, checked first, then Delete's errors; a failed
// update inserts nothing.
// PostgreSQL: heap_update in heapam.c.
func (r *Relation) Update(tid tuple.TID, t tuple.Tuple, xid tuple.XID, snap Snapshot) (tuple.TID, error) {
	panic("not implemented")
}

// Scan starts a sequential scan over the tuples of the relation that
// snap sees; with a nil snap, over those whose xmax is zero. The block
// count is read once here; an error from that is reported by Err.
// PostgreSQL: heap_beginscan in heapam.c.
func (r *Relation) Scan(snap Snapshot) *Scan {
	panic("not implemented")
}

// Scan iterates over a relation. Use it like bufio.Scanner.
type Scan struct {
	rel     *Relation
	snap    Snapshot
	nblocks tuple.BlockNumber
	next    tuple.BlockNumber // next block to load
	buffer  []scanItem        // visible tuples of the current block
	pos     int               // index into buffer of the current tuple
	err     error
	done    bool
}

type scanItem struct {
	tid tuple.TID
	tup tuple.Tuple
}

// Next advances to the next visible tuple: blocks in order, items in item
// order, skipping unused line pointers and tuples the snapshot does not
// see. Hint bits the snapshot sets are written to the page. Returns
// false at the end or on error (see Err). No buffer stays pinned between
// calls.
// PostgreSQL: heap_getnext in heapam.c.
func (s *Scan) Next() bool {
	panic("not implemented")
}

// TID returns the address of the current tuple. Valid only after Next
// returned true.
func (s *Scan) TID() tuple.TID {
	panic("not implemented")
}

// Tuple returns a copy of the current tuple; modifying it does not change
// the relation. Valid only after Next returned true.
func (s *Scan) Tuple() tuple.Tuple {
	panic("not implemented")
}

// Err returns the first error encountered by the scan, nil if none. Check
// it once Next has returned false.
func (s *Scan) Err() error {
	panic("not implemented")
}

// Close releases any resources held by the scan. Next returns false
// afterwards.
// PostgreSQL: heap_endscan in heapam.c.
func (s *Scan) Close() {
	panic("not implemented")
}
