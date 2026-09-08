// Package heap stores tuples in the pages of a relation. See
// chapters/05-heap.
package heap

import (
	"errors"
	"fmt"
	"sync"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/page"
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
	if err := pool.Store().Create(oid); err != nil {
		return nil, err
	}
	return Open(pool, oid, desc), nil
}

// Open returns a handle on an existing relation. It does no I/O.
func Open(pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) *Relation {
	return &Relation{pool: pool, oid: oid, desc: desc, target: tuple.InvalidBlockNumber}
}

// OID returns the relation's object identifier.
func (r *Relation) OID() tuple.OID { return r.oid }

// Desc returns the relation's tuple descriptor.
func (r *Relation) Desc() *tuple.Desc { return r.desc }

// NBlocks returns the number of pages in the relation, as the pool's
// store reports it.
// PostgreSQL: RelationGetNumberOfBlocksInFork in bufmgr.c.
func (r *Relation) NBlocks() (tuple.BlockNumber, error) {
	return r.pool.Store().NBlocks(r.oid)
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
	if len(t) > page.MaxItemSize {
		return tuple.TID{}, ErrTupleTooLarge
	}
	tup := append(tuple.Tuple(nil), t...)
	tup.SetXmin(xid)
	tup.SetXmax(tuple.InvalidXID)
	tup.SetInfomask(tup.Infomask() & tuple.HasNull)

	r.mu.Lock()
	defer r.mu.Unlock()

	// Try the block the last insert went to; on a fresh handle, the last
	// block of the relation.
	target := r.target
	if target == tuple.InvalidBlockNumber {
		n, err := r.NBlocks()
		if err != nil {
			return tuple.TID{}, err
		}
		if n > 0 {
			target = n - 1
		}
	}
	if target != tuple.InvalidBlockNumber {
		buf, err := r.pool.Pin(r.oid, target)
		if err != nil {
			return tuple.TID{}, err
		}
		tid, ok := putTuple(buf, tup)
		r.pool.Unpin(buf)
		if ok {
			r.target = target
			return tid, nil
		}
	}

	buf, err := r.pool.Extend(r.oid)
	if err != nil {
		return tuple.TID{}, err
	}
	buf.Lock()
	buf.Page().Init(0)
	buf.Unlock()
	tid, ok := putTuple(buf, tup)
	r.pool.Unpin(buf)
	if !ok {
		return tuple.TID{}, ErrTupleTooLarge
	}
	r.target = tid.Block
	return tid, nil
}

// putTuple adds tup to the pinned buffer if it fits, fixing up its ctid.
func putTuple(buf *bufmgr.Buffer, tup tuple.Tuple) (tuple.TID, bool) {
	buf.Lock()
	defer buf.Unlock()
	p := buf.Page()
	if p.FreeSpace() < (len(tup)+7)&^7 {
		return tuple.TID{}, false
	}
	off, err := p.AddItem(tup)
	if err != nil {
		return tuple.TID{}, false
	}
	tid := tuple.TID{Block: buf.Block(), Off: off}
	item, _ := p.GetItem(off)
	tuple.Tuple(item).SetCtid(tid)
	buf.MarkDirty()
	return tid, true
}

// pinTuple pins the buffer holding tid and returns it with the raw item.
// The caller must hold no lock; it receives the buffer pinned but unlocked.
func (r *Relation) pinTuple(tid tuple.TID) (*bufmgr.Buffer, error) {
	n, err := r.NBlocks()
	if err != nil {
		return nil, err
	}
	if tid.Block >= n || tid.Off == page.InvalidOffsetNumber {
		return nil, ErrNotFound
	}
	return r.pool.Pin(r.oid, tid.Block)
}

// Fetch returns a copy of the tuple at tid, deleted or not, and whether
// snap sees it; with a nil snap, whether its xmax is zero. Hint bits the
// snapshot sets are written to the page.
// Returns ErrNotFound for a block past the end, an item number of zero or
// past the page's line pointers, or an unused line pointer.
// PostgreSQL: heap_fetch in heapam.c.
func (r *Relation) Fetch(tid tuple.TID, snap Snapshot) (t tuple.Tuple, visible bool, err error) {
	buf, err := r.pinTuple(tid)
	if err != nil {
		return nil, false, err
	}
	defer r.pool.Unpin(buf)
	buf.RLock()
	item, err := buf.Page().GetItem(tid.Off)
	if err != nil {
		buf.RUnlock()
		return nil, false, ErrNotFound
	}
	t = append(tuple.Tuple(nil), item...)
	buf.RUnlock()
	if snap == nil {
		return t, t.Xmax() == tuple.InvalidXID, nil
	}
	h := hint{off: tid.Off, xmin: t.Xmin(), xmax: t.Xmax(), before: t.Infomask()}
	visible, err = snap.Visible(t)
	if err != nil {
		return nil, false, err
	}
	if h.after = t.Infomask(); h.after != h.before {
		setHints(buf, []hint{h})
	}
	return t, visible, nil
}

