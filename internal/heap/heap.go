// Package heap stores tuples in the pages of a relation. See
// chapters/05-heap.
package heap

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/bufmgr"
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

// Fetch returns a copy of the tuple at tid, deleted or not.
// Returns ErrNotFound for a block past the end, an item number of zero or
// past the page's line pointers, or an unused line pointer.
// PostgreSQL: heap_fetch in heapam.c.
func (r *Relation) Fetch(tid tuple.TID) (tuple.Tuple, error) {
	panic("not implemented")
}

// Delete stamps the tuple at tid with xmax = xid.
// Returns ErrNotFound as Fetch does; ErrAlreadyDeleted if xmax is already
// non-zero, leaving the tuple untouched.
// PostgreSQL: heap_delete in heapam.c.
func (r *Relation) Delete(tid tuple.TID, xid tuple.XID) error {
	panic("not implemented")
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
	panic("not implemented")
}

// Scan starts a sequential scan over the visible tuples of the relation.
// The block count is read once here; an error from that is reported by Err.
// PostgreSQL: heap_beginscan in heapam.c.
func (r *Relation) Scan() *Scan {
	panic("not implemented")
}

// Scan iterates over a relation. Use it like bufio.Scanner.
type Scan struct {
	rel *Relation
}

// Next advances to the next visible tuple: blocks in order, items in item
// order, skipping unused line pointers and tuples with a non-zero xmax.
// Returns false at the end or on error (see Err). No buffer stays pinned
// between calls.
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
