package page

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

func TestNewHeaderBytes(t *testing.T) {
	p := New(0)
	if len(p) != PageSize {
		t.Fatalf("len = %d, want %d", len(p), PageSize)
	}
	want := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, // LSN
		0, 0, // Checksum
		0, 0, // Flags
		0x18, 0x00, // Lower = 24
		0x00, 0x20, // Upper = 8192
		0x00, 0x20, // Special = 8192
		0x01, 0x20, // PageSizeVersion = 8192 | 1
		0, 0, 0, 0, // PruneXID
	}
	if got := []byte(p[:HeaderSize]); !bytes.Equal(got, want) {
		t.Fatalf("header bytes\n got % x\nwant % x", got, want)
	}
	if p.Lower() != 24 || p.Upper() != PageSize || p.Special() != PageSize {
		t.Fatalf("lower/upper/special = %d/%d/%d", p.Lower(), p.Upper(), p.Special())
	}
	if p.NumItems() != 0 {
		t.Fatalf("NumItems = %d, want 0", p.NumItems())
	}
	if p.IsNew() {
		t.Fatal("initialised page reported as new")
	}
}

func TestIsNew(t *testing.T) {
	p := make(Page, PageSize)
	if !p.IsNew() {
		t.Fatal("zero page not reported as new")
	}
	p.Init(0)
	if p.IsNew() {
		t.Fatal("initialised page reported as new")
	}
}

func TestNumItemsOnNewPage(t *testing.T) {
	p := make(Page, PageSize)
	if n := p.NumItems(); n != 0 {
		t.Fatalf("NumItems on a zero page = %d, want 0", n)
	}
	p.Init(0)
	if n := p.NumItems(); n != 0 {
		t.Fatalf("NumItems on an empty page = %d, want 0", n)
	}
}

func TestSpecialSpaceIsAligned(t *testing.T) {
	for _, tc := range []struct{ size, wantSpecial int }{
		{0, PageSize},
		{8, PageSize - 8},
		{1, PageSize - 8},
		{9, PageSize - 16},
		{16, PageSize - 16},
	} {
		p := New(tc.size)
		if int(p.Special()) != tc.wantSpecial {
			t.Errorf("New(%d).Special() = %d, want %d", tc.size, p.Special(), tc.wantSpecial)
		}
		if p.Upper() != p.Special() {
			t.Errorf("New(%d): upper %d != special %d", tc.size, p.Upper(), p.Special())
		}
		if got := len(p.SpecialSpace()); got != PageSize-tc.wantSpecial {
			t.Errorf("New(%d): len(SpecialSpace()) = %d", tc.size, got)
		}
	}
	p := New(16)
	copy(p.SpecialSpace(), []byte("sibling pointers"))
	if !bytes.Equal(p[PageSize-16:], []byte("sibling pointers")) {
		t.Fatal("SpecialSpace does not alias the page")
	}
}

func TestLSN(t *testing.T) {
	p := New(0)
	p.SetLSN(0x0102030405060708)
	if p.LSN() != 0x0102030405060708 {
		t.Fatalf("LSN = %#x", p.LSN())
	}
	if !bytes.Equal(p[:8], []byte{8, 7, 6, 5, 4, 3, 2, 1}) {
		t.Fatalf("LSN bytes % x", p[:8])
	}
}

func TestAddItemFirstItemBytes(t *testing.T) {
	p := New(0)
	n, err := p.AddItem([]byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("item number = %d, want 1", n)
	}
	// off=8184 (0x1FF8), flags=1 (<<15), len=3 (<<17) => 0x00069FF8.
	if got := p[HeaderSize : HeaderSize+LinePointerSize]; !bytes.Equal(got, []byte{0xF8, 0x9F, 0x06, 0x00}) {
		t.Fatalf("line pointer bytes % x", got)
	}
	if got := p.ItemID(1); got != (ItemID{Off: 8184, Len: 3, Flags: ItemNormal}) {
		t.Fatalf("ItemID(1) = %+v", got)
	}
	if p.Lower() != 28 || p.Upper() != 8184 {
		t.Fatalf("lower/upper = %d/%d, want 28/8184", p.Lower(), p.Upper())
	}
	if !bytes.Equal(p[8184:8187], []byte{1, 2, 3}) {
		t.Fatalf("item data at 8184: % x", p[8184:8192])
	}
}

