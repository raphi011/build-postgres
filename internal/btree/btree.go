// Package btree is an on-disk B+tree over one key column, storing heap
// TIDs. See chapters/12-btree.
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

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
	t := make(IndexTuple, HeaderSize, HeaderSize+16)
	putTID(t, tid)
	var info uint16
	if key == nil {
		info = IndexNullMask
	} else {
		var err error
		if t, err = appendKey(t, typ, key); err != nil {
			return nil, err
		}
	}
	if len(t) > MaxItemSize {
		return nil, ErrKeyTooLarge
	}
	binary.LittleEndian.PutUint16(t[6:], info|uint16(len(t)))
	return t, nil
}

func appendKey(b []byte, typ tuple.TypeID, key tuple.Datum) ([]byte, error) {
	switch typ {
	case tuple.Int4:
		if x, ok := key.(int32); ok {
			return binary.LittleEndian.AppendUint32(b, uint32(x)), nil
		}
	case tuple.Int8:
		if x, ok := key.(int64); ok {
			return binary.LittleEndian.AppendUint64(b, uint64(x)), nil
		}
	case tuple.Bool:
		if x, ok := key.(bool); ok {
			if x {
				return append(b, 1), nil
			}
			return append(b, 0), nil
		}
	case tuple.Text:
		if x, ok := key.(string); ok {
			b = binary.LittleEndian.AppendUint32(b, uint32(len(x)))
			return append(b, x...), nil
		}
	}
	return nil, ErrKeyType
}

func putTID(b []byte, tid tuple.TID) {
	binary.LittleEndian.PutUint32(b, uint32(tid.Block))
	binary.LittleEndian.PutUint16(b[4:], uint16(tid.Off))
}

func getTID(b []byte) tuple.TID {
	return tuple.TID{
		Block: tuple.BlockNumber(binary.LittleEndian.Uint32(b)),
		Off:   page.OffsetNumber(binary.LittleEndian.Uint16(b[4:])),
	}
}

func (t IndexTuple) info() uint16 { return binary.LittleEndian.Uint16(t[6:]) }

// TID returns the TID field: the heap TID of a leaf tuple, the child
// block of a downlink.
func (t IndexTuple) TID() tuple.TID { return getTID(t) }

// Size returns the tuple's size in bytes as recorded in its header.
func (t IndexTuple) Size() int { return int(t.info() & IndexSizeMask) }

// IsNull reports whether the key is NULL.
func (t IndexTuple) IsNull() bool { return t.info()&IndexNullMask != 0 }

// IsPivot reports whether t is a downlink or high key.
func (t IndexTuple) IsPivot() bool { return t.info()&IndexPivotMask != 0 }

// IsMinusInf reports whether t is the first downlink of an internal
// page, whose key is minus infinity and not stored.
func (t IndexTuple) IsMinusInf() bool { return t.IsPivot() && t.Size() == HeaderSize }

// HeapTID returns the heap TID: the TID field of a leaf tuple, the
// trailing TID of a pivot. It is zero for a minus infinity downlink.
// PostgreSQL: BTreeTupleGetHeapTID in nbtree.h.
func (t IndexTuple) HeapTID() tuple.TID {
	if !t.IsPivot() {
		return t.TID()
	}
	if t.IsMinusInf() {
		return tuple.TID{}
	}
	return getTID(t[len(t)-TIDSize:])
}

// Key decodes the key. It returns nil for NULL and for minus infinity.
// PostgreSQL: index_getattr in itup.h.
func (t IndexTuple) Key(typ tuple.TypeID) tuple.Datum {
	if t.IsNull() {
		return nil
	}
	b := t[HeaderSize:]
	switch typ {
	case tuple.Int4:
		return int32(binary.LittleEndian.Uint32(b))
	case tuple.Int8:
		return int64(binary.LittleEndian.Uint64(b))
	case tuple.Bool:
		return b[0] != 0
	case tuple.Text:
		n := binary.LittleEndian.Uint32(b)
		return string(b[4 : 4+n])
	}
	panic("btree: unknown type " + typ.String())
}

