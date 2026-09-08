// Package heap stores tuples in the pages of a relation. See
// chapters/05-heap.
package heap

import (
	"errors"
	"sync"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var (
	ErrNotFound       = errors.New("heap: tuple not found")
	ErrAlreadyDeleted = errors.New("heap: tuple already deleted")
	ErrTupleTooLarge  = errors.New("heap: tuple larger than a page")
)

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

// Fetch returns a copy of the tuple at tid, deleted or not.
// Returns ErrNotFound for a block past the end, an item number of zero or
// past the page's line pointers, or an unused line pointer.
// PostgreSQL: heap_fetch in heapam.c.
func (r *Relation) Fetch(tid tuple.TID) (tuple.Tuple, error) {
	buf, err := r.pinTuple(tid)
	if err != nil {
		return nil, err
	}
	defer r.pool.Unpin(buf)
	buf.RLock()
	defer buf.RUnlock()
	item, err := buf.Page().GetItem(tid.Off)
	if err != nil {
		return nil, ErrNotFound
	}
	return append(tuple.Tuple(nil), item...), nil
}

// Delete stamps the tuple at tid with xmax = xid.
// Returns ErrNotFound as Fetch does; ErrAlreadyDeleted if xmax is already
// non-zero, leaving the tuple untouched.
// PostgreSQL: heap_delete in heapam.c.
func (r *Relation) Delete(tid tuple.TID, xid tuple.XID) error {
	return r.markDeleted(tid, xid, nil)
}

// markDeleted sets xmax on the tuple at tid and, if newTID is not nil,
// points its ctid at newTID.
func (r *Relation) markDeleted(tid tuple.TID, xid tuple.XID, newTID *tuple.TID) error {
	buf, err := r.pinTuple(tid)
	if err != nil {
		return err
	}
	defer r.pool.Unpin(buf)
	buf.Lock()
	defer buf.Unlock()
	item, err := buf.Page().GetItem(tid.Off)
	if err != nil {
		return ErrNotFound
	}
	tup := tuple.Tuple(item)
	if tup.Xmax() != tuple.InvalidXID {
		return ErrAlreadyDeleted
	}
	tup.SetXmax(xid)
	if newTID != nil {
		tup.SetCtid(*newTID)
	}
	buf.MarkDirty()
	return nil
}

// Update deletes the tuple at tid as Delete does, inserts t, and points
// the old version's ctid at the new one, returning the new TID. Returns
// ErrTupleTooLarge for t, checked first, then Delete's errors. Whether
// xid may replace the old version is decided before the new one is
// inserted, so a failed update inserts nothing and leaves the old tuple
// unchanged; only another transaction changing the row between that
// check and the write can leave an inserted tuple no version points at.
// PostgreSQL: heap_update in heapam.c.
func (r *Relation) Update(tid tuple.TID, t tuple.Tuple, xid tuple.XID) (tuple.TID, error) {
	// Check the old version first so a failed update inserts nothing.
	old, err := r.Fetch(tid)
	if err != nil {
		return tuple.TID{}, err
	}
	if old.Xmax() != tuple.InvalidXID {
		return tuple.TID{}, ErrAlreadyDeleted
	}
	newTID, err := r.Insert(t, xid)
	if err != nil {
		return tuple.TID{}, err
	}
	if err := r.markDeleted(tid, xid, &newTID); err != nil {
		return tuple.TID{}, err
	}
	return newTID, nil
}

// Scan starts a sequential scan over the visible tuples of the relation.
// The block count is read once here; an error from that is reported by Err.
// PostgreSQL: heap_beginscan in heapam.c.
func (r *Relation) Scan() *Scan {
	s := &Scan{rel: r}
	s.nblocks, s.err = r.NBlocks()
	return s
}

// Scan iterates over a relation. Use it like bufio.Scanner.
type Scan struct {
	rel     *Relation
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
// order, skipping unused line pointers and tuples with a non-zero xmax.
// Returns false at the end or on error (see Err). No buffer stays pinned
// between calls.
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
	buf.RLock()
	defer buf.RUnlock()
	p := buf.Page()
	s.buffer = s.buffer[:0]
	for off := page.OffsetNumber(1); off <= p.NumItems(); off++ {
		item, err := p.GetItem(off)
		if err != nil {
			continue
		}
		if tuple.Tuple(item).Xmax() != tuple.InvalidXID {
			continue
		}
		s.buffer = append(s.buffer, scanItem{
			tid: tuple.TID{Block: blk, Off: off},
			tup: append(tuple.Tuple(nil), item...),
		})
	}
	return nil
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