func TestAddGetRoundTrip(t *testing.T) {
	p := New(0)
	items := [][]byte{[]byte("a"), []byte("hello"), {}, bytes.Repeat([]byte{9}, 100)}
	for i, it := range items {
		n, err := p.AddItem(it)
		if err != nil {
			t.Fatalf("AddItem #%d: %v", i, err)
		}
		if int(n) != i+1 {
			t.Fatalf("AddItem #%d returned %d", i, n)
		}
	}
	if p.NumItems() != OffsetNumber(len(items)) {
		t.Fatalf("NumItems = %d", p.NumItems())
	}
	for i, it := range items {
		got, err := p.GetItem(OffsetNumber(i + 1))
		if err != nil {
			t.Fatalf("GetItem(%d): %v", i+1, err)
		}
		if !bytes.Equal(got, it) {
			t.Fatalf("GetItem(%d) = %q, want %q", i+1, got, it)
		}
	}
}

func TestGetItemAliasesPage(t *testing.T) {
	p := New(0)
	n, _ := p.AddItem([]byte("abc"))
	got, _ := p.GetItem(n)
	got[0] = 'z'
	again, _ := p.GetItem(n)
	if string(again) != "zbc" {
		t.Fatalf("GetItem returned a copy: %q", again)
	}
}

func TestGetItemErrors(t *testing.T) {
	p := New(0)
	p.AddItem([]byte("x"))
	for _, n := range []OffsetNumber{0, 2, 1000} {
		if _, err := p.GetItem(n); !errors.Is(err, ErrInvalidOffset) {
			t.Errorf("GetItem(%d) err = %v, want ErrInvalidOffset", n, err)
		}
	}
	if err := p.DeleteItem(1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetItem(1); !errors.Is(err, ErrItemUnused) {
		t.Errorf("GetItem(deleted) err = %v, want ErrItemUnused", err)
	}
	if err := p.DeleteItem(1); !errors.Is(err, ErrItemUnused) {
		t.Errorf("DeleteItem(deleted) err = %v, want ErrItemUnused", err)
	}
	if err := p.DeleteItem(5); !errors.Is(err, ErrInvalidOffset) {
		t.Errorf("DeleteItem(5) err = %v, want ErrInvalidOffset", err)
	}
}

func TestFreeSpaceIsExact(t *testing.T) {
	p := New(0)
	if got := p.FreeSpace(); got != PageSize-HeaderSize-LinePointerSize {
		t.Fatalf("empty FreeSpace = %d", got)
	}
	p.AddItem(make([]byte, 10)) // occupies 16 bytes + 4 for the pointer
	if got := p.FreeSpace(); got != PageSize-HeaderSize-4-16-4 {
		t.Fatalf("FreeSpace after 10-byte item = %d", got)
	}
	p.AddItem(make([]byte, 16)) // exactly aligned
	if got := p.FreeSpace(); got != PageSize-HeaderSize-4-16-4-16-4 {
		t.Fatalf("FreeSpace after 16-byte item = %d", got)
	}
	// Special space is not free space.
	q := New(64)
	if got := q.FreeSpace(); got != PageSize-64-HeaderSize-LinePointerSize {
		t.Fatalf("FreeSpace with special = %d", got)
	}
}

func TestFreeSpaceNeverNegative(t *testing.T) {
	p := New(0)
	for {
		if _, err := p.AddItem(make([]byte, 8)); err != nil {
			break
		}
	}
	if p.FreeSpace() < 0 {
		t.Fatalf("FreeSpace = %d", p.FreeSpace())
	}
	if p.Upper()-p.Lower() >= 12 {
		t.Fatalf("page not full: lower=%d upper=%d", p.Lower(), p.Upper())
	}
}

func TestAddItemNoSpace(t *testing.T) {
	p := New(0)
	if _, err := p.AddItem(make([]byte, MaxItemSize)); err != nil {
		t.Fatalf("MaxItemSize item should fit on an empty page: %v", err)
	}
	if p.FreeSpace() != 0 {
		t.Fatalf("FreeSpace after max item = %d", p.FreeSpace())
	}
	if _, err := p.AddItem([]byte{1}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	// The remaining 4 bytes hold one more line pointer, so an empty item
	// still fits, and then nothing does.
	if _, err := p.AddItem(nil); err != nil {
		t.Fatalf("empty item into 4 free bytes: %v", err)
	}
	if _, err := p.AddItem(nil); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("empty item err = %v, want ErrNoSpace", err)
	}
}

func TestAddItemTooLarge(t *testing.T) {
	p := New(0)
	if _, err := p.AddItem(make([]byte, MaxItemSize+1)); !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("err = %v, want ErrItemTooLarge", err)
	}
	if p.NumItems() != 0 || p.Upper() != PageSize {
		t.Fatal("failed AddItem modified the page")
	}
}

