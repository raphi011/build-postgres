// Package tuple defines the on-disk heap tuple format and the value types
// it stores. See chapters/02-tuples.
package tuple

import (
	"encoding/binary"
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
	switch t {
	case Int4:
		return "int4"
	case Int8:
		return "int8"
	case Bool:
		return "bool"
	case Text:
		return "text"
	}
	return fmt.Sprintf("TypeID(%d)", uint8(t))
}

// Size returns the on-disk size of a value, or -1 for variable-length types.
func (t TypeID) Size() int {
	switch t {
	case Int4:
		return 4
	case Int8:
		return 8
	case Bool:
		return 1
	case Text:
		return -1
	}
	panic("tuple: unknown type " + t.String())
}

// Align returns the alignment of the type within a tuple.
func (t TypeID) Align() int {
	switch t {
	case Int4, Text:
		return 4
	case Int8:
		return 8
	case Bool:
		return 1
	}
	panic("tuple: unknown type " + t.String())
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

func alignTo(n, a int) int { return (n + a - 1) &^ (a - 1) }

// Tuple is an encoded heap tuple: header followed by column data.
type Tuple []byte

// Form encodes values according to d. nulls may be nil.
// Returns ErrArity if len(values), or a non-nil nulls, disagrees with
// d.Len(); ErrType if a non-null value (including nil when nulls is nil)
// has the wrong Go type. NotNull is not enforced here.
// PostgreSQL: heap_form_tuple in heaptuple.c.
func Form(d *Desc, values []Datum, nulls []bool) (Tuple, error) {
	n := d.Len()
	if len(values) != n || (nulls != nil && len(nulls) != n) {
		return nil, ErrArity
	}
	hasNull := false
	for i := range nulls {
		hasNull = hasNull || nulls[i]
	}

	// Header.
	hoff := FixedHeaderSize
	if hasNull {
		hoff += (n + 7) / 8
	}
	hoff = alignTo(hoff, 8)
	t := make(Tuple, hoff, hoff+16*n)
	binary.LittleEndian.PutUint16(t[offInfomask2:], uint16(n)&nattsMask)
	if hasNull {
		binary.LittleEndian.PutUint16(t[offInfomask:], HasNull)
	}
	t[offHoff] = uint8(hoff)
	if hasNull {
		bitmap := t[FixedHeaderSize:]
		for i, isNull := range nulls {
			if !isNull {
				bitmap[i>>3] |= 1 << (i & 7)
			}
		}
	}

	// Data.
	for i, a := range d.Attrs {
		if nulls != nil && nulls[i] {
			continue
		}
		v := values[i]
		pad := alignTo(len(t), a.Type.Align()) - len(t)
		t = append(t, make([]byte, pad)...)
		switch a.Type {
		case Int4:
			x, ok := v.(int32)
			if !ok {
				return nil, typeError(a, v)
			}
			t = binary.LittleEndian.AppendUint32(t, uint32(x))
		case Int8:
			x, ok := v.(int64)
			if !ok {
				return nil, typeError(a, v)
			}
			t = binary.LittleEndian.AppendUint64(t, uint64(x))
		case Bool:
			x, ok := v.(bool)
			if !ok {
				return nil, typeError(a, v)
			}
			if x {
				t = append(t, 1)
			} else {
				t = append(t, 0)
			}
		case Text:
			x, ok := v.(string)
			if !ok {
				return nil, typeError(a, v)
			}
			t = binary.LittleEndian.AppendUint32(t, uint32(len(x)))
			t = append(t, x...)
		default:
			panic("tuple: unknown type " + a.Type.String())
		}
	}
	return t, nil
}

// Deform decodes t according to d. Returned values do not alias t.
// Attributes beyond Natts are NULL. Returns ErrCorrupt if t is shorter
// than the fixed header, stores more attributes than d has, or its data
// runs past len(t).
// PostgreSQL: heap_deform_tuple in heaptuple.c.
func Deform(d *Desc, t Tuple) (values []Datum, nulls []bool, err error) {
	if len(t) < FixedHeaderSize {
		return nil, nil, fmt.Errorf("%w: %d-byte tuple", ErrCorrupt, len(t))
	}
	natts := t.Natts()
	if natts > d.Len() {
		return nil, nil, fmt.Errorf("%w: tuple has %d attributes, descriptor %d", ErrCorrupt, natts, d.Len())
	}
	hoff := t.Hoff()
	if hoff > len(t) {
		return nil, nil, fmt.Errorf("%w: hoff %d past end", ErrCorrupt, hoff)
	}
	values = make([]Datum, d.Len())
	nulls = make([]bool, d.Len())
	pos := hoff
	for i, a := range d.Attrs {
		if i >= natts || t.IsNull(i) {
			nulls[i] = true
			continue
		}
		pos = alignTo(pos, a.Type.Align())
		size := a.Type.Size()
		if size < 0 {
			if pos+4 > len(t) {
				return nil, nil, fmt.Errorf("%w: attribute %d length past end", ErrCorrupt, i)
			}
			size = 4 + int(binary.LittleEndian.Uint32(t[pos:]))
		}
		if pos+size > len(t) {
			return nil, nil, fmt.Errorf("%w: attribute %d data past end", ErrCorrupt, i)
		}
		b := t[pos : pos+size]
		switch a.Type {
		case Int4:
			values[i] = int32(binary.LittleEndian.Uint32(b))
		case Int8:
			values[i] = int64(binary.LittleEndian.Uint64(b))
		case Bool:
			values[i] = b[0] != 0
		case Text:
			values[i] = string(b[4:])
		}
		pos += size
	}
	return values, nulls, nil
}

// Natts returns the number of attributes stored in the tuple.
// PostgreSQL: HeapTupleHeaderGetNatts in htup_details.h.
func (t Tuple) Natts() int {
	return int(binary.LittleEndian.Uint16(t[offInfomask2:]) & nattsMask)
}

// Hoff returns the offset of column data.
func (t Tuple) Hoff() int { return int(t[offHoff]) }

// IsNull reports whether attribute i (0-based) is NULL. True for
// i >= Natts(); false for every i when HasNull is clear.
// PostgreSQL: heap_attisnull in heaptuple.c.
func (t Tuple) IsNull(i int) bool {
	if i >= t.Natts() {
		return true
	}
	if t.Infomask()&HasNull == 0 {
		return false
	}
	return t[FixedHeaderSize+i>>3]&(1<<(i&7)) == 0
}

// Xmin, Xmax, Ctid, and Infomask read header fields in place; the Set
// variants overwrite them. SetInfomask stores the whole mask, so OR new
// bits into Infomask() rather than replacing it, or HasNull is lost.
// PostgreSQL: HeapTupleHeaderGetXmin and the other HeapTupleHeaderGet/Set
// macros in htup_details.h.
func (t Tuple) Xmin() XID     { return XID(binary.LittleEndian.Uint32(t[offXmin:])) }
func (t Tuple) SetXmin(x XID) { binary.LittleEndian.PutUint32(t[offXmin:], uint32(x)) }
func (t Tuple) Xmax() XID     { return XID(binary.LittleEndian.Uint32(t[offXmax:])) }
func (t Tuple) SetXmax(x XID) { binary.LittleEndian.PutUint32(t[offXmax:], uint32(x)) }

func (t Tuple) Ctid() TID {
	return TID{
		Block: BlockNumber(binary.LittleEndian.Uint32(t[offCtid:])),
		Off:   page.OffsetNumber(binary.LittleEndian.Uint16(t[offCtid+4:])),
	}
}

func (t Tuple) SetCtid(id TID) {
	binary.LittleEndian.PutUint32(t[offCtid:], uint32(id.Block))
	binary.LittleEndian.PutUint16(t[offCtid+4:], uint16(id.Off))
}

func (t Tuple) Infomask() uint16     { return binary.LittleEndian.Uint16(t[offInfomask:]) }
func (t Tuple) SetInfomask(m uint16) { binary.LittleEndian.PutUint16(t[offInfomask:], m) }
