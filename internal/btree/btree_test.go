package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

const relOID tuple.OID = 16385

func newPool(t *testing.T, dir string, nframes int) *bufmgr.Pool {
	t.Helper()
	store, err := smgr.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return bufmgr.New(store, nframes)
}

func create(t *testing.T, typ tuple.TypeID) (*Tree, *bufmgr.Pool, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir, 16)
	tree, err := Create(pool, relOID, typ)
	if err != nil {
		t.Fatal(err)
	}
	return tree, pool, dir
}

func tid(block, off int) tuple.TID {
	return tuple.TID{Block: tuple.BlockNumber(block), Off: page.OffsetNumber(off)}
}

func insert(t *testing.T, tree *Tree, key tuple.Datum, id tuple.TID) {
	t.Helper()
	if err := tree.Insert(key, id); err != nil {
		t.Fatalf("Insert(%v, %v): %v", key, id, err)
	}
}

// entry is what a scan yields, for comparing against a reference.
type entry struct {
	key tuple.Datum
	tid tuple.TID
}

func collect(t *testing.T, s *Scan) []entry {
	t.Helper()
	var out []entry
	for s.Next() {
		out = append(out, entry{s.Key(), s.TID()})
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	s.Close()
	return out
}

func check(t *testing.T, tree *Tree) {
	t.Helper()
	if err := tree.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func meta(t *testing.T, tree *Tree) Meta {
	t.Helper()
	m, err := tree.Meta()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func nblocks(t *testing.T, pool *bufmgr.Pool) int {
	t.Helper()
	n, err := pool.Store().NBlocks(relOID)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestIndexTupleBytes(t *testing.T) {
	cases := []struct {
		name string
		typ  tuple.TypeID
		key  tuple.Datum
		tid  tuple.TID
		want []byte
	}{
		{"int4", tuple.Int4, int32(7), tid(3, 5), []byte{3, 0, 0, 0, 5, 0, 12, 0, 7, 0, 0, 0}},
		{"int4 negative", tuple.Int4, int32(-1), tid(0, 1), []byte{0, 0, 0, 0, 1, 0, 12, 0, 0xff, 0xff, 0xff, 0xff}},
		{"int8", tuple.Int8, int64(1) << 40, tid(1, 2), []byte{1, 0, 0, 0, 2, 0, 16, 0, 0, 0, 0, 0, 0, 1, 0, 0}},
		{"bool", tuple.Bool, true, tid(1, 1), []byte{1, 0, 0, 0, 1, 0, 9, 0, 1}},
		{"text", tuple.Text, "ab", tid(2, 9), []byte{2, 0, 0, 0, 9, 0, 14, 0, 2, 0, 0, 0, 'a', 'b'}},
		{"empty text", tuple.Text, "", tid(2, 9), []byte{2, 0, 0, 0, 9, 0, 12, 0, 0, 0, 0, 0}},
		{"null", tuple.Int4, nil, tid(4, 4), []byte{4, 0, 0, 0, 4, 0, 8, 0x80}},
	}
	for _, c := range cases {
		it, err := FormIndexTuple(c.typ, c.key, c.tid)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !bytes.Equal(it, c.want) {
			t.Errorf("%s: bytes % x, want % x", c.name, []byte(it), c.want)
		}
		if it.TID() != c.tid || it.HeapTID() != c.tid || it.Size() != len(c.want) || it.IsPivot() {
			t.Errorf("%s: TID %v, HeapTID %v, Size %d, pivot %v", c.name, it.TID(), it.HeapTID(), it.Size(), it.IsPivot())
		}
		if it.IsNull() != (c.key == nil) || !reflect.DeepEqual(it.Key(c.typ), c.key) {
			t.Errorf("%s: IsNull %v, Key %v", c.name, it.IsNull(), it.Key(c.typ))
		}
	}
}

func TestIndexTupleErrors(t *testing.T) {
	bad := []struct {
		typ tuple.TypeID
		key tuple.Datum
	}{
		{tuple.Int4, int64(1)}, {tuple.Int4, "1"}, {tuple.Int8, int32(1)},
		{tuple.Bool, int32(1)}, {tuple.Text, 1}, {tuple.Text, []byte("x")},
	}
	for _, c := range bad {
		if _, err := FormIndexTuple(c.typ, c.key, tid(1, 1)); !errors.Is(err, ErrKeyType) {
			t.Errorf("FormIndexTuple(%v, %T) = %v, want ErrKeyType", c.typ, c.key, err)
		}
	}
	// The largest text key is MaxItemSize minus the header and length.
	if MaxItemSize != 2704 {
		t.Errorf("MaxItemSize = %d, want 2704", MaxItemSize)
	}
	fits := strings.Repeat("x", MaxItemSize-HeaderSize-4)
	if it, err := FormIndexTuple(tuple.Text, fits, tid(1, 1)); err != nil || len(it) != MaxItemSize {
		t.Errorf("largest key: %v, size %d", err, len(it))
	}
	if _, err := FormIndexTuple(tuple.Text, fits+"x", tid(1, 1)); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("oversized key: %v, want ErrKeyTooLarge", err)
	}
}

func TestOpaqueBytes(t *testing.T) {
	p := page.New(SpecialSize)
	o := Opaque{Prev: 3, Next: 9, Level: 2, Flags: FlagRoot}
	WriteOpaque(p, o)
	want := []byte{3, 0, 0, 0, 9, 0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0}
	if got := p.SpecialSpace(); !bytes.Equal(got, want) {
		t.Errorf("special space % x, want % x", got, want)
	}
	if got := ReadOpaque(p); got != o {
		t.Errorf("ReadOpaque = %+v, want %+v", got, o)
	}
	if p.Special() != page.PageSize-SpecialSize {
		t.Errorf("special offset %d", p.Special())
	}
}

func TestCreateMetapage(t *testing.T) {
	tree, pool, _ := create(t, tuple.Int4)
	if n := nblocks(t, pool); n != 1 {
		t.Fatalf("new tree has %d blocks, want 1", n)
	}
	if m := meta(t, tree); m != (Meta{Root: None, Level: 0}) {
		t.Errorf("Meta = %+v", m)
	}
	buf, err := pool.Pin(relOID, MetaBlock)
	if err != nil {
		t.Fatal(err)
	}
	p := buf.Page()
	item, err := p.GetItem(1)
	want := []byte{0x62, 0x31, 0x05, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err != nil || !bytes.Equal(item, want) {
		t.Errorf("metapage item % x (%v), want % x", item, err, want)
	}
	if o := ReadOpaque(p); o != (Opaque{Flags: FlagMeta}) {
		t.Errorf("metapage opaque %+v", o)
	}
	pool.Unpin(buf)
	check(t, tree)

	if _, err := Create(pool, relOID, tuple.Int4); !errors.Is(err, smgr.ErrExists) {
		t.Errorf("Create existing: %v, want smgr.ErrExists", err)
	}
	// Scans and searches of an empty tree find nothing.
	if got := collect(t, tree.Scan(nil, nil)); len(got) != 0 {
		t.Errorf("empty scan returned %v", got)
	}
	if blk, off, err := tree.Search(int32(1)); blk != None || off != page.InvalidOffsetNumber || err != nil {
		t.Errorf("Search on empty tree = %d, %d, %v", blk, off, err)
	}
}

func TestOpenNotATree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir, 4)
	if err := pool.Store().Create(relOID); err != nil {
		t.Fatal(err)
	}
	buf, err := pool.Extend(relOID)
	if err != nil {
		t.Fatal(err)
	}
	buf.Page().Init(0) // a heap page
	buf.MarkDirty()
	pool.Unpin(buf)
	tree := Open(pool, relOID, tuple.Int4)
	if _, err := tree.Meta(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Meta on a heap file: %v, want ErrCorrupt", err)
	}
	if err := tree.Insert(int32(1), tid(1, 1)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Insert on a heap file: %v, want ErrCorrupt", err)
	}
	if err := tree.Check(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Check on a heap file: %v, want ErrCorrupt", err)
	}
}

func TestInsertAndScan(t *testing.T) {
	tree, _, _ := create(t, tuple.Int4)
	r := rand.New(rand.NewSource(1))
	keys := r.Perm(100)
	for _, k := range keys {
		insert(t, tree, int32(k), tid(k/10, k%10+1))
	}
	if m := meta(t, tree); m != (Meta{Root: 1, Level: 0}) {
		t.Errorf("Meta = %+v, want a single leaf root at block 1", m)
	}
	check(t, tree)

	var all []entry
	for k := 0; k < 100; k++ {
		all = append(all, entry{int32(k), tid(k/10, k%10+1)})
	}
	b := func(k int, incl bool) *Bound { return &Bound{Key: int32(k), Inclusive: incl} }
	cases := []struct {
		name   string
		lo, hi *Bound
		want   []entry
	}{
		{"all", nil, nil, all},
		{"ge 90", b(90, true), nil, all[90:]},
		{"gt 90", b(90, false), nil, all[91:]},
		{"le 9", nil, b(9, true), all[:10]},
		{"lt 9", nil, b(9, false), all[:9]},
		{"between inclusive", b(10, true), b(20, true), all[10:21]},
		{"between exclusive", b(10, false), b(20, false), all[11:20]},
		{"eq", b(42, true), b(42, true), all[42:43]},
		{"empty range", b(42, false), b(42, false), nil},
		{"above all", b(100, true), nil, nil},
		{"below all", nil, b(0, false), nil},
		{"inverted", b(50, true), b(40, true), nil},
	}
	for _, c := range cases {
		got := collect(t, tree.Scan(c.lo, c.hi))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %d entries %v, want %d %v", c.name, len(got), got, len(c.want), c.want)
		}
	}
}

func TestDuplicatesOrderByTID(t *testing.T) {
	tree, _, _ := create(t, tuple.Text)
	tids := []tuple.TID{tid(2, 1), tid(0, 5), tid(1, 1), tid(0, 1), tid(1, 3), tid(0, 4)}
	for _, id := range tids {
		insert(t, tree, "dup", id)
	}
	insert(t, tree, "a", tid(9, 9))
	insert(t, tree, "z", tid(0, 0))
	got := collect(t, tree.Scan(&Bound{"dup", true}, &Bound{"dup", true}))
	want := []entry{{"dup", tid(0, 1)}, {"dup", tid(0, 4)}, {"dup", tid(0, 5)}, {"dup", tid(1, 1)}, {"dup", tid(1, 3)}, {"dup", tid(2, 1)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("duplicates: %v, want %v", got, want)
	}
	check(t, tree)
}

func TestNullsSortLast(t *testing.T) {
	tree, _, _ := create(t, tuple.Int8)
	insert(t, tree, nil, tid(1, 2))
	insert(t, tree, int64(5), tid(1, 1))
	insert(t, tree, nil, tid(0, 3))
	insert(t, tree, int64(-5), tid(2, 1))
	insert(t, tree, int64(1)<<40, tid(3, 1))
	check(t, tree)

	got := collect(t, tree.Scan(nil, nil))
	want := []entry{{int64(-5), tid(2, 1)}, {int64(5), tid(1, 1)}, {int64(1) << 40, tid(3, 1)}, {nil, tid(0, 3)}, {nil, tid(1, 2)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("all: %v, want %v", got, want)
	}
	// A lower bound alone still runs into the NULLs at the end.
	got = collect(t, tree.Scan(&Bound{int64(5), false}, nil))
	if !reflect.DeepEqual(got, want[2:]) {
		t.Errorf("gt 5: %v, want %v", got, want[2:])
	}
	// An upper bound excludes them.
	got = collect(t, tree.Scan(&Bound{int64(5), false}, &Bound{int64(1) << 62, true}))
	if !reflect.DeepEqual(got, want[2:3]) {
		t.Errorf("gt 5 with upper bound: %v, want %v", got, want[2:3])
	}
	s := tree.Scan(nil, nil)
	nulls := 0
	for s.Next() {
		if s.IsNull() != (s.Key() == nil) {
			t.Errorf("IsNull %v but Key %v", s.IsNull(), s.Key())
		}
		if s.IsNull() {
			nulls++
		}
	}
	if nulls != 2 {
		t.Errorf("%d NULL entries, want 2", nulls)
	}
	// NULL searches position at the first NULL.
	blk, off, err := tree.Search(nil)
	if err != nil || blk != 1 || off != 4 {
		t.Errorf("Search(NULL) = %d, %d, %v; want 1, 4", blk, off, err)
	}
}

func TestSearch(t *testing.T) {
	tree, _, _ := create(t, tuple.Int4)
	for _, k := range []int{10, 20, 20, 30} {
		insert(t, tree, int32(k), tid(k, 1))
	}
	cases := []struct {
		key int32
		off page.OffsetNumber
	}{
		{5, 1}, {10, 1}, {15, 2}, {20, 2}, {25, 4}, {30, 4}, {35, 5},
	}
	for _, c := range cases {
		blk, off, err := tree.Search(c.key)
		if err != nil || blk != 1 || off != c.off {
			t.Errorf("Search(%d) = %d, %d, %v; want 1, %d", c.key, blk, off, err, c.off)
		}
	}
	if _, _, err := tree.Search("x"); !errors.Is(err, ErrKeyType) {
		t.Errorf("Search with wrong type: %v", err)
	}
}

func TestErrorsLeaveTreeUnchanged(t *testing.T) {
	tree, pool, _ := create(t, tuple.Text)
	insert(t, tree, "a", tid(1, 1))
	if err := tree.Insert(int32(1), tid(1, 2)); !errors.Is(err, ErrKeyType) {
		t.Errorf("wrong type: %v", err)
	}
	if err := tree.Insert(strings.Repeat("x", MaxItemSize), tid(1, 3)); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("oversized: %v", err)
	}
	if got := collect(t, tree.Scan(nil, nil)); len(got) != 1 {
		t.Errorf("tree changed: %v", got)
	}
	if n := nblocks(t, pool); n != 2 {
		t.Errorf("%d blocks, want 2", n)
	}
	check(t, tree)
	s := tree.Scan(&Bound{Key: int32(1)}, nil)
	if s.Next() || !errors.Is(s.Err(), ErrKeyType) {
		t.Errorf("scan with wrong bound type: %v", s.Err())
	}
	s = tree.Scan(nil, &Bound{Key: nil})
	if s.Next() || !errors.Is(s.Err(), ErrKeyType) {
		t.Errorf("scan with NULL bound: %v", s.Err())
	}
}

// bigKey makes a text key of about n bytes that sorts by i.
func bigKey(i, n int) string {
	return fmt.Sprintf("%08d", i) + strings.Repeat("x", n-8)
}

func TestSplitsGrowTree(t *testing.T) {
	tree, pool, _ := create(t, tuple.Text)
	r := rand.New(rand.NewSource(2))
	const n = 400
	perm := r.Perm(n)
	roots := map[tuple.BlockNumber]bool{}
	levels := map[uint32]bool{}
	for i, k := range perm {
		// About 1000 bytes per key: seven per leaf.
		insert(t, tree, bigKey(k, 1000), tid(k, 1))
		m := meta(t, tree)
		roots[m.Root] = true
		levels[m.Level] = true
		if i%25 == 0 || i == n-1 {
			check(t, tree)
		}
	}
	// Random order leaves pages about two thirds full, and downlinks are
	// as large as the keys, so seven per page at every level: four levels.
	m := meta(t, tree)
	if m.Level != 3 {
		t.Errorf("final level %d, want 3 (four levels)", m.Level)
	}
	if !levels[0] || !levels[1] || !levels[2] || !levels[3] || len(roots) != 4 {
		t.Errorf("levels seen %v, roots seen %v", levels, roots)
	}
	if nb := nblocks(t, pool); nb < n/7 || nb > n/2 {
		t.Errorf("%d blocks for %d keys", nb, n)
	}
	got := collect(t, tree.Scan(nil, nil))
	if len(got) != n {
		t.Fatalf("scan returned %d entries, want %d", len(got), n)
	}
	for i, e := range got {
		if e.key != bigKey(i, 1000) || e.tid != tid(i, 1) {
			t.Fatalf("entry %d = %v", i, e.tid)
		}
	}
	// A range in the middle crosses page boundaries.
	got = collect(t, tree.Scan(&Bound{bigKey(100, 1000), true}, &Bound{bigKey(150, 1000), false}))
	if len(got) != 50 || got[0].tid != tid(100, 1) || got[49].tid != tid(149, 1) {
		t.Errorf("range scan: %d entries, first %v, last %v", len(got), got[0].tid, got[len(got)-1].tid)
	}
}

func TestAscendingInsertsPackPages(t *testing.T) {
	tree, pool, _ := create(t, tuple.Int4)
	const n = 20000
	for i := 0; i < n; i++ {
		insert(t, tree, int32(i), tid(i/200, i%200+1))
	}
	check(t, tree)
	// A 12-byte tuple takes 20 bytes with alignment and its line pointer,
	// so a leaf holds 407; at 90 percent fill 366, which is 55 leaves plus
	// the root and the metapage. Balanced splits would need about 100.
	if nb := nblocks(t, pool); nb != 57 {
		t.Errorf("%d blocks for %d ascending keys, want 57", nb, n)
	}
	got := collect(t, tree.Scan(&Bound{int32(n - 3), true}, nil))
	if len(got) != 3 || got[0].key != int32(n-3) {
		t.Errorf("tail scan: %v", got)
	}
}

func TestPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir, 8)
	tree, err := Create(pool, relOID, tuple.Int8)
	if err != nil {
		t.Fatal(err)
	}
	const n = 3000
	for i := 0; i < n; i++ {
		insert(t, tree, int64(i*7%n), tid(i, 1))
	}
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	pool = newPool(t, dir, 8)
	tree = Open(pool, relOID, tuple.Int8)
	check(t, tree)
	got := collect(t, tree.Scan(nil, nil))
	if len(got) != n {
		t.Fatalf("%d entries after reopen, want %d", len(got), n)
	}
	for i, e := range got {
		if e.key != int64(i) {
			t.Fatalf("entry %d has key %v", i, e.key)
		}
	}
	if m := meta(t, tree); m.Level < 1 {
		t.Errorf("Meta = %+v, want a tree of at least two levels", m)
	}
}

