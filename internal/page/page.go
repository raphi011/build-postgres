// Package page implements the 8 KiB slotted page that every relation file
// is made of. See chapters/01-pages.
package page

import (
	"encoding/binary"
	"errors"
)

const (
	// PageSize is the size of every page in bytes.
	PageSize = 8192
	// HeaderSize is the size of the page header in bytes.
	HeaderSize = 24
	// LinePointerSize is the size of one line pointer in bytes.
	LinePointerSize = 4
	// LayoutVersion is stored in the low byte of the PageSizeVersion field.
	LayoutVersion = 1
	// MaxItemSize is the largest item that fits on a page with no special
	// space: the page minus the header and one line pointer, aligned down.
	MaxItemSize = (PageSize - HeaderSize - LinePointerSize) &^ 7
)

// Header field offsets.
const (
	offLSN      = 0
	offChecksum = 8
	offFlags    = 10
	offLower    = 12
	offUpper    = 14
	offSpecial  = 16
	offVersion  = 18
	offPruneXID = 20
)

var (
	ErrNoSpace       = errors.New("page: no space for item")
	ErrItemTooLarge  = errors.New("page: item larger than page")
	ErrInvalidOffset = errors.New("page: invalid item number")
	ErrItemUnused    = errors.New("page: item is unused")
)

// OffsetNumber is a 1-based item number within a page.
type OffsetNumber uint16

// InvalidOffsetNumber is never a valid item number.
const InvalidOffsetNumber OffsetNumber = 0

// ItemFlags is the two-bit state of a line pointer.
type ItemFlags uint8

const (
	ItemUnused   ItemFlags = 0
	ItemNormal   ItemFlags = 1
	ItemRedirect ItemFlags = 2 // reserved
	ItemDead     ItemFlags = 3 // reserved
)

// ItemID is the decoded form of a line pointer.
type ItemID struct {
	Off   uint16
	Len   uint16
	Flags ItemFlags
}

func (id ItemID) pack() uint32 {
	return uint32(id.Off)&0x7fff | uint32(id.Flags&3)<<15 | uint32(id.Len)&0x7fff<<17
}

func unpackItemID(v uint32) ItemID {
	return ItemID{
		Off:   uint16(v & 0x7fff),
		Flags: ItemFlags(v >> 15 & 3),
		Len:   uint16(v >> 17 & 0x7fff),
	}
}

func align8(n int) int { return (n + 7) &^ 7 }

// Page is a PageSize-byte slice with the slotted page layout.
type Page []byte

// New allocates and initialises a page with the given special space size.
func New(specialSize int) Page {
	p := make(Page, PageSize)
	p.Init(specialSize)
	return p
}

// Init formats p as an empty page with the given special space size, which
// is rounded up to a multiple of 8. Panics if specialSize is negative or
// leaves no room for the header.
// PostgreSQL: PageInit in bufpage.c.
func (p Page) Init(specialSize int) {
	if specialSize < 0 || align8(specialSize) > PageSize-HeaderSize {
		panic("page: invalid special space size")
	}
	clear(p[:HeaderSize])
	special := PageSize - align8(specialSize)
	p.setU16(offLower, HeaderSize)
	p.setU16(offUpper, uint16(special))
	p.setU16(offSpecial, uint16(special))
	p.setU16(offVersion, PageSize|LayoutVersion)
}

// IsNew reports whether the page is all zero bytes, i.e. never initialised.
// Checking Upper() == 0 is enough: Init never leaves it zero.
// PostgreSQL: PageIsNew in bufpage.h.
func (p Page) IsNew() bool {
	return p.Upper() == 0
}

func (p Page) u16(off int) uint16       { return binary.LittleEndian.Uint16(p[off:]) }
func (p Page) setU16(off int, v uint16) { binary.LittleEndian.PutUint16(p[off:], v) }

// LSN returns the log sequence number stored in the header.
func (p Page) LSN() uint64 {
	return binary.LittleEndian.Uint64(p[offLSN:])
}

// SetLSN stores lsn in the header.
func (p Page) SetLSN(lsn uint64) {
	binary.LittleEndian.PutUint64(p[offLSN:], lsn)
}

// Lower returns the offset of the end of the line pointer array.
func (p Page) Lower() uint16 { return p.u16(offLower) }

// Upper returns the offset of the start of item data.
func (p Page) Upper() uint16 { return p.u16(offUpper) }

// Special returns the offset of the special space.
func (p Page) Special() uint16 { return p.u16(offSpecial) }

// SpecialSpace returns the special space as a slice aliasing the page.
// PostgreSQL: PageGetSpecialPointer in bufpage.h.
func (p Page) SpecialSpace() []byte {
	return p[p.Special():PageSize]
}

// NumItems returns the number of line pointers on the page, used or not:
// (Lower() - HeaderSize) / LinePointerSize, and zero if Lower() is at or
// below HeaderSize, as on a never-initialised page.
// PostgreSQL: PageGetMaxOffsetNumber in bufpage.h.
func (p Page) NumItems() OffsetNumber {
	if p.Lower() <= HeaderSize {
		return 0
	}
	return OffsetNumber((p.Lower() - HeaderSize) / LinePointerSize)
}