func TestAddItemFillsPageExactly(t *testing.T) {
	p := New(0)
	// Each 4-byte item costs 8 (aligned) + 4 (pointer) = 12 bytes.
	want := (PageSize - HeaderSize) / 12
	n := 0
	for {
		if _, err := p.AddItem([]byte{1, 2, 3, 4}); err != nil {
			if !errors.Is(err, ErrNoSpace) {
				t.Fatal(err)
			}
			break
		}
		n++
	}
	if n != want {
		t.Fatalf("fit %d items, want %d", n, want)
	}
}

func TestDeleteKeepsItemNumbers(t *testing.T) {
	p := New(0)
	for i := 0; i < 5; i++ {
		p.AddItem([]byte{byte('a' + i)})
	}
	if err := p.DeleteItem(3); err != nil {
		t.Fatal(err)
	}
	if p.NumItems() != 5 {
		t.Fatalf("NumItems after delete = %d, want 5", p.NumItems())
	}
	if id := p.ItemID(3); id != (ItemID{}) {
		t.Fatalf("deleted ItemID = %+v, want zero", id)
	}
	for _, n := range []OffsetNumber{1, 2, 4, 5} {
		got, err := p.GetItem(n)
		if err != nil || len(got) != 1 || got[0] != byte('a'+n-1) {
			t.Fatalf("GetItem(%d) = %q, %v", n, got, err)
		}
	}
	// Delete does not reclaim data space by itself.
	if p.Upper() != PageSize-5*8 {
		t.Fatalf("Upper after delete = %d", p.Upper())
	}
}

func TestAddItemReusesUnusedLinePointer(t *testing.T) {
	p := New(0)
	for i := 0; i < 4; i++ {
		p.AddItem([]byte{byte(i)})
	}
	p.DeleteItem(2)
	p.DeleteItem(4)
	n, err := p.AddItem([]byte{42})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reused item number = %d, want 2", n)
	}
	if p.NumItems() != 4 {
		t.Fatalf("NumItems = %d, want 4", p.NumItems())
	}
	got, _ := p.GetItem(2)
	if !bytes.Equal(got, []byte{42}) {
		t.Fatalf("GetItem(2) = % x", got)
	}
}

func TestCompact(t *testing.T) {
	p := New(0)
	for i := 0; i < 6; i++ {
		p.AddItem(bytes.Repeat([]byte{byte('a' + i)}, 10+i))
	}
	p.DeleteItem(2)
	p.DeleteItem(5)
	p.DeleteItem(6)
	upperBefore := p.Upper()
	p.Compact()
	if p.Upper() <= upperBefore {
		t.Fatalf("Compact did not reclaim space: %d -> %d", upperBefore, p.Upper())
	}
	// Items 1, 3, 4 alive: aligned sizes 16, 16, 16.
	if want := uint16(PageSize - 48); p.Upper() != want {
		t.Fatalf("Upper after compact = %d, want %d", p.Upper(), want)
	}
	// Trailing unused pointers 5 and 6 are dropped; the hole at 2 stays.
	if p.NumItems() != 4 {
		t.Fatalf("NumItems after compact = %d, want 4", p.NumItems())
	}
	if p.Lower() != HeaderSize+4*LinePointerSize {
		t.Fatalf("Lower after compact = %d", p.Lower())
	}
	for _, n := range []OffsetNumber{1, 3, 4} {
		got, err := p.GetItem(n)
		if err != nil {
			t.Fatalf("GetItem(%d): %v", n, err)
		}
		want := bytes.Repeat([]byte{byte('a' + n - 1)}, 10+int(n)-1)
		if !bytes.Equal(got, want) {
			t.Fatalf("GetItem(%d) = %q, want %q", n, got, want)
		}
	}
	if _, err := p.GetItem(2); !errors.Is(err, ErrItemUnused) {
		t.Fatalf("GetItem(2) err = %v", err)
	}
	// Free space is contiguous again: a big item fits.
	if _, err := p.AddItem(make([]byte, p.FreeSpace())); err != nil {
		t.Fatalf("AddItem(FreeSpace()) after compact: %v", err)
	}
}

