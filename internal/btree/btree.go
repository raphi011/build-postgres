// Package btree is an on-disk B+tree over one key column, storing heap
// TIDs. See chapters/12-btree.
package btree

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/tuple"
)

const (
	// SpecialSize is the size of a B-tree page's special space
	// (BTPageOpaqueData).
	SpecialSize = 16
	// MetaBlock is the block holding the metapage.
	MetaBlock tuple.BlockNumber = 0
	// None is the block number meaning "no such page" in sibling links
	// and in the metapage's root (P_NONE). Block 0 is the metapage, so
	// zero is never a tree page.
	None tuple.BlockNumber = 0
	// Magic identifies a B-tree metapage (BTREE_MAGIC).
	Magic uint32 = 0x053162
	// Version is the on-disk format version.
	Version uint32 = 1
	// MetaSize is the size of the metapage item.
	MetaSize = 16
	// HeaderSize is the size of an index tuple's header: TID and info.
	HeaderSize = 8
	// TIDSize is the size of an encoded TID.
	TIDSize = 6
	// MaxItemSize is the largest leaf index tuple: three must fit on a
	// page, each leaving room for the heap TID a pivot copy carries
	// (BTMaxItemSize).
	MaxItemSize = ((page.PageSize-page.HeaderSize-SpecialSize-3*page.LinePointerSize)/3)&^7 - 8
	// FillFactor is the percentage of a page left on the left half when
	// the rightmost page splits on an append (BTREE_DEFAULT_FILLFACTOR).
	FillFactor = 90
)

// Page flags in the special space (btpo_flags).
const (
	FlagLeaf uint16 = 1 << 0
	FlagRoot uint16 = 1 << 1
	FlagMeta uint16 = 1 << 3
)

// Index tuple info bits (t_info).
const (
	IndexSizeMask  uint16 = 0x1FFF
	IndexPivotMask uint16 = 0x2000 // INDEX_ALT_TID_MASK: a pivot tuple
	IndexNullMask  uint16 = 0x8000
)

var (
	ErrKeyType     = errors.New("btree: key has wrong type")
	ErrKeyTooLarge = errors.New("btree: index row too large")
	ErrCorrupt     = errors.New("btree: corrupt index")
)

// IndexTuple is one index entry: a 6-byte TID, a 2-byte info word, then
// the key. On a leaf the TID is the heap tuple's. A pivot tuple (a
// downlink or a high key) has the child block in the TID's block field
// and carries the heap TID of the tuple it was made from after the key.
type IndexTuple []byte

// FormIndexTuple builds a leaf tuple. A nil key is NULL.
// Returns ErrKeyType if key's Go type is not the one for typ (int32,
// int64, bool, string); ErrKeyTooLarge if the tuple exceeds MaxItemSize.
// PostgreSQL: index_form_tuple in indextuple.c.
func FormIndexTuple(typ tuple.TypeID, key tuple.Datum, tid tuple.TID) (IndexTuple, error) {
	panic("not implemented")
}

// TID returns the TID field: the heap TID of a leaf tuple, the child
// block of a downlink.
func (t IndexTuple) TID() tuple.TID {
	panic("not implemented")
}

// Size returns the tuple's size in bytes as recorded in its header.
func (t IndexTuple) Size() int {
	panic("not implemented")
}

// IsNull reports whether the key is NULL.
func (t IndexTuple) IsNull() bool {
	panic("not implemented")
}

// IsPivot reports whether t is a downlink or high key.
func (t IndexTuple) IsPivot() bool {
	panic("not implemented")
}

// IsMinusInf reports whether t is the first downlink of an internal
// page, whose key is minus infinity and not stored.
func (t IndexTuple) IsMinusInf() bool {
	panic("not implemented")
}

// HeapTID returns the heap TID: the TID field of a leaf tuple, the
// trailing TID of a pivot. It is zero for a minus infinity downlink.
// PostgreSQL: BTreeTupleGetHeapTID in nbtree.h.
func (t IndexTuple) HeapTID() tuple.TID {
	panic("not implemented")
}

// Key decodes the key. It returns nil for NULL and for minus infinity.
// PostgreSQL: index_getattr in itup.h.
func (t IndexTuple) Key(typ tuple.TypeID) tuple.Datum {
	panic("not implemented")
}

// Opaque is the decoded special space of a tree page (BTPageOpaqueData):
// sibling links, level (0 for leaves), and flags.
type Opaque struct {
	Prev, Next tuple.BlockNumber // None at the ends of a level
	Level      uint32
	Flags      uint16
}

// ReadOpaque decodes the special space of p.
func ReadOpaque(p page.Page) Opaque {
	panic("not implemented")
}