func lpOffset(n OffsetNumber) int {
	return HeaderSize + (int(n)-1)*LinePointerSize
}

// ItemID returns the decoded line pointer n. It panics if n is outside
// 1..NumItems().
// PostgreSQL: PageGetItemId in bufpage.h.
func (p Page) ItemID(n OffsetNumber) ItemID {
	if n < 1 || n > p.NumItems() {
		panic("page: item number out of range")
	}
	return unpackItemID(binary.LittleEndian.Uint32(p[lpOffset(n):]))
}

func (p Page) setItemID(n OffsetNumber, id ItemID) {
	binary.LittleEndian.PutUint32(p[lpOffset(n):], id.pack())
}

// FreeSpace returns the bytes available for a new item, assuming a new line
// pointer is needed: Upper() - Lower() - LinePointerSize, never negative.
// PostgreSQL: PageGetFreeSpace in bufpage.c.
func (p Page) FreeSpace() int {
	space := int(p.Upper()) - int(p.Lower())
	if space < LinePointerSize {
		return 0
	}
	return space - LinePointerSize
}

// AddItem stores item on the page and returns its item number. It reuses
// the lowest-numbered unused line pointer, else appends one.
// Returns ErrItemTooLarge if len(item) > MaxItemSize; ErrNoSpace if the
// 8-aligned item plus any new line pointer does not fit between lower and
// upper. A failed call leaves the page unchanged.
// PostgreSQL: PageAddItemExtended in bufpage.c.
func (p Page) AddItem(item []byte) (OffsetNumber, error) {
	if len(item) > MaxItemSize {
		return InvalidOffsetNumber, ErrItemTooLarge
	}
	// Reuse the lowest-numbered unused line pointer, if any.
	n := InvalidOffsetNumber
	for i := OffsetNumber(1); i <= p.NumItems(); i++ {
		if p.ItemID(i).Flags == ItemUnused {
			n = i
			break
		}
	}
	lower := int(p.Lower())
	if n == InvalidOffsetNumber {
		n = p.NumItems() + 1
		lower += LinePointerSize
	}
	upper := int(p.Upper()) - align8(len(item))
	if upper < lower {
		return InvalidOffsetNumber, ErrNoSpace
	}
	copy(p[upper:], item)
	p.setU16(offLower, uint16(lower))
	p.setU16(offUpper, uint16(upper))
	p.setItemID(n, ItemID{Off: uint16(upper), Len: uint16(len(item)), Flags: ItemNormal})
	return n, nil
}

// GetItem returns item n as a slice aliasing the page.
// Returns ErrInvalidOffset if n is outside 1..NumItems(); ErrItemUnused if
// line pointer n is not normal.
// PostgreSQL: PageGetItem in bufpage.h.
func (p Page) GetItem(n OffsetNumber) ([]byte, error) {
	if n < 1 || n > p.NumItems() {
		return nil, ErrInvalidOffset
	}
	id := p.ItemID(n)
	if id.Flags != ItemNormal {
		return nil, ErrItemUnused
	}
	return p[id.Off : int(id.Off)+int(id.Len)], nil
}

// DeleteItem marks item n unused. Its line pointer stays in place, with
// offset and length zeroed.
// Returns ErrInvalidOffset if n is outside 1..NumItems(); ErrItemUnused if
// it is already unused.
// PostgreSQL: ItemIdSetUnused in itemid.h.
func (p Page) DeleteItem(n OffsetNumber) error {
	if n < 1 || n > p.NumItems() {
		return ErrInvalidOffset
	}
	if p.ItemID(n).Flags != ItemNormal {
		return ErrItemUnused
	}
	p.setItemID(n, ItemID{})
	return nil
}

// Compact makes all normal items contiguous at the end of the page and drops
// trailing unused line pointers. Item numbers of normal items are unchanged.
// On an empty or all-deleted page lower returns to HeaderSize and upper to
// Special().
// PostgreSQL: PageRepairFragmentation and PageTruncateLinePointerArray in
// bufpage.c.
func (p Page) Compact() {
	// Copy live item data out, then lay it back in from the special space
	// downward. Copying out first sidesteps any overlap between old and new
	// positions.
	type live struct {
		n    OffsetNumber
		data []byte
	}
	var items []live
	last := InvalidOffsetNumber
	for n := OffsetNumber(1); n <= p.NumItems(); n++ {
		id := p.ItemID(n)
		if id.Flags != ItemNormal {
			continue
		}
		last = n
		items = append(items, live{n, append([]byte(nil), p[id.Off:int(id.Off)+int(id.Len)]...)})
	}
	upper := int(p.Special())
	for _, it := range items {
		upper -= align8(len(it.data))
		copy(p[upper:], it.data)
		p.setItemID(it.n, ItemID{Off: uint16(upper), Len: uint16(len(it.data)), Flags: ItemNormal})
	}
	p.setU16(offUpper, uint16(upper))
	p.setU16(offLower, uint16(lpOffset(last+1)))
}