// keyBytes returns the encoded key without the header or a pivot's
// trailing heap TID.
func (t IndexTuple) keyBytes() []byte {
	if t.IsMinusInf() {
		return nil
	}
	end := len(t)
	if t.IsPivot() {
		end -= TIDSize
	}
	return t[HeaderSize:end]
}

// pivot makes a downlink to child (or a high key, child == None) from
// it: the same key, then the heap TID.
func pivot(it IndexTuple, child tuple.BlockNumber) IndexTuple {
	p := make(IndexTuple, 0, len(it)+TIDSize)
	p = append(p, it[:HeaderSize]...)
	p = append(p, it.keyBytes()...)
	p = p[:len(p)+TIDSize]
	putTID(p[len(p)-TIDSize:], it.HeapTID())
	putTID(p, tuple.TID{Block: child})
	info := it.info()&^IndexSizeMask | IndexPivotMask | uint16(len(p))
	binary.LittleEndian.PutUint16(p[6:], info)
	return p
}

// minusInf makes the first downlink of an internal page.
func minusInf(child tuple.BlockNumber) IndexTuple {
	p := make(IndexTuple, HeaderSize)
	putTID(p, tuple.TID{Block: child})
	binary.LittleEndian.PutUint16(p[6:], IndexPivotMask|IndexNullMask|HeaderSize)
	return p
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
	s := p.SpecialSpace()
	return Opaque{
		Prev:  tuple.BlockNumber(binary.LittleEndian.Uint32(s)),
		Next:  tuple.BlockNumber(binary.LittleEndian.Uint32(s[4:])),
		Level: binary.LittleEndian.Uint32(s[8:]),
		Flags: binary.LittleEndian.Uint16(s[12:]),
	}
}

// WriteOpaque encodes o into the special space of p.
func WriteOpaque(p page.Page, o Opaque) {
	s := p.SpecialSpace()
	binary.LittleEndian.PutUint32(s, uint32(o.Prev))
	binary.LittleEndian.PutUint32(s[4:], uint32(o.Next))
	binary.LittleEndian.PutUint32(s[8:], o.Level)
	binary.LittleEndian.PutUint16(s[12:], o.Flags)
	binary.LittleEndian.PutUint16(s[14:], 0)
}

// Meta is the metapage content: the root block (None while the tree is
// empty) and the root's level, which is the tree height minus one.
type Meta struct {
	Root  tuple.BlockNumber
	Level uint32
}

func metaBytes(m Meta) []byte {
	b := make([]byte, MetaSize)
	binary.LittleEndian.PutUint32(b, Magic)
	binary.LittleEndian.PutUint32(b[4:], Version)
	binary.LittleEndian.PutUint32(b[8:], uint32(m.Root))
	binary.LittleEndian.PutUint32(b[12:], m.Level)
	return b
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
	if err := pool.Store().Create(oid); err != nil {
		return nil, err
	}
	t := Open(pool, oid, typ)
	buf, err := pool.Extend(oid)
	if err != nil {
		return nil, err
	}
	buf.Lock()
	p := buf.Page()
	p.Init(SpecialSize)
	if _, err := p.AddItem(metaBytes(Meta{Root: None})); err != nil {
		buf.Unlock()
		pool.Unpin(buf)
		return nil, err
	}
	WriteOpaque(p, Opaque{Flags: FlagMeta})
	buf.MarkDirty()
	buf.Unlock()
	pool.Unpin(buf)
	return t, nil
}

// Open returns a handle on an existing tree. It does no I/O.
func Open(pool *bufmgr.Pool, oid tuple.OID, typ tuple.TypeID) *Tree {
	return &Tree{pool: pool, oid: oid, typ: typ}
}

// OID returns the tree's relation OID.
func (t *Tree) OID() tuple.OID { return t.oid }

