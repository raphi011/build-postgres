// Package page implements the 8 KiB slotted page that every relation file
// is made of. See chapters/01-pages.
package page

import "errors"

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

// Page is a PageSize-byte slice with the slotted page layout.
type Page []byte

// New allocates and initialises a page with the given special space size.
func New(specialSize int) Page {
	panic("not implemented")
}

// Init formats p as an empty page with the given special space size, which
// is rounded up to a multiple of 8. Panics if specialSize is negative or
// leaves no room for the header.
// PostgreSQL: PageInit in bufpage.c.
func (p Page) Init(specialSize int) {
	panic("not implemented")
}

// IsNew reports whether the page is all zero bytes, i.e. never initialised.
// Checking Upper() == 0 is enough: Init never leaves it zero.
// PostgreSQL: PageIsNew in bufpage.h.
func (p Page) IsNew() bool {
	panic("not implemented")
}

// LSN returns the log sequence number stored in the header.
func (p Page) LSN() uint64 {
	panic("not implemented")
}

// SetLSN stores lsn in the header.
func (p Page) SetLSN(lsn uint64) {
	panic("not implemented")
}

// Lower returns the offset of the end of the line pointer array.
func (p Page) Lower() uint16 {
	panic("not implemented")
}

// Upper returns the offset of the start of item data.
func (p Page) Upper() uint16 {
	panic("not implemented")
}

// Special returns the offset of the special space.
func (p Page) Special() uint16 {
	panic("not implemented")
}

// SpecialSpace returns the special space as a slice aliasing the page.
// PostgreSQL: PageGetSpecialPointer in bufpage.h.
func (p Page) SpecialSpace() []byte {
	panic("not implemented")
}

// NumItems returns the number of line pointers on the page, used or not:
// (Lower() - HeaderSize) / LinePointerSize, and zero if Lower() is at or
// below HeaderSize, as on a never-initialised page.
// PostgreSQL: PageGetMaxOffsetNumber in bufpage.h.
func (p Page) NumItems() OffsetNumber {
	panic("not implemented")
}

// ItemID returns the decoded line pointer n. It panics if n is outside
// 1..NumItems().
// PostgreSQL: PageGetItemId in bufpage.h.
func (p Page) ItemID(n OffsetNumber) ItemID {
	panic("not implemented")
}

// FreeSpace returns the bytes available for a new item, assuming a new line
// pointer is needed: Upper() - Lower() - LinePointerSize, never negative.
// PostgreSQL: PageGetFreeSpace in bufpage.c.
func (p Page) FreeSpace() int {
	panic("not implemented")
}

// AddItem stores item on the page and returns its item number. It reuses
// the lowest-numbered unused line pointer, else appends one.
// Returns ErrItemTooLarge if len(item) > MaxItemSize; ErrNoSpace if the
// 8-aligned item plus any new line pointer does not fit between lower and
// upper. A failed call leaves the page unchanged.
// PostgreSQL: PageAddItemExtended in bufpage.c.
func (p Page) AddItem(item []byte) (OffsetNumber, error) {
	panic("not implemented")
}

// GetItem returns item n as a slice aliasing the page.
// Returns ErrInvalidOffset if n is outside 1..NumItems(); ErrItemUnused if
// line pointer n is not normal.
// PostgreSQL: PageGetItem in bufpage.h.
func (p Page) GetItem(n OffsetNumber) ([]byte, error) {
	panic("not implemented")
}

// DeleteItem marks item n unused. Its line pointer stays in place, with
// offset and length zeroed.
// Returns ErrInvalidOffset if n is outside 1..NumItems(); ErrItemUnused if
// it is already unused.
// PostgreSQL: ItemIdSetUnused in itemid.h.
func (p Page) DeleteItem(n OffsetNumber) error {
	panic("not implemented")
}

// Compact makes all normal items contiguous at the end of the page and drops
// trailing unused line pointers. Item numbers of normal items are unchanged.
// On an empty or all-deleted page lower returns to HeaderSize and upper to
// Special().
// PostgreSQL: PageRepairFragmentation and PageTruncateLinePointerArray in
// bufpage.c.
func (p Page) Compact() {
	panic("not implemented")
}

// InsertItem stores item as item n, moving the line pointers of items n
// and above up by one; n may be NumItems()+1. Unlike AddItem it never
// reuses an unused line pointer. Added by chapter 12 for B-tree pages,
// whose items are kept in key order (PageAddItem with an offset number).
// Returns ErrItemTooLarge if len(item) > MaxItemSize; ErrInvalidOffset if
// n is outside 1..NumItems()+1; ErrNoSpace if the 8-aligned item plus a
// new line pointer does not fit.
// PostgreSQL: PageAddItemExtended in bufpage.c.
func (p Page) InsertItem(n OffsetNumber, item []byte) error {
	panic("not implemented")
}