// hint is a change of infomask bits a snapshot made on the copy of the
// tuple at off, to be written back to the page if the tuple's xmin and
// xmax are still the ones the verdict was reached on.
type hint struct {
	off           page.OffsetNumber
	xmin, xmax    tuple.XID
	before, after uint16
}

// setHints writes hint bits back to the pinned buffer under the
// exclusive lock. A tuple whose xmax changed in between (a delete after
// the verdict on an aborted xmax) is left alone; the next reader decides
// again.
// PostgreSQL: SetHintBits in heapam_visibility.c.
func setHints(buf *bufmgr.Buffer, hints []hint) {
	buf.Lock()
	defer buf.Unlock()
	p := buf.Page()
	dirty := false
	for _, h := range hints {
		item, err := p.GetItem(h.off)
		if err != nil {
			continue
		}
		t := tuple.Tuple(item)
		if t.Xmin() != h.xmin || t.Xmax() != h.xmax {
			continue
		}
		t.SetInfomask(t.Infomask() | h.after&^h.before)
		dirty = true
	}
	if dirty {
		buf.MarkDirty()
	}
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
	return r.markDeleted(tid, xid, tuple.TID{}, snap)
}

// Lock waits until no other transaction is changing the tuple at tid and
// reports whether xid may delete or update it now: nil, or the error
// Delete would return. It writes nothing, there being no row locks, so
// the Delete or Update that follows decides again; it lets a caller wait
// and check before doing work that must precede the write, such as the
// unique check.
// PostgreSQL: heap_lock_tuple in heapam.c.
func (r *Relation) Lock(tid tuple.TID, xid tuple.XID, snap Snapshot) error {
	return r.markDeleted(tid, tuple.InvalidXID, tuple.TID{}, snap)
}

// markDeleted sets xmax on the tuple at tid, clearing the hint bits of
// the xmax it replaces, and its ctid too when ctid is not the zero TID,
// so that an update writes both under one lock; with xid InvalidXID it
// only checks. It waits, without any lock held, for a transaction that
// still holds the tuple.
func (r *Relation) markDeleted(tid tuple.TID, xid tuple.XID, ctid tuple.TID, snap Snapshot) error {
	buf, err := r.pinTuple(tid)
	if err != nil {
		return err
	}
	defer r.pool.Unpin(buf)
	for {
		buf.Lock()
		item, err := buf.Page().GetItem(tid.Off)
		if err != nil {
			buf.Unlock()
			return ErrNotFound
		}
		tup := tuple.Tuple(item)
		res := TMOk
		switch {
		case snap == nil:
			if tup.Xmax() != tuple.InvalidXID {
				res = TMDeleted
			}
		default:
			res, err = snap.Modify(append(tuple.Tuple(nil), tup...), tid)
			if err != nil {
				buf.Unlock()
				return err
			}
		}
		switch res {
		case TMOk:
			if xid != tuple.InvalidXID {
				tup.SetXmax(xid)
				tup.SetInfomask(tup.Infomask() &^ (tuple.XmaxCommitted | tuple.XmaxInvalid))
				if ctid != (tuple.TID{}) {
					tup.SetCtid(ctid)
				}
				buf.MarkDirty()
			}
			buf.Unlock()
			return nil
		case TMBeingModified:
			xmax := tup.Xmax()
			buf.Unlock()
			snap.Wait(xmax)
		case TMInvisible:
			buf.Unlock()
			return ErrInvisible
		default:
			buf.Unlock()
			return ErrAlreadyDeleted
		}
	}
}

