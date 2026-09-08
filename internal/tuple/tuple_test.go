package tuple

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/raphi011/build-postgres/internal/page"
)

var desc4 = NewDesc(
	Attr{Name: "a", Type: Int4},
	Attr{Name: "b", Type: Text},
	Attr{Name: "c", Type: Bool},
	Attr{Name: "d", Type: Int8},
)

func TestTypeIDs(t *testing.T) {
	for _, tc := range []struct {
		id    TypeID
		name  string
		size  int
		align int
	}{
		{Int4, "int4", 4, 4},
		{Int8, "int8", 8, 8},
		{Bool, "bool", 1, 1},
		{Text, "text", -1, 4},
	} {
		if tc.id.String() != tc.name || tc.id.Size() != tc.size || tc.id.Align() != tc.align {
			t.Errorf("%d: got (%s, %d, %d), want (%s, %d, %d)", tc.id, tc.id, tc.id.Size(), tc.id.Align(), tc.name, tc.size, tc.align)
		}
	}
}

func TestFormGoldenNoNulls(t *testing.T) {
	tup, err := Form(desc4, []Datum{int32(1), "hi", true, int64(2)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0, 0, 0, 0, // xmin
		0, 0, 0, 0, // xmax
		0, 0, 0, 0, 0, 0, // ctid
		4, 0, // infomask2: natts = 4
		0, 0, // infomask
		24,            // hoff
		0, 0, 0, 0, 0, // padding to 24
		1, 0, 0, 0, // a = 1 at 24
		2, 0, 0, 0, // b length at 28
		'h', 'i', // b bytes at 32
		1,             // c = true at 34
		0, 0, 0, 0, 0, // padding to 40
		2, 0, 0, 0, 0, 0, 0, 0, // d = 2 at 40
	}
	if !bytes.Equal(tup, want) {
		t.Fatalf("tuple bytes\n got % x\nwant % x", []byte(tup), want)
	}
	if tup.Natts() != 4 || tup.Hoff() != 24 || tup.Infomask() != 0 {
		t.Fatalf("natts=%d hoff=%d infomask=%#x", tup.Natts(), tup.Hoff(), tup.Infomask())
	}
}

func TestFormGoldenWithNulls(t *testing.T) {
	tup, err := Form(desc4, []Datum{nil, "hi", nil, int64(2)}, []bool{true, false, true, false})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0, 0, 0, 0, // xmin
		0, 0, 0, 0, // xmax
		0, 0, 0, 0, 0, 0, // ctid
		4, 0, // infomask2: natts = 4
		1, 0, // infomask: HasNull
		24,         // hoff
		0x0A,       // null bitmap: attrs 1 and 3 present
		0, 0, 0, 0, // padding to 24
		2, 0, 0, 0, // b length at 24
		'h', 'i', // b bytes at 28
		0, 0, // padding to 32
		2, 0, 0, 0, 0, 0, 0, 0, // d = 2 at 32
	}
	if !bytes.Equal(tup, want) {
		t.Fatalf("tuple bytes\n got % x\nwant % x", []byte(tup), want)
	}
	for i, wantNull := range []bool{true, false, true, false} {
		if tup.IsNull(i) != wantNull {
			t.Errorf("IsNull(%d) = %v, want %v", i, tup.IsNull(i), wantNull)
		}
	}
	if !tup.IsNull(4) || !tup.IsNull(100) {
		t.Error("IsNull past natts should be true")
	}
}

func TestNullBitmapOnlyWhenNeeded(t *testing.T) {
	tup, err := Form(desc4, []Datum{int32(1), "hi", true, int64(2)}, []bool{false, false, false, false})
	if err != nil {
		t.Fatal(err)
	}
	if tup.Infomask()&HasNull != 0 {
		t.Fatal("HasNull set with all-false nulls slice")
	}
	if tup.Hoff() != 24 {
		t.Fatalf("hoff = %d", tup.Hoff())
	}
}