func TestRandomRangeScans(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, tc := range []struct {
		name string
		typ  tuple.TypeID
		n    int
		gen  func() tuple.Datum
		less func(a, b tuple.Datum) bool
	}{
		{"int8 with duplicates", tuple.Int8, 5000,
			func() tuple.Datum { return int64(r.Intn(1000)) },
			func(a, b tuple.Datum) bool { return a.(int64) < b.(int64) }},
		{"text of mixed length", tuple.Text, 2000,
			func() tuple.Datum { return strings.Repeat(string(rune('a'+r.Intn(26))), 1+r.Intn(300)) },
			func(a, b tuple.Datum) bool { return a.(string) < b.(string) }},
		{"bool", tuple.Bool, 3000,
			func() tuple.Datum { return r.Intn(2) == 1 },
			func(a, b tuple.Datum) bool { return !a.(bool) && b.(bool) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree, _, _ := create(t, tc.typ)
			var ref []entry
			for i := 0; i < tc.n; i++ {
				var key tuple.Datum
				if r.Intn(20) != 0 {
					key = tc.gen()
				}
				id := tid(i/100, i%100+1)
				insert(t, tree, key, id)
				ref = append(ref, entry{key, id})
			}
			check(t, tree)
			lessEntry := func(a, b entry) bool {
				switch {
				case a.key == nil && b.key == nil:
				case a.key == nil:
					return false
				case b.key == nil:
					return true
				case tc.less(a.key, b.key):
					return true
				case tc.less(b.key, a.key):
					return false
				}
				if a.tid.Block != b.tid.Block {
					return a.tid.Block < b.tid.Block
				}
				return a.tid.Off < b.tid.Off
			}
			sort.SliceStable(ref, func(i, j int) bool { return lessEntry(ref[i], ref[j]) })
			if got := collect(t, tree.Scan(nil, nil)); !reflect.DeepEqual(got, ref) {
				t.Fatalf("full scan differs: %d entries, want %d", len(got), len(ref))
			}
			for i := 0; i < 200; i++ {
				var lo, hi *Bound
				if r.Intn(4) != 0 {
					lo = &Bound{tc.gen(), r.Intn(2) == 0}
				}
				if r.Intn(4) != 0 {
					hi = &Bound{tc.gen(), r.Intn(2) == 0}
				}
				var want []entry
				for _, e := range ref {
					// NULLs sort last: above any lower bound, beyond any upper.
					if e.key == nil {
						if hi == nil {
							want = append(want, e)
						}
						continue
					}
					if lo != nil && (tc.less(e.key, lo.Key) || (!lo.Inclusive && !tc.less(lo.Key, e.key))) {
						continue
					}
					if hi != nil && (tc.less(hi.Key, e.key) || (!hi.Inclusive && !tc.less(e.key, hi.Key))) {
						continue
					}
					want = append(want, e)
				}
				got := collect(t, tree.Scan(lo, hi))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("scan [%v, %v]: %d entries, want %d", lo, hi, len(got), len(want))
				}
			}
		})
	}
}

