package index

import (
	"errors"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/btree"
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// db is a bootstrapped data directory with one table t (a int4, b text,
// c bool) and no indexes.
type db struct {
	pool *bufmgr.Pool
	cat  *catalog.Catalog
	t    *catalog.RelationInfo
}

var tDesc = tuple.NewDesc(
	tuple.Attr{Name: "a", Type: tuple.Int4},
	tuple.Attr{Name: "b", Type: tuple.Text},
	tuple.Attr{Name: "c", Type: tuple.Bool},
)

func newDB(t *testing.T) *db {
	t.Helper()
	store, err := smgr.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pool := bufmgr.New(store, 16)
	cat, err := catalog.Bootstrap(pool)
	if err != nil {
		t.Fatal(err)
	}
	d := &db{pool: pool, cat: cat}
	if _, err := cat.CreateTable("t", tDesc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	d.refresh(t)
	return d
}

// refresh reloads t after DDL.
func (d *db) refresh(t *testing.T) {
	t.Helper()
	info, err := d.cat.Lookup("t")
	if err != nil {
		t.Fatal(err)
	}
	d.t = info
}

// index creates an index on column attr of t and returns it.
func (d *db) index(t *testing.T, name string, attr int, unique bool) *catalog.IndexInfo {
	t.Helper()
	if _, err := d.cat.CreateIndex(name, d.t.OID, attr, unique, false, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	d.refresh(t)
	idx, err := d.cat.LookupIndex(name)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// seed inserts rows straight into the heap, bypassing the indexes. A nil
// value is NULL.
func (d *db) seed(t *testing.T, rows ...[]tuple.Datum) []tuple.TID {
	t.Helper()
	h := heap.Open(d.pool, d.t.OID, d.t.Desc)
	var tids []tuple.TID
	for _, r := range rows {
		tup, err := tuple.Form(d.t.Desc, r, nullsOf(r))
		if err != nil {
			t.Fatal(err)
		}
		tid, err := h.Insert(tup, tuple.FrozenXID)
		if err != nil {
			t.Fatal(err)
		}
		tids = append(tids, tid)
	}
	return tids
}

func nullsOf(r []tuple.Datum) []bool {
	nulls := make([]bool, len(r))
	for i, v := range r {
		nulls[i] = v == nil
	}
	return nulls
}

// entries returns every (key, TID) of an index in index order; a NULL
// key is nil.
func (d *db) entries(t *testing.T, idx *catalog.IndexInfo) (keys []tuple.Datum, tids []tuple.TID) {
	t.Helper()
	tree := Open(d.pool, d.t, idx)
	if err := tree.Check(); err != nil {
		t.Fatalf("Check(%s): %v", idx.Name, err)
	}
	s := tree.Scan(nil, nil)
	defer s.Close()
	for s.Next() {
		keys = append(keys, s.Key())
		tids = append(tids, s.TID())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return keys, tids
}

func uniqueErr(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrUniqueViolation) {
		t.Fatalf("got %v (%T), want *Error wrapping ErrUniqueViolation", err, err)
	}
	if e.Msg != want || err.Error() != want {
		t.Fatalf("message %q, want %q", e.Msg, want)
	}
}

func TestOpen(t *testing.T) {
	d := newDB(t)
	for _, tc := range []struct {
		name string
		attr int
		typ  tuple.TypeID
	}{{"t_a", 0, tuple.Int4}, {"t_b", 1, tuple.Text}, {"t_c", 2, tuple.Bool}} {
		idx := d.index(t, tc.name, tc.attr, false)
		tree := Open(d.pool, d.t, idx)
		if tree.OID() != idx.OID || tree.KeyType() != tc.typ {
			t.Errorf("Open(%s): OID %d type %v, want %d %v", tc.name, tree.OID(), tree.KeyType(), idx.OID, tc.typ)
		}
		meta, err := tree.Meta()
		if err != nil || meta.Root != btree.None {
			t.Errorf("Open(%s): Meta = %+v, %v", tc.name, meta, err)
		}
	}
}

var rows = [][]tuple.Datum{
	{int32(3), "c", true},
	{int32(1), nil, false},
	{int32(2), "a", nil},
	{int32(2), "b", true},
	{nil, "a", false},
}

func TestBuild(t *testing.T) {
	d := newDB(t)
	tids := d.seed(t, rows...)
	a := d.index(t, "t_a", 0, false)
	b := d.index(t, "t_b", 1, false)
	c := d.index(t, "t_c", 2, false)
	for _, idx := range []*catalog.IndexInfo{a, b, c} {
		if err := Build(d.pool, d.t, idx); err != nil {
			t.Fatalf("Build(%s): %v", idx.Name, err)
		}
	}
	// Entries are in key order, duplicates in TID order, NULL keys last.
	keys, got := d.entries(t, a)
	if want := []tuple.Datum{int32(1), int32(2), int32(2), int32(3), nil}; !reflect.DeepEqual(keys, want) {
		t.Errorf("t_a keys = %v, want %v", keys, want)
	}
	if want := []tuple.TID{tids[1], tids[2], tids[3], tids[0], tids[4]}; !reflect.DeepEqual(got, want) {
		t.Errorf("t_a TIDs = %v, want %v", got, want)
	}
	keys, got = d.entries(t, b)
	if want := []tuple.Datum{"a", "a", "b", "c", nil}; !reflect.DeepEqual(keys, want) {
		t.Errorf("t_b keys = %v, want %v", keys, want)
	}
	if want := []tuple.TID{tids[2], tids[4], tids[3], tids[0], tids[1]}; !reflect.DeepEqual(got, want) {
		t.Errorf("t_b TIDs = %v, want %v", got, want)
	}
	keys, _ = d.entries(t, c)
	if want := []tuple.Datum{false, false, true, true, nil}; !reflect.DeepEqual(keys, want) {
		t.Errorf("t_c keys = %v, want %v", keys, want)
	}
	// Deleted tuples are not indexed.
	h := heap.Open(d.pool, d.t.OID, d.t.Desc)
	if err := h.Delete(tids[0], tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	e := d.index(t, "t_a2", 0, false)
	if err := Build(d.pool, d.t, e); err != nil {
		t.Fatal(err)
	}
	if _, got := d.entries(t, e); len(got) != 4 {
		t.Errorf("index built after a delete has %d entries, want 4", len(got))
	}
	// An empty table builds an empty index.
	if _, err := d.cat.CreateTable("e", tDesc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	empty, _ := d.cat.Lookup("e")
	if _, err := d.cat.CreateIndex("e_a", empty.OID, 0, true, false, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	empty, _ = d.cat.Lookup("e")
	if err := Build(d.pool, empty, empty.Indexes[0]); err != nil {
		t.Fatal(err)
	}
	if meta, err := Open(d.pool, empty, empty.Indexes[0]).Meta(); err != nil || meta.Root != btree.None {
		t.Errorf("empty build: Meta = %+v, %v", meta, err)
	}
}

func TestBuildUnique(t *testing.T) {
	d := newDB(t)
	d.seed(t, rows...)
	// Column b has "a" twice; column a has 2 twice; c has NULL once and
	// true twice.
	for _, tc := range []struct {
		name string
		attr int
	}{{"t_a_key", 0}, {"t_b_key", 1}, {"t_c_key", 2}} {
		idx := d.index(t, tc.name, tc.attr, true)
		uniqueErr(t, Build(d.pool, d.t, idx), `could not create unique index "`+tc.name+`"`)
	}
	// NULLs are not duplicates of each other.
	if _, err := d.cat.CreateTable("n", tDesc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	n, _ := d.cat.Lookup("n")
	h := heap.Open(d.pool, n.OID, n.Desc)
	for _, r := range [][]tuple.Datum{{nil, "x", nil}, {nil, "y", nil}, {int32(1), "z", nil}} {
		tup, _ := tuple.Form(n.Desc, r, nullsOf(r))
		if _, err := h.Insert(tup, tuple.FrozenXID); err != nil {
			t.Fatal(err)
		}
	}
	for _, attr := range []int{0, 2} {
		name := "n_" + string(rune('a'+attr)) + "_key"
		if _, err := d.cat.CreateIndex(name, n.OID, attr, true, false, tuple.FrozenXID); err != nil {
			t.Fatal(err)
		}
		n, _ = d.cat.Lookup("n")
		idx, _ := d.cat.LookupIndex(name)
		if err := Build(d.pool, n, idx); err != nil {
			t.Errorf("Build(%s) over NULLs: %v", name, err)
		}
	}
}

func TestInsert(t *testing.T) {
	d := newDB(t)
	a := d.index(t, "t_a", 0, false)
	b := d.index(t, "t_b", 1, false)
	h := heap.Open(d.pool, d.t.OID, d.t.Desc)
	var tids []tuple.TID
	for _, r := range rows {
		tup, _ := tuple.Form(d.t.Desc, r, nullsOf(r))
		tid, err := h.Insert(tup, tuple.FrozenXID)
		if err != nil {
			t.Fatal(err)
		}
		if err := Insert(d.pool, d.t, r, nullsOf(r), tid); err != nil {
			t.Fatal(err)
		}
		tids = append(tids, tid)
	}
	keys, got := d.entries(t, a)
	if want := []tuple.Datum{int32(1), int32(2), int32(2), int32(3), nil}; !reflect.DeepEqual(keys, want) {
		t.Errorf("t_a keys = %v, want %v", keys, want)
	}
	if want := []tuple.TID{tids[1], tids[2], tids[3], tids[0], tids[4]}; !reflect.DeepEqual(got, want) {
		t.Errorf("t_a TIDs = %v, want %v", got, want)
	}
	if keys, _ := d.entries(t, b); !reflect.DeepEqual(keys, []tuple.Datum{"a", "a", "b", "c", nil}) {
		t.Errorf("t_b keys = %v", keys)
	}
	// Insert into an index-less table is a no-op.
	if _, err := d.cat.CreateTable("u", tDesc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	u, _ := d.cat.Lookup("u")
	if err := Insert(d.pool, u, rows[0], nullsOf(rows[0]), tuple.TID{Block: 0, Off: 1}); err != nil {
		t.Errorf("Insert without indexes: %v", err)
	}
}

func TestCheckUnique(t *testing.T) {
	d := newDB(t)
	tids := d.seed(t,
		[]tuple.Datum{int32(1), "a", true},
		[]tuple.Datum{int32(2), "a", false},
		[]tuple.Datum{int32(3), "a", nil},
		[]tuple.Datum{int32(4), "a", nil},
	)
	key := d.index(t, "t_c_key", 2, true)
	d.index(t, "t_b", 1, false) // not unique: never conflicts
	for _, idx := range d.t.Indexes {
		if err := Build(d.pool, d.t, idx); err != nil {
			t.Fatal(err)
		}
	}
	check := func(row []tuple.Datum, except tuple.TID) error {
		return CheckUnique(d.pool, d.t, row, nullsOf(row), except)
	}
	none := tuple.TID{}
	// A taken key conflicts, whatever the other columns hold.
	uniqueErr(t, check([]tuple.Datum{int32(9), "z", true}, none), `duplicate key value violates unique constraint "t_c_key"`)
	uniqueErr(t, check([]tuple.Datum{int32(9), "z", false}, none), `duplicate key value violates unique constraint "t_c_key"`)
	// NULL never conflicts, nor does a duplicate in a non-unique index.
	if err := check([]tuple.Datum{int32(1), "a", nil}, none); err != nil {
		t.Errorf("NULL key: %v", err)
	}
	// The tuple being updated does not conflict with itself.
	if err := check([]tuple.Datum{int32(1), "b", true}, tids[0]); err != nil {
		t.Errorf("update to own key: %v", err)
	}
	uniqueErr(t, check([]tuple.Datum{int32(1), "b", false}, tids[0]), `duplicate key value violates unique constraint "t_c_key"`)
	// A dead heap tuple does not conflict: its index entry is ignored.
	h := heap.Open(d.pool, d.t.OID, d.t.Desc)
	if err := h.Delete(tids[1], tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	if err := check([]tuple.Datum{int32(9), "z", false}, none); err != nil {
		t.Errorf("key of a deleted tuple: %v", err)
	}
	// After a failed check nothing was written: four entries, the NULL
	// keys included.
	if _, got := d.entries(t, key); len(got) != 4 {
		t.Errorf("unique index has %d entries after checks, want 4", len(got))
	}
	// No unique index, no check.
	if _, err := d.cat.CreateTable("u", tDesc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	u, _ := d.cat.Lookup("u")
	if err := CheckUnique(d.pool, u, rows[0], nullsOf(rows[0]), none); err != nil {
		t.Errorf("CheckUnique without indexes: %v", err)
	}
}

func TestCheck(t *testing.T) {
	d := newDB(t)
	d.index(t, "t_b", 1, false)
	key := d.index(t, "t_a_key", 0, true)
	tids := d.seed(t, []tuple.Datum{int32(1), "x", true})
	if err := Build(d.pool, d.t, key); err != nil {
		t.Fatal(err)
	}
	check := func(row []tuple.Datum, except tuple.TID) error {
		return Check(d.pool, d.t, row, nullsOf(row), except)
	}
	none := tuple.TID{}
	if err := check([]tuple.Datum{int32(2), "y", nil}, none); err != nil {
		t.Errorf("valid row: %v", err)
	}
	// Check includes the unique check.
	uniqueErr(t, check([]tuple.Datum{int32(1), "y", nil}, none), `duplicate key value violates unique constraint "t_a_key"`)
	if err := check([]tuple.Datum{int32(1), "y", nil}, tids[0]); err != nil {
		t.Errorf("own key: %v", err)
	}
	// A key whose index tuple would exceed btree.MaxItemSize is refused
	// before anything is written; the limit is on the index tuple (8-byte
	// header, 4-byte length, the bytes), not the heap tuple.
	long := strings.Repeat("x", btree.MaxItemSize-12)
	if err := check([]tuple.Datum{int32(2), long, nil}, none); err != nil {
		t.Errorf("key at the limit: %v", err)
	}
	err := check([]tuple.Datum{int32(2), long + "x", nil}, none)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized key: %v (%T), want *Error wrapping ErrTooLarge", err, err)
	}
	if want := `index row size 2705 exceeds btree version 4 maximum 2704 for index "t_b"`; e.Msg != want {
		t.Errorf("message %q, want %q", e.Msg, want)
	}
	// A NULL is never too large.
	if err := check([]tuple.Datum{int32(2), nil, nil}, none); err != nil {
		t.Errorf("NULL key: %v", err)
	}
	// Build reports the same error over an oversized value.
	d.seed(t, []tuple.Datum{int32(3), long + "x", nil})
	idx := d.index(t, "t_b2", 1, false)
	err = Build(d.pool, d.t, idx)
	if !errors.As(err, &e) || !errors.Is(err, ErrTooLarge) || e.Msg != `index row size 2705 exceeds btree version 4 maximum 2704 for index "t_b2"` {
		t.Errorf("Build over an oversized value: %#v", err)
	}
}

// TestBuildMatchesInsert inserts random rows through Insert into one
// index and builds another over the same heap afterwards: both must hold
// the same entries, and both must pass the checker.
func TestBuildMatchesInsert(t *testing.T) {
	d := newDB(t)
	live := d.index(t, "t_live", 1, false)
	rng := rand.New(rand.NewSource(13))
	h := heap.Open(d.pool, d.t.OID, d.t.Desc)
	type entry struct {
		key string
		tid tuple.TID
	}
	var want []entry
	for i := 0; i < 2000; i++ {
		r := []tuple.Datum{int32(i), string(rune('a' + rng.Intn(26))), rng.Intn(2) == 0}
		if rng.Intn(10) == 0 {
			r[1] = nil
		}
		tup, _ := tuple.Form(d.t.Desc, r, nullsOf(r))
		tid, err := h.Insert(tup, tuple.FrozenXID)
		if err != nil {
			t.Fatal(err)
		}
		if err := Insert(d.pool, d.t, r, nullsOf(r), tid); err != nil {
			t.Fatal(err)
		}
		if r[1] != nil {
			want = append(want, entry{r[1].(string), tid})
		}
	}
	sort.Slice(want, func(i, j int) bool {
		if want[i].key != want[j].key {
			return want[i].key < want[j].key
		}
		if want[i].tid.Block != want[j].tid.Block {
			return want[i].tid.Block < want[j].tid.Block
		}
		return want[i].tid.Off < want[j].tid.Off
	})
	built := d.index(t, "t_built", 1, false)
	if err := Build(d.pool, d.t, built); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []*catalog.IndexInfo{live, built} {
		keys, tids := d.entries(t, idx)
		var got []entry
		for i, k := range keys {
			if k != nil {
				got = append(got, entry{k.(string), tids[i]})
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %d entries, want %d (first mismatch %v)", idx.Name, len(got), len(want), firstDiff(got, want))
		}
		if nulls := len(keys) - len(got); nulls != 2000-len(want) {
			t.Errorf("%s: %d NULL entries, want %d", idx.Name, nulls, 2000-len(want))
		}
	}
}

func firstDiff[T comparable](a, b []T) any {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