func TestHoffGrowsWithWideBitmap(t *testing.T) {
	// 41 attributes need 6 bitmap bytes: 19 + 6 = 25, rounded to 32.
	var attrs []Attr
	values := make([]Datum, 41)
	nulls := make([]bool, 41)
	for i := range values {
		attrs = append(attrs, Attr{Name: "c", Type: Bool})
		values[i] = true
	}
	nulls[40] = true
	values[40] = nil
	tup, err := Form(NewDesc(attrs...), values, nulls)
	if err != nil {
		t.Fatal(err)
	}
	if tup.Hoff() != 32 {
		t.Fatalf("hoff = %d, want 32", tup.Hoff())
	}
	if len(tup) != 32+40 {
		t.Fatalf("len = %d, want 72", len(tup))
	}
	// 40 attributes with nulls: 19 + 5 = 24, no growth.
	tup, err = Form(NewDesc(attrs[:40]...), values[:40], nulls[:40])
	if err != nil {
		t.Fatal(err)
	}
	if tup.Hoff() != 24 {
		t.Fatalf("hoff for 40 attrs = %d, want 24", tup.Hoff())
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		values []Datum
		nulls  []bool
	}{
		{[]Datum{int32(-7), "", false, int64(-1 << 62)}, nil},
		{[]Datum{int32(1 << 30), "héllo wörld", true, int64(0)}, nil},
		{[]Datum{nil, nil, nil, nil}, []bool{true, true, true, true}},
		{[]Datum{int32(5), nil, true, nil}, []bool{false, true, false, true}},
		{[]Datum{nil, "x", nil, int64(9)}, []bool{true, false, true, false}},
	}
	for i, tc := range cases {
		tup, err := Form(desc4, tc.values, tc.nulls)
		if err != nil {
			t.Fatalf("case %d: Form: %v", i, err)
		}
		values, nulls, err := Deform(desc4, tup)
		if err != nil {
			t.Fatalf("case %d: Deform: %v", i, err)
		}
		if len(values) != 4 || len(nulls) != 4 {
			t.Fatalf("case %d: lengths %d/%d", i, len(values), len(nulls))
		}
		for j := range values {
			wantNull := tc.nulls != nil && tc.nulls[j]
			if nulls[j] != wantNull {
				t.Fatalf("case %d attr %d: null = %v, want %v", i, j, nulls[j], wantNull)
			}
			if wantNull {
				if values[j] != nil {
					t.Fatalf("case %d attr %d: null value = %v, want nil", i, j, values[j])
				}
				continue
			}
			if values[j] != tc.values[j] {
				t.Fatalf("case %d attr %d: %v (%T), want %v (%T)", i, j, values[j], values[j], tc.values[j], tc.values[j])
			}
		}
	}
}

func TestDeformDoesNotAlias(t *testing.T) {
	tup, _ := Form(desc4, []Datum{int32(1), "abc", true, int64(2)}, nil)
	values, _, _ := Deform(desc4, tup)
	for i := range tup {
		tup[i] = 0xFF
	}
	if values[1].(string) != "abc" {
		t.Fatalf("string aliases tuple: %q", values[1])
	}
}