// Update deletes the tuple at tid as Delete does, inserts t, and points
// the old version's ctid at the new one, returning the new TID. Returns
// ErrTupleTooLarge for t, checked first, then Delete's errors. Whether
// xid may replace the old version is decided before the new one is
// inserted, so a failed update inserts nothing and leaves the old tuple
// unchanged; if another transaction takes the row between that check and
// the write, the tuple already inserted is stamped with xid's own xmax,
// so no snapshot ever sees it.
// PostgreSQL: heap_update in heapam.c.
func (r *Relation) Update(tid tuple.TID, t tuple.Tuple, xid tuple.XID, snap Snapshot) (tuple.TID, error) {
	if len(t) > page.MaxItemSize {
		return tuple.TID{}, ErrTupleTooLarge
	}
	// Decide before the insert: stamping first and then failing to
	// insert would leave a row deleted with no replacement (D25).
	if err := r.markDeleted(tid, tuple.InvalidXID, tuple.TID{}, snap); err != nil {
		return tuple.TID{}, err
	}
	newTID, err := r.Insert(t, xid)
	if err != nil {
		return tuple.TID{}, err
	}
	// The check above does not settle it: another transaction can take
	// the row while the insert runs, and this is where the two are
	// serialised. Bury the version already inserted before giving up, so
	// that its xmin and xmax are this transaction and nobody sees it.
	if err := r.markDeleted(tid, xid, newTID, snap); err != nil {
		if berr := r.markDeleted(newTID, xid, tuple.TID{}, nil); berr != nil {
			return tuple.TID{}, berr
		}
		return tuple.TID{}, err
	}
	return newTID, nil
}

// Scan starts a sequential scan over the tuples of the relation that
// snap sees; with a nil snap, over those whose xmax is zero. The block
// count is read once here; an error from that is reported by Err.
// PostgreSQL: heap_beginscan in heapam.c.
func (r *Relation) Scan(snap Snapshot) *Scan {
	s := &Scan{rel: r, snap: snap}
	s.nblocks, s.err = r.NBlocks()
	return s
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
	if s.done || s.err != nil {
		return false
	}
	s.pos++
	for s.pos >= len(s.buffer) {
		if s.next >= s.nblocks {
			s.done = true
			return false
		}
		if err := s.loadBlock(s.next); err != nil {
			s.err = err
			return false
		}
		s.next++
		s.pos = 0
	}
	return true
}

// loadBlock copies the visible tuples of blk into the scan's buffer.
func (s *Scan) loadBlock(blk tuple.BlockNumber) error {
	buf, err := s.rel.pool.Pin(s.rel.oid, blk)
	if err != nil {
		return err
	}
	defer s.rel.pool.Unpin(buf)
	hints, err := s.readBlock(buf, blk)
	if len(hints) > 0 {
		setHints(buf, hints)
	}
	return err
}

// readBlock copies the visible tuples of the pinned buffer into the
// scan's buffer under the shared lock and returns the hint bits to set.
func (s *Scan) readBlock(buf *bufmgr.Buffer, blk tuple.BlockNumber) ([]hint, error) {
	buf.RLock()
	defer buf.RUnlock()
	p := buf.Page()
	s.buffer = s.buffer[:0]
	var hints []hint
	for off := page.OffsetNumber(1); off <= p.NumItems(); off++ {
		item, err := p.GetItem(off)
		if err != nil {
			continue
		}
		tup := append(tuple.Tuple(nil), item...)
		if s.snap == nil {
			if tup.Xmax() != tuple.InvalidXID {
				continue
			}
		} else {
			h := hint{off: off, xmin: tup.Xmin(), xmax: tup.Xmax(), before: tup.Infomask()}
			visible, err := s.snap.Visible(tup)
			if err != nil {
				return hints, err
			}
			if h.after = tup.Infomask(); h.after != h.before {
				hints = append(hints, h)
			}
			if !visible {
				continue
			}
		}
		s.buffer = append(s.buffer, scanItem{tid: tuple.TID{Block: blk, Off: off}, tup: tup})
	}
	return hints, nil
}

// TID returns the address of the current tuple. Valid only after Next
// returned true.
func (s *Scan) TID() tuple.TID { return s.buffer[s.pos].tid }

// Tuple returns a copy of the current tuple; modifying it does not change
// the relation. Valid only after Next returned true.
func (s *Scan) Tuple() tuple.Tuple { return s.buffer[s.pos].tup }

// Err returns the first error encountered by the scan, nil if none. Check
// it once Next has returned false.
func (s *Scan) Err() error { return s.err }

// Close releases any resources held by the scan. Next returns false
// afterwards.
// PostgreSQL: heap_endscan in heapam.c.
func (s *Scan) Close() {
	s.done = true
	s.buffer = nil
}