// corrupt applies fn to block blk through the pool.
func corrupt(t *testing.T, pool *bufmgr.Pool, blk tuple.BlockNumber, fn func(page.Page)) {
	t.Helper()
	buf, err := pool.Pin(relOID, blk)
	if err != nil {
		t.Fatal(err)
	}
	buf.Lock()
	fn(buf.Page())
	buf.MarkDirty()
	buf.Unlock()
	pool.Unpin(buf)
}

func TestCheckDetectsCorruption(t *testing.T) {
	build := func(t *testing.T) (*Tree, *bufmgr.Pool, tuple.BlockNumber) {
		tree, pool, _ := create(t, tuple.Int4)
		for i := 0; i < 3000; i++ {
			insert(t, tree, int32(i), tid(i, 1))
		}
		check(t, tree)
		leaf, _, err := tree.Search(int32(0))
		if err != nil {
			t.Fatal(err)
		}
		return tree, pool, leaf
	}
	cases := []struct {
		name string
		fn   func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber)
	}{
		{"items out of order", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				a, _ := p.GetItem(2)
				b, _ := p.GetItem(3)
				tmp := append([]byte(nil), a...)
				copy(a, b)
				copy(b, tmp)
			})
		}},
		{"broken sibling link", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				o := ReadOpaque(p)
				o.Next = None
				WriteOpaque(p, o)
			})
		}},
		{"wrong prev link", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, ReadOpaqueBlock(t, pool, leaf).Next, func(p page.Page) {
				o := ReadOpaque(p)
				o.Prev = leaf + 1000
				WriteOpaque(p, o)
			})
		}},
		{"high key changed", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				hk, _ := p.GetItem(1)
				hk[HeaderSize]++
			})
		}},
		{"item above high key", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				last, _ := p.GetItem(p.NumItems())
				last[HeaderSize+3] = 0x7f
			})
		}},
		{"wrong level", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				o := ReadOpaque(p)
				o.Level = 1
				WriteOpaque(p, o)
			})
		}},
		{"root flag on a leaf", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, leaf, func(p page.Page) {
				o := ReadOpaque(p)
				o.Flags |= FlagRoot
				WriteOpaque(p, o)
			})
		}},
		{"metapage level", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, MetaBlock, func(p page.Page) {
				m, _ := p.GetItem(1)
				m[12]++
			})
		}},
		{"metapage magic", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			corrupt(t, pool, MetaBlock, func(p page.Page) {
				m, _ := p.GetItem(1)
				m[0]++
			})
		}},
		{"unreachable page", func(t *testing.T, pool *bufmgr.Pool, tree *Tree, leaf tuple.BlockNumber) {
			buf, err := pool.Extend(relOID)
			if err != nil {
				t.Fatal(err)
			}
			buf.Page().Init(SpecialSize)
			buf.MarkDirty()
			pool.Unpin(buf)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree, pool, leaf := build(t)
			c.fn(t, pool, tree, leaf)
			err := tree.Check()
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("Check = %v, want ErrCorrupt", err)
			}
		})
	}
}

// ReadOpaqueBlock reads the special space of a block through the pool.
func ReadOpaqueBlock(t *testing.T, pool *bufmgr.Pool, blk tuple.BlockNumber) Opaque {
	t.Helper()
	buf, err := pool.Pin(relOID, blk)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Unpin(buf)
	return ReadOpaque(buf.Page())
}