func TestCompactEmptyAndAllDeleted(t *testing.T) {
	p := New(8)
	p.Compact()
	if p.Lower() != HeaderSize || p.Upper() != PageSize-8 {
		t.Fatalf("Compact on empty page changed it: %d/%d", p.Lower(), p.Upper())
	}
	p.AddItem([]byte("x"))
	p.AddItem([]byte("y"))
	p.DeleteItem(1)
	p.DeleteItem(2)
	p.Compact()
	if p.NumItems() != 0 || p.Lower() != HeaderSize || p.Upper() != PageSize-8 {
		t.Fatalf("Compact with all deleted: items=%d lower=%d upper=%d", p.NumItems(), p.Lower(), p.Upper())
	}
}

func TestSerializationRoundTrip(t *testing.T) {
	p := New(16)
	p.SetLSN(77)
	copy(p.SpecialSpace(), []byte("special!"))
	p.AddItem([]byte("one"))
	p.AddItem([]byte("two"))
	p.DeleteItem(1)

	raw := make([]byte, PageSize)
	copy(raw, p)
	q := Page(raw)
	if q.LSN() != 77 || q.NumItems() != 2 || q.Special() != PageSize-16 {
		t.Fatalf("header not preserved: lsn=%d items=%d special=%d", q.LSN(), q.NumItems(), q.Special())
	}
	if got, _ := q.GetItem(2); string(got) != "two" {
		t.Fatalf("GetItem(2) = %q", got)
	}
	if _, err := q.GetItem(1); !errors.Is(err, ErrItemUnused) {
		t.Fatalf("GetItem(1) err = %v", err)
	}
	if string(q.SpecialSpace()[:8]) != "special!" {
		t.Fatal("special space not preserved")
	}
}

func TestItemIDPanicsOutOfRange(t *testing.T) {
	p := New(0)
	defer func() {
		if recover() == nil {
			t.Fatal("ItemID(1) on empty page did not panic")
		}
	}()
	p.ItemID(1)
}

// TestRandomOperations runs random add/delete/compact sequences and checks
// the page against a map after every step.
func TestRandomOperations(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		special := []int{0, 8, 32}[rng.Intn(3)]
		p := New(special)
		model := map[OffsetNumber][]byte{}

		check := func(step int) {
			t.Helper()
			if !(HeaderSize <= p.Lower() && p.Lower() <= p.Upper() && p.Upper() <= p.Special() && p.Special() <= PageSize) {
				t.Fatalf("seed %d step %d: invariant broken: lower=%d upper=%d special=%d", seed, step, p.Lower(), p.Upper(), p.Special())
			}
			if p.Lower() != HeaderSize+uint16(p.NumItems())*LinePointerSize {
				t.Fatalf("seed %d step %d: lower %d disagrees with NumItems %d", seed, step, p.Lower(), p.NumItems())
			}
			for n := OffsetNumber(1); n <= p.NumItems(); n++ {
				want, alive := model[n]
				got, err := p.GetItem(n)
				switch {
				case alive && err != nil:
					t.Fatalf("seed %d step %d: GetItem(%d): %v", seed, step, n, err)
				case alive && !bytes.Equal(got, want):
					t.Fatalf("seed %d step %d: GetItem(%d) = %q, want %q", seed, step, n, got, want)
				case !alive && !errors.Is(err, ErrItemUnused):
					t.Fatalf("seed %d step %d: GetItem(%d) err = %v, want ErrItemUnused", seed, step, n, err)
				}
			}
			for n := range model {
				if n > p.NumItems() {
					t.Fatalf("seed %d step %d: live item %d beyond NumItems %d", seed, step, n, p.NumItems())
				}
			}
		}

		for step := 0; step < 400; step++ {
			switch r := rng.Intn(10); {
			case r < 6:
				item := make([]byte, rng.Intn(200))
				rng.Read(item)
				free := p.FreeSpace()
				n, err := p.AddItem(item)
				if err != nil {
					if !errors.Is(err, ErrNoSpace) {
						t.Fatalf("seed %d step %d: %v", seed, step, err)
					}
					// It may only fail when it really does not fit.
					if (len(item)+7)&^7 <= free {
						t.Fatalf("seed %d step %d: ErrNoSpace with FreeSpace=%d for %d bytes", seed, step, free, len(item))
					}
					continue
				}
				if _, dup := model[n]; dup {
					t.Fatalf("seed %d step %d: AddItem returned live item number %d", seed, step, n)
				}
				model[n] = append([]byte(nil), item...)
			case r < 9:
				if p.NumItems() == 0 {
					continue
				}
				n := OffsetNumber(1 + rng.Intn(int(p.NumItems())))
				_, alive := model[n]
				err := p.DeleteItem(n)
				if alive && err != nil {
					t.Fatalf("seed %d step %d: DeleteItem(%d): %v", seed, step, n, err)
				}
				if !alive && !errors.Is(err, ErrItemUnused) {
					t.Fatalf("seed %d step %d: DeleteItem(%d) err = %v", seed, step, n, err)
				}
				delete(model, n)
			default:
				p.Compact()
				sum := 0
				for _, it := range model {
					sum += (len(it) + 7) &^ 7
				}
				if int(p.Special())-int(p.Upper()) != sum {
					t.Fatalf("seed %d step %d: after Compact upper=%d special=%d, live bytes %d", seed, step, p.Upper(), p.Special(), sum)
				}
			}
			check(step)
		}
	}
}

