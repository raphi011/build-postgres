// Package tuple defines the on-disk heap tuple format and the value types
// it stores. See chapters/02-tuples.
package tuple

import (
	"errors"
	"fmt"

	"github.com/raphi011/build-postgres/internal/page"
)

// XID is a transaction identifier.
type XID uint32

const (
	InvalidXID   XID = 0
	BootstrapXID XID = 1
	FrozenXID    XID = 2
	// FirstNormalXID is the first XID handed to a user transaction.
	FirstNormalXID XID = 3
)

// OID is an object identifier: the name of a relation on disk and the key of
// every catalog row.
type OID uint32

// InvalidOID is never a valid object identifier.
const InvalidOID OID = 0

// BlockNumber is a 0-based page number within a relation file.
type BlockNumber uint32

// InvalidBlockNumber is never a valid block number.
const InvalidBlockNumber BlockNumber = 0xFFFFFFFF

// TID addresses a tuple by block and item number.
type TID struct {
	Block BlockNumber
	Off   page.OffsetNumber
}

// TypeID identifies one of the supported column types.
type TypeID uint8

const (
	Int4 TypeID = iota + 1
	Int8
	Bool
	Text
)

// String returns the SQL name of the type.
func (t TypeID) String() string {
	panic("not implemented")
}

// Size returns the on-disk size of a value, or -1 for variable-length types.
func (t TypeID) Size() int {
	panic("not implemented")
}

// Align returns the alignment of the type within a tuple.
func (t TypeID) Align() int {
	panic("not implemented")
}

// Attr describes one column.
type Attr struct {
	Name    string
	Type    TypeID
	NotNull bool
}

// Desc describes the columns of a tuple.
type Desc struct {
	Attrs []Attr
}

// NewDesc builds a descriptor from attributes.
func NewDesc(attrs ...Attr) *Desc {
	return &Desc{Attrs: attrs}
}

// Len returns the number of attributes.
func (d *Desc) Len() int { return len(d.Attrs) }

// Datum is a column value: int32, int64, bool, or string.
type Datum = any

// Header layout.
const (
	offXmin      = 0
	offXmax      = 4
	offCtid      = 8
	offInfomask2 = 14
	offInfomask  = 16
	offHoff      = 18
	// FixedHeaderSize is the size of the header before the null bitmap.
	FixedHeaderSize = 19
)

// Infomask bits.
const (
	HasNull       uint16 = 0x0001
	XminCommitted uint16 = 0x0100
	XminInvalid   uint16 = 0x0200
	XmaxCommitted uint16 = 0x0400
	XmaxInvalid   uint16 = 0x0800
)

// nattsMask selects the attribute count from infomask2.
const nattsMask = 0x07FF

var (
	ErrArity   = errors.New("tuple: value count does not match descriptor")
	ErrType    = errors.New("tuple: value has wrong type")
	ErrCorrupt = errors.New("tuple: corrupt tuple")
)

// typeError builds an ErrType for attribute a holding v.
func typeError(a Attr, v Datum) error {
	return fmt.Errorf("%w: column %q is %s, got %T", ErrType, a.Name, a.Type, v)
}

// Tuple is an encoded heap tuple: header followed by column data.
type Tuple []byte

// Form encodes values according to d. nulls may be nil.
// Returns ErrArity if len(values), or a non-nil nulls, disagrees with
// d.Len(); ErrType if a non-null value (including nil when nulls is nil)
// has the wrong Go type. NotNull is not enforced here.
// PostgreSQL: heap_form_tuple in heaptuple.c.
func Form(d *Desc, values []Datum, nulls []bool) (Tuple, error) {
	panic("not implemented")
}

// Deform decodes t according to d. Returned values do not alias t.
// Attributes beyond Natts are NULL. Returns ErrCorrupt if t is shorter
// than the fixed header, stores more attributes than d has, or its data
// runs past len(t).
// PostgreSQL: heap_deform_tuple in heaptuple.c.
func Deform(d *Desc, t Tuple) (values []Datum, nulls []bool, err error) {
	panic("not implemented")
}

// Natts returns the number of attributes stored in the tuple.
// PostgreSQL: HeapTupleHeaderGetNatts in htup_details.h.
func (t Tuple) Natts() int {
	panic("not implemented")
}

// Hoff returns the offset of column data.
func (t Tuple) Hoff() int {
	panic("not implemented")
}

// IsNull reports whether attribute i (0-based) is NULL. True for
// i >= Natts(); false for every i when HasNull is clear.
// PostgreSQL: heap_attisnull in heaptuple.c.
func (t Tuple) IsNull(i int) bool {
	panic("not implemented")
}

// Xmin, Xmax, Ctid, and Infomask read header fields in place; the Set
// variants overwrite them. SetInfomask stores the whole mask, so OR new
// bits into Infomask() rather than replacing it, or HasNull is lost.
// PostgreSQL: HeapTupleHeaderGetXmin and the other HeapTupleHeaderGet/Set
// macros in htup_details.h.
func (t Tuple) Xmin() XID            { panic("not implemented") }
func (t Tuple) SetXmin(x XID)        { panic("not implemented") }
func (t Tuple) Xmax() XID            { panic("not implemented") }
func (t Tuple) SetXmax(x XID)        { panic("not implemented") }
func (t Tuple) Ctid() TID            { panic("not implemented") }
func (t Tuple) SetCtid(id TID)       { panic("not implemented") }
func (t Tuple) Infomask() uint16     { panic("not implemented") }
func (t Tuple) SetInfomask(m uint16) { panic("not implemented") }