// KeyType returns the key column's type.
func (t *Tree) KeyType() tuple.TypeID { return t.typ }

// readPage returns a copy of block blk.
func (t *Tree) readPage(blk tuple.BlockNumber) (page.Page, error) {
	buf, err := t.pool.Pin(t.oid, blk)
	if err != nil {
		return nil, err
	}
	buf.RLock()
	p := append(page.Page(nil), buf.Page()...)
	buf.RUnlock()
	t.pool.Unpin(buf)
	return p, nil
}

// modify applies fn to block blk under the exclusive lock.
func (t *Tree) modify(blk tuple.BlockNumber, fn func(page.Page) error) error {
	buf, err := t.pool.Pin(t.oid, blk)
	if err != nil {
		return err
	}
	buf.Lock()
	err = fn(buf.Page())
	if err == nil {
		buf.MarkDirty()
	}
	buf.Unlock()
	t.pool.Unpin(buf)
	return err
}

// newPage extends the relation by one initialised tree page.
func (t *Tree) newPage() (tuple.BlockNumber, error) {
	buf, err := t.pool.Extend(t.oid)
	if err != nil {
		return None, err
	}
	blk := buf.Block()
	buf.Lock()
	buf.Page().Init(SpecialSize)
	buf.MarkDirty()
	buf.Unlock()
	t.pool.Unpin(buf)
	return blk, nil
}

// Meta reads the metapage. Returns ErrCorrupt if block 0 is not a
// metapage: wrong special space size, no FlagMeta, no 16-byte item 1,
// bad magic or version.
// PostgreSQL: _bt_getmeta in nbtpage.c.
func (t *Tree) Meta() (Meta, error) {
	p, err := t.readPage(MetaBlock)
	if err != nil {
		return Meta{}, err
	}
	return decodeMeta(p)
}