func TestInsertItem(t *testing.T) {
	p := New(0)
	for _, it := range []string{"a", "b", "c"} {
		if _, err := p.AddItem([]byte(it)); err != nil {
			t.Fatal(err)
		}
	}
	free := p.FreeSpace()
	if err := p.InsertItem(2, []byte("x")); err != nil {
		t.Fatalf("InsertItem(2): %v", err)
	}
	if err := p.InsertItem(5, []byte("y")); err != nil {
		t.Fatalf("InsertItem(5): %v", err)
	}
	if err := p.InsertItem(1, []byte("w")); err != nil {
		t.Fatalf("InsertItem(1): %v", err)
	}
	want := []string{"w", "a", "x", "b", "c", "y"}
	if p.NumItems() != OffsetNumber(len(want)) {
		t.Fatalf("NumItems = %d, want %d", p.NumItems(), len(want))
	}
	for i, w := range want {
		got, err := p.GetItem(OffsetNumber(i + 1))
		if err != nil || string(got) != w {
			t.Errorf("item %d = %q (%v), want %q", i+1, got, err, w)
		}
	}
	// Three items of 8 aligned bytes plus three line pointers.
	if got := p.FreeSpace(); got != free-3*(8+LinePointerSize) {
		t.Errorf("FreeSpace = %d, want %d", got, free-3*(8+LinePointerSize))
	}
	for _, n := range []OffsetNumber{0, 8} {
		if err := p.InsertItem(n, []byte("z")); !errors.Is(err, ErrInvalidOffset) {
			t.Errorf("InsertItem(%d) = %v, want ErrInvalidOffset", n, err)
		}
	}
	if err := p.InsertItem(1, make([]byte, MaxItemSize+1)); !errors.Is(err, ErrItemTooLarge) {
		t.Errorf("oversized item: %v, want ErrItemTooLarge", err)
	}
	if err := p.InsertItem(1, make([]byte, p.FreeSpace()+1)); !errors.Is(err, ErrNoSpace) {
		t.Errorf("item past free space: %v, want ErrNoSpace", err)
	}
	if err := p.InsertItem(1, make([]byte, p.FreeSpace()&^7)); err != nil {
		t.Errorf("item filling free space: %v", err)
	}
	// Unused line pointers are not reused: the new item is renumbered in.
	p = New(0)
	p.AddItem([]byte("a"))
	p.AddItem([]byte("b"))
	p.DeleteItem(1)
	if err := p.InsertItem(1, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.GetItem(1); string(got) != "c" || p.NumItems() != 3 || p.ItemID(2).Flags != ItemUnused {
		t.Errorf("after insert over an unused pointer: item 1 %q, %d items, item 2 flags %v", got, p.NumItems(), p.ItemID(2).Flags)
	}
}