// WriteOpaque encodes o into the special space of p.
func WriteOpaque(p page.Page, o Opaque) {
	panic("not implemented")
}

// Meta is the metapage content: the root block (None while the tree is
// empty) and the root's level, which is the tree height minus one.
type Meta struct {
	Root  tuple.BlockNumber
	Level uint32
}

// Tree is an open B-tree relation.
type Tree struct {
	pool *bufmgr.Pool
	oid  tuple.OID
	typ  tuple.TypeID
}

// Create makes a new relation file holding an empty tree: just the
// metapage. Returns smgr.ErrExists if the file exists.
// PostgreSQL: _bt_initmetapage in nbtpage.c.
func Create(pool *bufmgr.Pool, oid tuple.OID, typ tuple.TypeID) (*Tree, error) {
	panic("not implemented")
}

// Open returns a handle on an existing tree. It does no I/O.
func Open(pool *bufmgr.Pool, oid tuple.OID, typ tuple.TypeID) *Tree {
	panic("not implemented")
}

// OID returns the tree's relation OID.
func (t *Tree) OID() tuple.OID {
	panic("not implemented")
}

// KeyType returns the key column's type.
func (t *Tree) KeyType() tuple.TypeID {
	panic("not implemented")
}

// Meta reads the metapage. Returns ErrCorrupt if block 0 is not a
// metapage: wrong special space size, no FlagMeta, no 16-byte item 1,
// bad magic or version.
// PostgreSQL: _bt_getmeta in nbtpage.c.
func (t *Tree) Meta() (Meta, error) {
	panic("not implemented")
}

// Insert adds an entry for key (nil is NULL) pointing at tid.
// Returns ErrKeyType or ErrKeyTooLarge as FormIndexTuple does, before any
// I/O; ErrCorrupt if the metapage or a downlink is bad. A failed insert
// changes nothing.
// PostgreSQL: _bt_doinsert in nbtinsert.c.
func (t *Tree) Insert(key tuple.Datum, tid tuple.TID) error {
	panic("not implemented")
}

// Search returns the leaf block that key (nil is NULL) belongs on and
// the item number of the first entry there with a key >= key, which is
// one past the last item when there is none. An empty tree returns
// None and page.InvalidOffsetNumber. Returns ErrKeyType if key has the
// wrong type; ErrCorrupt as Insert does.
// PostgreSQL: _bt_search and _bt_binsrch in nbtsearch.c.
func (t *Tree) Search(key tuple.Datum) (tuple.BlockNumber, page.OffsetNumber, error) {
	panic("not implemented")
}

// Bound is one end of a scan range: a non-NULL key, included or not.
type Bound struct {
	Key       tuple.Datum
	Inclusive bool
}

// Scan starts a forward scan over the entries between lo and hi; a nil
// bound is open. NULL keys sort after every value, so a scan with no
// upper bound ends with them; callers that want only values stop at the
// first IsNull entry. A bound of the wrong type or with a nil Key makes
// Next return false and Err return ErrKeyType.
// PostgreSQL: _bt_first in nbtsearch.c.
func (t *Tree) Scan(lo, hi *Bound) *Scan {
	panic("not implemented")
}

// Scan iterates over a key range in key order. Use it like bufio.Scanner.
type Scan struct {
	t     *Tree
	hi    *Bound
	items []IndexTuple // entries of the current leaf from the start offset
	pos   int
	next  tuple.BlockNumber // next leaf to read
	err   error
	done  bool
}

// Next advances to the next entry. It returns false at the upper bound,
// at the end of the leaf chain, or once an error is set.
// PostgreSQL: _bt_next in nbtsearch.c.
func (s *Scan) Next() bool {
	panic("not implemented")
}

// TID returns the current entry's heap TID.
func (s *Scan) TID() tuple.TID {
	panic("not implemented")
}

// Key returns the current entry's key, nil for NULL.
func (s *Scan) Key() tuple.Datum {
	panic("not implemented")
}

// IsNull reports whether the current entry's key is NULL.
func (s *Scan) IsNull() bool {
	panic("not implemented")
}

// Err returns the first error encountered by the scan.
func (s *Scan) Err() error {
	panic("not implemented")
}

// Close releases the scan.
func (s *Scan) Close() {
	panic("not implemented")
}

// Check walks every page and verifies the tree's invariants, like
// amcheck's bt_index_check: the metapage, page flags and levels,
// sibling links, item order, high keys against parent downlinks, and
// that every block is reached exactly once. The error wraps ErrCorrupt
// and names the block; I/O errors are returned as they are.
// PostgreSQL: bt_check_every_level in verify_nbtree.c.
func (t *Tree) Check() error {
	panic("not implemented")
}