func TestDeformMissingTrailingAttrsAreNull(t *testing.T) {
	old := NewDesc(desc4.Attrs[:2]...)
	tup, err := Form(old, []Datum{int32(1), "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, nulls, err := Deform(desc4, tup)
	if err != nil {
		t.Fatal(err)
	}
	if values[0] != int32(1) || values[1] != "hi" {
		t.Fatalf("values = %v", values)
	}
	if !nulls[2] || !nulls[3] || values[2] != nil || values[3] != nil {
		t.Fatalf("trailing attrs not null: %v %v", values, nulls)
	}
	if tup.Natts() != 2 {
		t.Fatalf("natts = %d", tup.Natts())
	}
}

func TestFormErrors(t *testing.T) {
	if _, err := Form(desc4, []Datum{int32(1)}, nil); !errors.Is(err, ErrArity) {
		t.Errorf("short values: %v", err)
	}
	if _, err := Form(desc4, []Datum{int32(1), "a", true, int64(1)}, []bool{false}); !errors.Is(err, ErrArity) {
		t.Errorf("short nulls: %v", err)
	}
	for _, values := range [][]Datum{
		{int64(1), "a", true, int64(1)}, // int8 where int4 expected
		{int32(1), 5, true, int64(1)},   // int where text expected
		{int32(1), "a", 1, int64(1)},    // int where bool expected
		{int32(1), "a", true, int32(1)}, // int4 where int8 expected
		{nil, "a", true, int64(1)},      // nil without nulls slice
	} {
		if _, err := Form(desc4, values, nil); !errors.Is(err, ErrType) {
			t.Errorf("values %v: err = %v, want ErrType", values, err)
		}
	}
}

func TestDeformErrors(t *testing.T) {
	tup, _ := Form(desc4, []Datum{int32(1), "hello", true, int64(2)}, nil)
	narrow := NewDesc(desc4.Attrs[:3]...)
	if _, _, err := Deform(narrow, tup); !errors.Is(err, ErrCorrupt) {
		t.Errorf("more attrs than descriptor: %v", err)
	}
	if _, _, err := Deform(desc4, tup[:30]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated tuple: %v", err)
	}
	if _, _, err := Deform(desc4, tup[:10]); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated header: %v", err)
	}
	if _, _, err := Deform(desc4, nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("empty tuple: %v", err)
	}
}

func TestHeaderFields(t *testing.T) {
	tup, _ := Form(desc4, []Datum{int32(1), "a", true, int64(1)}, nil)
	if tup.Xmin() != InvalidXID || tup.Xmax() != InvalidXID || tup.Ctid() != (TID{}) {
		t.Fatal("fresh tuple has non-zero transaction fields")
	}
	tup.SetXmin(0x01020304)
	tup.SetXmax(0x0A0B0C0D)
	tup.SetCtid(TID{Block: 0x11223344, Off: 0x5566})
	tup.SetInfomask(tup.Infomask() | XminCommitted | XmaxInvalid)
	if tup.Xmin() != 0x01020304 || tup.Xmax() != 0x0A0B0C0D {
		t.Fatalf("xmin/xmax = %#x/%#x", tup.Xmin(), tup.Xmax())
	}
	if tup.Ctid() != (TID{Block: 0x11223344, Off: 0x5566}) {
		t.Fatalf("ctid = %+v", tup.Ctid())
	}
	if tup.Infomask() != XminCommitted|XmaxInvalid {
		t.Fatalf("infomask = %#x", tup.Infomask())
	}
	want := []byte{4, 3, 2, 1, 0x0D, 0x0C, 0x0B, 0x0A, 0x44, 0x33, 0x22, 0x11, 0x66, 0x55, 4, 0, 0x00, 0x09, 24}
	if !bytes.Equal(tup[:FixedHeaderSize], want) {
		t.Fatalf("header bytes\n got % x\nwant % x", []byte(tup[:FixedHeaderSize]), want)
	}
	// Setting header fields does not disturb the data.
	values, _, err := Deform(desc4, tup)
	if err != nil || values[1] != "a" {
		t.Fatalf("data after header writes: %v %v", values, err)
	}
	// Setting infomask must not clear HasNull.
	tup2, _ := Form(desc4, []Datum{nil, "a", true, int64(1)}, []bool{true, false, false, false})
	tup2.SetInfomask(tup2.Infomask() | XminCommitted)
	if tup2.Infomask()&HasNull == 0 {
		t.Fatal("HasNull lost")
	}
}

func TestTupleFitsOnPage(t *testing.T) {
	tup, _ := Form(desc4, []Datum{int32(1), "hello", true, int64(2)}, nil)
	p := page.New(0)
	n, err := p.AddItem(tup)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := p.GetItem(n)
	values, _, err := Deform(desc4, Tuple(raw))
	if err != nil || values[1] != "hello" {
		t.Fatalf("tuple from page: %v %v", values, err)
	}
}

func TestRandomRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	types := []TypeID{Int4, Int8, Bool, Text}
	for iter := 0; iter < 500; iter++ {
		n := 1 + rng.Intn(20)
		attrs := make([]Attr, n)
		values := make([]Datum, n)
		nulls := make([]bool, n)
		for i := range attrs {
			attrs[i] = Attr{Name: "c", Type: types[rng.Intn(4)]}
			if rng.Intn(4) == 0 {
				nulls[i] = true
				continue
			}
			switch attrs[i].Type {
			case Int4:
				values[i] = int32(rng.Int63())
			case Int8:
				values[i] = int64(rng.Uint64())
			case Bool:
				values[i] = rng.Intn(2) == 1
			case Text:
				b := make([]byte, rng.Intn(50))
				rng.Read(b)
				values[i] = string(b)
			}
		}
		d := NewDesc(attrs...)
		tup, err := Form(d, values, nulls)
		if err != nil {
			t.Fatalf("iter %d: Form: %v", iter, err)
		}
		got, gotNulls, err := Deform(d, tup)
		if err != nil {
			t.Fatalf("iter %d: Deform: %v", iter, err)
		}
		for i := range attrs {
			if gotNulls[i] != nulls[i] {
				t.Fatalf("iter %d attr %d: null %v, want %v", iter, i, gotNulls[i], nulls[i])
			}
			if !nulls[i] && got[i] != values[i] {
				t.Fatalf("iter %d attr %d (%s): %v, want %v", iter, i, attrs[i].Type, got[i], values[i])
			}
		}
	}
}