func decodeMeta(p page.Page) (Meta, error) {
	if int(p.Special()) != page.PageSize-SpecialSize || ReadOpaque(p).Flags&FlagMeta == 0 {
		return Meta{}, fmt.Errorf("%w: block 0 is not a metapage", ErrCorrupt)
	}
	b, err := p.GetItem(1)
	if err != nil || len(b) != MetaSize {
		return Meta{}, fmt.Errorf("%w: metapage has no metadata item", ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(b) != Magic {
		return Meta{}, fmt.Errorf("%w: bad metapage magic %#x", ErrCorrupt, binary.LittleEndian.Uint32(b))
	}
	if v := binary.LittleEndian.Uint32(b[4:]); v != Version {
		return Meta{}, fmt.Errorf("%w: metapage version %d, want %d", ErrCorrupt, v, Version)
	}
	return Meta{
		Root:  tuple.BlockNumber(binary.LittleEndian.Uint32(b[8:])),
		Level: binary.LittleEndian.Uint32(b[12:]),
	}, nil
}

func (t *Tree) writeMeta(m Meta) error {
	return t.modify(MetaBlock, func(p page.Page) error {
		b, err := p.GetItem(1)
		if err != nil {
			return fmt.Errorf("%w: metapage has no metadata item", ErrCorrupt)
		}
		copy(b, metaBytes(m))
		return nil
	})
}

// Key comparison. NULL sorts after every value; ties are broken by the
// heap TID, so every entry has a distinct position (PostgreSQL 12+).

func checkKey(typ tuple.TypeID, key tuple.Datum) error {
	_, err := appendKey(nil, typ, key)
	return err
}

func compareKey(typ tuple.TypeID, a, b tuple.Datum) int {
	switch typ {
	case tuple.Int4:
		return cmp(a.(int32), b.(int32))
	case tuple.Int8:
		return cmp(a.(int64), b.(int64))
	case tuple.Bool:
		x, y := a.(bool), b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case tuple.Text:
		return strings.Compare(a.(string), b.(string))
	}
	panic("btree: unknown type " + typ.String())
}

func cmp[T int32 | int64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareNullable(typ tuple.TypeID, a, b tuple.Datum) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return compareKey(typ, a, b)
}

func compareTID(a, b tuple.TID) int {
	if a.Block != b.Block {
		return cmp(int64(a.Block), int64(b.Block))
	}
	return cmp(int32(a.Off), int32(b.Off))
}

// compareTuples orders two index tuples by (key, heap TID).
func (t *Tree) compareTuples(a, b IndexTuple) int {
	if c := compareNullable(t.typ, a.Key(t.typ), b.Key(t.typ)); c != 0 {
		return c
	}
	return compareTID(a.HeapTID(), b.HeapTID())
}

// skey is what a search looks for: a key with, in place of a heap TID,
// either an exact TID (inserts) or minus or plus infinity (scan bounds),
// or minus infinity outright (the leftmost leaf).
type skey struct {
	key      tuple.Datum // nil is NULL
	tid      tuple.TID
	tidMode  int // -1: below every TID, 0: tid, 1: above every TID
	minusInf bool
}

// compare orders an index tuple against k.
func (t *Tree) compare(it IndexTuple, k skey) int {
	if k.minusInf {
		return 1
	}
	if c := compareNullable(t.typ, it.Key(t.typ), k.key); c != 0 {
		return c
	}
	switch k.tidMode {
	case -1:
		return 1
	case 1:
		return -1
	}
	return compareTID(it.HeapTID(), k.tid)
}

// firstData returns the item number of the first data item: 2 on a page
// with a high key, else 1.
func firstData(o Opaque) page.OffsetNumber {
	if o.Next != None {
		return 2
	}
	return 1
}

func item(p page.Page, off page.OffsetNumber) IndexTuple {
	b, err := p.GetItem(off)
	if err != nil {
		return nil
	}
	return IndexTuple(b)
}

// binsrch is _bt_binsrch: on a leaf it returns the first data item that
// is >= k (possibly one past the last); on an internal page the last
// downlink that is <= k, where the first downlink counts as minus
// infinity.
func (t *Tree) binsrch(p page.Page, o Opaque, k skey) page.OffsetNumber {
	leaf := o.Flags&FlagLeaf != 0
	low, high := firstData(o), p.NumItems()+1
	if !leaf {
		low++
	}
	for low < high {
		mid := low + (high-low)/2
		c := t.compare(item(p, mid), k)
		if c < 0 || (!leaf && c == 0) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if leaf {
		return low
	}
	return low - 1
}

// pathEntry records a downlink taken on the way down: the internal page
// and the item number of the downlink.
type pathEntry struct {
	blk tuple.BlockNumber
	off page.OffsetNumber
}

// descend is _bt_search: it walks from the root to the leaf that k
// belongs on and returns a copy of the leaf, its block, and the
// downlinks taken. The leaf is nil when the tree is empty.
func (t *Tree) descend(k skey) (page.Page, tuple.BlockNumber, []pathEntry, error) {
	meta, err := t.Meta()
	if err != nil {
		return nil, None, nil, err
	}
	if meta.Root == None {
		return nil, None, nil, nil
	}
	var path []pathEntry
	blk := meta.Root
	for {
		p, err := t.readPage(blk)
		if err != nil {
			return nil, None, nil, err
		}
		o := ReadOpaque(p)
		if o.Flags&FlagLeaf != 0 {
			return p, blk, path, nil
		}
		off := t.binsrch(p, o, k)
		it := item(p, off)
		if it == nil {
			return nil, None, nil, fmt.Errorf("%w: block %d has no downlink at %d", ErrCorrupt, blk, off)
		}
		path = append(path, pathEntry{blk, off})
		child := it.TID().Block
		if child == None || child == blk {
			return nil, None, nil, fmt.Errorf("%w: block %d has downlink to block %d", ErrCorrupt, blk, child)
		}
		blk = child
	}
}

// Insert adds an entry for key (nil is NULL) pointing at tid.
// Returns ErrKeyType or ErrKeyTooLarge as FormIndexTuple does, before any
// I/O; ErrCorrupt if the metapage or a downlink is bad. A failed insert
// changes nothing.
// PostgreSQL: _bt_doinsert in nbtinsert.c.
func (t *Tree) Insert(key tuple.Datum, tid tuple.TID) error {
	it, err := FormIndexTuple(t.typ, key, tid)
	if err != nil {
		return err
	}
	k := skey{key: key, tid: tid}
	leaf, blk, path, err := t.descend(k)
	if err != nil {
		return err
	}
	if leaf == nil {
		return t.newRootLeaf(it)
	}
	pos := t.binsrch(leaf, ReadOpaque(leaf), k)
	return t.insertAt(blk, leaf, pos, it, path)
}

// newRootLeaf gives an empty tree its first page.
func (t *Tree) newRootLeaf(it IndexTuple) error {
	blk, err := t.newPage()
	if err != nil {
		return err
	}
	p := page.New(SpecialSize)
	if _, err := p.AddItem(it); err != nil {
		return err
	}
	WriteOpaque(p, Opaque{Flags: FlagLeaf | FlagRoot})
	if err := t.writePage(blk, p); err != nil {
		return err
	}
	return t.writeMeta(Meta{Root: blk, Level: 0})
}

func (t *Tree) writePage(blk tuple.BlockNumber, p page.Page) error {
	return t.modify(blk, func(dst page.Page) error {
		copy(dst, p)
		return nil
	})
}

// insertAt is _bt_insertonpg: it puts it at pos on block blk (of which
// p is a copy), splitting the page when it does not fit.
func (t *Tree) insertAt(blk tuple.BlockNumber, p page.Page, pos page.OffsetNumber, it IndexTuple, path []pathEntry) error {
	if p.FreeSpace() >= (len(it)+7)&^7 {
		return t.modify(blk, func(p page.Page) error { return p.InsertItem(pos, it) })
	}
	return t.split(blk, p, pos, it, path)
}

// split is _bt_split: block blk (copy p) gets a new right sibling, the
// items including it are divided between them, the left page's new
// high key is the right page's first item, and a downlink to the right
// page goes into the parent, which may split in turn.
func (t *Tree) split(blk tuple.BlockNumber, p page.Page, pos page.OffsetNumber, it IndexTuple, path []pathEntry) error {
	o := ReadOpaque(p)
	leaf := o.Flags&FlagLeaf != 0
	var oldHigh IndexTuple
	if o.Next != None {
		oldHigh = item(p, 1)
	}
	var items []IndexTuple
	for off := firstData(o); off <= p.NumItems()+1; off++ {
		if off == pos {
			items = append(items, it)
		}
		if off <= p.NumItems() {
			items = append(items, item(p, off))
		}
	}
	k := splitPoint(items, oldHigh, o.Next == None && pos == p.NumItems()+1)

	rightBlk, err := t.newPage()
	if err != nil {
		return err
	}
	sep := items[k]
	right := page.New(SpecialSize)
	if oldHigh != nil {
		if _, err := right.AddItem(oldHigh); err != nil {
			return err
		}
	}
	for i, x := range items[k:] {
		if i == 0 && !leaf {
			x = minusInf(x.TID().Block)
		}
		if _, err := right.AddItem(x); err != nil {
			return err
		}
	}
	WriteOpaque(right, Opaque{Prev: blk, Next: o.Next, Level: o.Level, Flags: o.Flags &^ FlagRoot})

	left := page.New(SpecialSize)
	if _, err := left.AddItem(pivot(sep, None)); err != nil {
		return err
	}
	for _, x := range items[:k] {
		if _, err := left.AddItem(x); err != nil {
			return err
		}
	}
	WriteOpaque(left, Opaque{Prev: o.Prev, Next: rightBlk, Level: o.Level, Flags: o.Flags &^ FlagRoot})

	if err := t.writePage(rightBlk, right); err != nil {
		return err
	}
	if err := t.writePage(blk, left); err != nil {
		return err
	}
	if o.Next != None {
		err := t.modify(o.Next, func(p page.Page) error {
			no := ReadOpaque(p)
			no.Prev = rightBlk
			WriteOpaque(p, no)
			return nil
		})
		if err != nil {
			return err
		}
	}

	down := pivot(sep, rightBlk)
	if len(path) == 0 {
		return t.newRoot(blk, down, o.Level+1)
	}
	parent := path[len(path)-1]
	pp, err := t.readPage(parent.blk)
	if err != nil {
		return err
	}
	return t.insertAt(parent.blk, pp, parent.off+1, down, path[:len(path)-1])
}

// splitPoint is _bt_findsplitloc: it returns how many of items go to
// the left page. Both halves must fit, with the left page's new high key
// (items[k]) and the right page's inherited one. The split is balanced
// by bytes, except that an append to the rightmost page leaves the left
// page FillFactor percent full, so ascending inserts pack pages.
func splitPoint(items []IndexTuple, oldHigh IndexTuple, rightmostAppend bool) int {
	size := func(it IndexTuple) int { return (len(it)+7)&^7 + page.LinePointerSize }
	usable := page.PageSize - page.HeaderSize - SpecialSize
	total := 0
	for _, it := range items {
		total += size(it)
	}
	target := total / 2
	if rightmostAppend {
		target = usable * FillFactor / 100
	}
	best, bestDist := -1, 0
	leftSize := 0
	for k := 1; k < len(items); k++ {
		leftSize += size(items[k-1])
		l := leftSize + size(items[k]) + TIDSize // plus the high key
		r := total - leftSize
		if oldHigh != nil {
			r += size(oldHigh)
		}
		if l > usable || r > usable {
			continue
		}
		dist := l - target
		if dist < 0 {
			dist = -dist
		}
		if best < 0 || dist < bestDist {
			best, bestDist = k, dist
		}
	}
	if best < 0 {
		best = len(items) / 2
	}
	return best
}

// newRoot is _bt_newroot: after the root split into left and right, a
// new root at level holds a minus infinity downlink to left and down.
func (t *Tree) newRoot(left tuple.BlockNumber, down IndexTuple, level uint32) error {
	blk, err := t.newPage()
	if err != nil {
		return err
	}
	p := page.New(SpecialSize)
	if _, err := p.AddItem(minusInf(left)); err != nil {
		return err
	}
	if _, err := p.AddItem(down); err != nil {
		return err
	}
	WriteOpaque(p, Opaque{Level: level, Flags: FlagRoot})
	if err := t.writePage(blk, p); err != nil {
		return err
	}
	return t.writeMeta(Meta{Root: blk, Level: level})
}

// Search returns the leaf block that key (nil is NULL) belongs on and
// the item number of the first entry there with a key >= key, which is
// one past the last item when there is none. An empty tree returns
// None and page.InvalidOffsetNumber. Returns ErrKeyType if key has the
// wrong type; ErrCorrupt as Insert does.
// PostgreSQL: _bt_search and _bt_binsrch in nbtsearch.c.
func (t *Tree) Search(key tuple.Datum) (tuple.BlockNumber, page.OffsetNumber, error) {
	if key != nil {
		if err := checkKey(t.typ, key); err != nil {
			return None, page.InvalidOffsetNumber, err
		}
	}
	k := skey{key: key, tidMode: -1}
	leaf, blk, _, err := t.descend(k)
	if err != nil || leaf == nil {
		return None, page.InvalidOffsetNumber, err
	}
	return blk, t.binsrch(leaf, ReadOpaque(leaf), k), nil
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
	s := &Scan{t: t, hi: hi}
	for _, b := range []*Bound{lo, hi} {
		if b != nil {
			if err := checkKey(t.typ, b.Key); err != nil {
				s.err = err
				return s
			}
		}
	}
	k := skey{minusInf: true}
	if lo != nil {
		k = skey{key: lo.Key, tidMode: 1}
		if lo.Inclusive {
			k.tidMode = -1
		}
	}
	leaf, blk, _, err := t.descend(k)
	if err != nil {
		s.err = err
		return s
	}
	if leaf == nil {
		s.done = true
		return s
	}
	s.pos = -1
	s.load(blk, leaf, t.binsrch(leaf, ReadOpaque(leaf), k))
	return s
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

// load copies the entries of leaf p from off on into s.items, stopping at
// the upper bound.
func (s *Scan) load(blk tuple.BlockNumber, p page.Page, off page.OffsetNumber) {
	o := ReadOpaque(p)
	s.items = s.items[:0]
	for ; off <= p.NumItems(); off++ {
		it := item(p, off)
		if it == nil {
			s.err = fmt.Errorf("%w: block %d: unused item %d", ErrCorrupt, blk, off)
			return
		}
		if s.hi != nil {
			c := compareNullable(s.t.typ, it.Key(s.t.typ), s.hi.Key)
			if c > 0 || (c == 0 && !s.hi.Inclusive) {
				s.done = true
				break
			}
		}
		s.items = append(s.items, append(IndexTuple(nil), it...))
	}
	s.next = o.Next
}

// Next advances to the next entry. It returns false at the upper bound,
// at the end of the leaf chain, or once an error is set.
// PostgreSQL: _bt_next in nbtsearch.c.
func (s *Scan) Next() bool {
	if s.err != nil {
		return false
	}
	s.pos++
	for s.pos >= len(s.items) {
		if s.done || s.next == None {
			s.done = true
			return false
		}
		p, err := s.t.readPage(s.next)
		if err != nil {
			s.err = err
			return false
		}
		blk := s.next
		s.load(blk, p, firstData(ReadOpaque(p)))
		if s.err != nil {
			return false
		}
		s.pos = 0
	}
	return true
}

// TID returns the current entry's heap TID.
func (s *Scan) TID() tuple.TID { return s.items[s.pos].TID() }

// Key returns the current entry's key, nil for NULL.
func (s *Scan) Key() tuple.Datum { return s.items[s.pos].Key(s.t.typ) }

// IsNull reports whether the current entry's key is NULL.
func (s *Scan) IsNull() bool { return s.items[s.pos].IsNull() }

// Err returns the first error encountered by the scan.
func (s *Scan) Err() error { return s.err }

// Close releases the scan.
func (s *Scan) Close() {
	s.done = true
	s.items = nil
}

// Check walks every page and verifies the tree's invariants, like
// amcheck's bt_index_check: the metapage, page flags and levels,
// sibling links, item order, high keys against parent downlinks, and
// that every block is reached exactly once. The error wraps ErrCorrupt
// and names the block; I/O errors are returned as they are.
// PostgreSQL: bt_check_every_level in verify_nbtree.c.
func (t *Tree) Check() error {
	nblocks, err := t.pool.Store().NBlocks(t.oid)
	if err != nil {
		return err
	}
	meta, err := t.Meta()
	if err != nil {
		return err
	}
	if meta.Root == None {
		if nblocks != 1 {
			return fmt.Errorf("%w: empty tree has %d blocks", ErrCorrupt, nblocks)
		}
		return nil
	}
	seen := map[tuple.BlockNumber]bool{MetaBlock: true}
	// expect is what the parent level says about each child: its high
	// key must equal the next downlink, or it must be rightmost.
	type expectation struct {
		high      IndexTuple
		rightmost bool
	}
	expect := map[tuple.BlockNumber]expectation{meta.Root: {rightmost: true}}
	order := []tuple.BlockNumber{meta.Root}
	for level := meta.Level; ; level-- {
		var nextOrder []tuple.BlockNumber
		nextExpect := map[tuple.BlockNumber]expectation{}
		var prevHigh IndexTuple
		i := 0
		for blk, prev := order[0], None; blk != None; i++ {
			fail := func(format string, args ...any) error {
				return fmt.Errorf("%w: block %d: %s", ErrCorrupt, blk, fmt.Sprintf(format, args...))
			}
			if blk >= nblocks {
				return fail("past the end of the relation")
			}
			if seen[blk] {
				return fail("reached twice")
			}
			seen[blk] = true
			if i >= len(order) || order[i] != blk {
				return fail("sibling chain disagrees with the parent's downlinks")
			}
			e := expect[blk]
			p, err := t.readPage(blk)
			if err != nil {
				return err
			}
			o := ReadOpaque(p)
			if int(p.Special()) != page.PageSize-SpecialSize {
				return fail("wrong special space size")
			}
			if o.Level != level {
				return fail("level %d, want %d", o.Level, level)
			}
			leaf := o.Flags&FlagLeaf != 0
			if leaf != (level == 0) || o.Flags&FlagMeta != 0 {
				return fail("flags %#x at level %d", o.Flags, level)
			}
			if (o.Flags&FlagRoot != 0) != (blk == meta.Root) {
				return fail("root flag %v", o.Flags&FlagRoot != 0)
			}
			if o.Prev != prev {
				return fail("prev link %d, want %d", o.Prev, prev)
			}
			if e.rightmost != (o.Next == None) {
				return fail("next link %d, rightmost %v", o.Next, e.rightmost)
			}
			n, first := p.NumItems(), firstData(o)
			if n < first {
				return fail("no data items")
			}
			var high IndexTuple
			if o.Next != None {
				high = item(p, 1)
				if high == nil || !high.IsPivot() {
					return fail("high key is not a pivot tuple")
				}
				if !e.rightmost && (high.IsNull() != e.high.IsNull() || !bytes.Equal(high[HeaderSize:], e.high[HeaderSize:])) {
					return fail("high key differs from the parent's next downlink")
				}
			}
			var last IndexTuple
			for off := first; off <= n; off++ {
				it := item(p, off)
				if it == nil {
					return fail("item %d is unused", off)
				}
				if leaf && it.IsPivot() {
					return fail("item %d is a pivot tuple on a leaf", off)
				}
				if !leaf {
					if !it.IsPivot() {
						return fail("item %d is not a pivot tuple", off)
					}
					if it.IsMinusInf() != (off == first) {
						return fail("item %d: minus infinity %v", off, it.IsMinusInf())
					}
					child := it.TID().Block
					nextOrder = append(nextOrder, child)
					if off > first {
						nextExpect[nextOrder[len(nextOrder)-2]] = expectation{high: it}
					}
				}
				if !it.IsMinusInf() {
					if last != nil && t.compareTuples(last, it) >= 0 {
						return fail("items %d and %d out of order", off-1, off)
					}
					if last == nil && prevHigh != nil && t.compareTuples(it, prevHigh) < 0 {
						return fail("item %d is below the previous page's high key", off)
					}
					last = it
				}
			}
			if high != nil && last != nil && t.compareTuples(last, high) >= 0 {
				return fail("item %d is not below the high key", n)
			}
			if !leaf {
				lastChild := nextOrder[len(nextOrder)-1]
				if high != nil {
					nextExpect[lastChild] = expectation{high: high}
				} else {
					nextExpect[lastChild] = expectation{rightmost: true}
				}
			}
			prevHigh, prev, blk = high, blk, o.Next
		}
		if i != len(order) {
			return fmt.Errorf("%w: level %d has %d pages, parent downlinks name %d", ErrCorrupt, level, i, len(order))
		}
		if level == 0 {
			break
		}
		order, expect = nextOrder, nextExpect
	}
	for blk := tuple.BlockNumber(1); blk < nblocks; blk++ {
		if !seen[blk] {
			return fmt.Errorf("%w: block %d is not reachable", ErrCorrupt, blk)
		}
	}
	return nil
}
