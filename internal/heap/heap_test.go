package heap

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

const relOID tuple.OID = 16384

var desc = tuple.NewDesc(
	tuple.Attr{Name: "id", Type: tuple.Int4},
	tuple.Attr{Name: "name", Type: tuple.Text},
)

func newPool(t *testing.T, dir string, nframes int) *bufmgr.Pool {
	t.Helper()
	store, err := smgr.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return bufmgr.New(store, nframes)
}

func create(t *testing.T) (*Relation, *bufmgr.Pool, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir, 8)
	rel, err := Create(pool, relOID, desc)
	if err != nil {
		t.Fatal(err)
	}
	return rel, pool, dir
}

func row(t *testing.T, id int, name string) tuple.Tuple {
	t.Helper()
	tup, err := tuple.Form(desc, []tuple.Datum{int32(id), name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tup
}

func id(t *testing.T, tup tuple.Tuple) int {
	t.Helper()
	values, _, err := tuple.Deform(desc, tup)
	if err != nil {
		t.Fatal(err)
	}
	return int(values[0].(int32))
}

func collect(t *testing.T, rel *Relation) (tids []tuple.TID, ids []int) {
	t.Helper()
	s := rel.Scan()
	defer s.Close()
	for s.Next() {
		tids = append(tids, s.TID())
		ids = append(ids, id(t, s.Tuple()))
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return tids, ids
}

func TestCreateAndOpen(t *testing.T) {
	rel, pool, _ := create(t)
	if rel.OID() != relOID || rel.Desc() != desc {
		t.Fatal("OID or Desc wrong")
	}
	ok, err := pool.Store().Exists(relOID)
	if err != nil || !ok {
		t.Fatalf("relation file missing: %v", err)
	}
	if n, err := rel.NBlocks(); err != nil || n != 0 {
		t.Fatalf("NBlocks = %d, %v", n, err)
	}
	if _, err := Create(pool, relOID, desc); !errors.Is(err, smgr.ErrExists) {
		t.Fatalf("second Create err = %v", err)
	}
	if _, ids := collect(t, Open(pool, relOID, desc)); len(ids) != 0 {
		t.Fatalf("empty relation scanned %v", ids)
	}
}

func TestInsertFetch(t *testing.T) {
	rel, _, _ := create(t)
	tid, err := rel.Insert(row(t, 1, "one"), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tid != (tuple.TID{Block: 0, Off: 1}) {
		t.Fatalf("first TID = %+v", tid)
	}
	tid2, _ := rel.Insert(row(t, 2, "two"), 42)
	if tid2 != (tuple.TID{Block: 0, Off: 2}) {
		t.Fatalf("second TID = %+v", tid2)
	}
	got, err := rel.Fetch(tid)
	if err != nil {
		t.Fatal(err)
	}
	if id(t, got) != 1 || got.Xmin() != 42 || got.Xmax() != 0 || got.Ctid() != tid {
		t.Fatalf("fetched: id=%d xmin=%d xmax=%d ctid=%+v", id(t, got), got.Xmin(), got.Xmax(), got.Ctid())
	}
	if n, _ := rel.NBlocks(); n != 1 {
		t.Fatalf("NBlocks = %d", n)
	}
}

func TestInsertStampsHeaderAndCopies(t *testing.T) {
	rel, _, _ := create(t)
	in := row(t, 7, "seven")
	in.SetXmax(99)
	in.SetInfomask(in.Infomask() | tuple.XminCommitted)
	in.SetCtid(tuple.TID{Block: 5, Off: 5})
	tid, err := rel.Insert(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	in[len(in)-1] = 'X' // mutate the caller's copy
	got, _ := rel.Fetch(tid)
	if got.Xmax() != 0 || got.Infomask()&tuple.XminCommitted != 0 || got.Ctid() != tid {
		t.Fatalf("header not reset: xmax=%d infomask=%#x ctid=%+v", got.Xmax(), got.Infomask(), got.Ctid())
	}
	values, _, _ := tuple.Deform(desc, got)
	if values[1] != "seven" {
		t.Fatalf("Insert did not copy: %q", values[1])
	}
}

func TestFetchErrors(t *testing.T) {
	rel, _, _ := create(t)
	rel.Insert(row(t, 1, "a"), 1)
	for _, tid := range []tuple.TID{{Block: 1, Off: 1}, {Block: 0, Off: 0}, {Block: 0, Off: 2}, {Block: 0, Off: 500}} {
		if _, err := rel.Fetch(tid); !errors.Is(err, ErrNotFound) {
			t.Errorf("Fetch(%+v) err = %v", tid, err)
		}
	}
	if err := rel.Delete(tuple.TID{Block: 3, Off: 1}, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestScanManyPages(t *testing.T) {
	rel, _, _ := create(t)
	const n = 2000
	for i := 0; i < n; i++ {
		if _, err := rel.Insert(row(t, i, fmt.Sprintf("name-%04d-%s", i, bytes.Repeat([]byte("x"), 80))), 1); err != nil {
			t.Fatal(err)
		}
	}
	nb, _ := rel.NBlocks()
	if nb < 20 {
		t.Fatalf("NBlocks = %d, expected many pages", nb)
	}
	tids, ids := collect(t, rel)
	if len(ids) != n {
		t.Fatalf("scanned %d tuples, want %d", len(ids), n)
	}
	for i, got := range ids {
		if got != i {
			t.Fatalf("tuple %d has id %d", i, got)
		}
	}
	for i := 1; i < len(tids); i++ {
		a, b := tids[i-1], tids[i]
		if a.Block > b.Block || (a.Block == b.Block && a.Off >= b.Off) {
			t.Fatalf("scan order not by TID: %+v then %+v", a, b)
		}
	}
}

func TestInsertFillsPagesBeforeExtending(t *testing.T) {
	rel, _, _ := create(t)
	for {
		rel.Insert(row(t, 1, "some text"), 1)
		if n, _ := rel.NBlocks(); n == 2 {
			break
		}
	}
	_, ids := collect(t, rel)
	// Each row is 24 header + 4 + 4 + 9 = 41 bytes, aligned to 48, plus a
	// 4-byte line pointer: 52 bytes. 8168 / 52 = 157 fit on one page.
	if len(ids) != 158 {
		t.Fatalf("second page started after %d rows, want 158", len(ids))
	}
}

func TestDelete(t *testing.T) {
	rel, _, _ := create(t)
	var tids []tuple.TID
	for i := 0; i < 5; i++ {
		tid, _ := rel.Insert(row(t, i, "r"), 1)
		tids = append(tids, tid)
	}
	if err := rel.Delete(tids[2], 77); err != nil {
		t.Fatal(err)
	}
	_, ids := collect(t, rel)
	if fmt.Sprint(ids) != "[0 1 3 4]" {
		t.Fatalf("after delete: %v", ids)
	}
	got, err := rel.Fetch(tids[2])
	if err != nil {
		t.Fatalf("Fetch of deleted tuple: %v", err)
	}
	if got.Xmax() != 77 || id(t, got) != 2 {
		t.Fatalf("deleted tuple: xmax=%d id=%d", got.Xmax(), id(t, got))
	}
	if err := rel.Delete(tids[2], 78); !errors.Is(err, ErrAlreadyDeleted) {
		t.Fatalf("second delete err = %v", err)
	}
	if got, _ := rel.Fetch(tids[2]); got.Xmax() != 77 {
		t.Fatal("failed delete changed xmax")
	}
}

func TestUpdate(t *testing.T) {
	rel, _, _ := create(t)
	old, _ := rel.Insert(row(t, 1, "before"), 1)
	rel.Insert(row(t, 2, "other"), 1)
	nw, err := rel.Update(old, row(t, 1, "after"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if nw == old {
		t.Fatal("update returned the old TID")
	}
	_, ids := collect(t, rel)
	if len(ids) != 2 {
		t.Fatalf("after update: %v", ids)
	}
	oldTup, _ := rel.Fetch(old)
	if oldTup.Xmax() != 5 || oldTup.Ctid() != nw {
		t.Fatalf("old version: xmax=%d ctid=%+v, want 5/%+v", oldTup.Xmax(), oldTup.Ctid(), nw)
	}
	newTup, _ := rel.Fetch(nw)
	values, _, _ := tuple.Deform(desc, newTup)
	if values[1] != "after" || newTup.Xmin() != 5 || newTup.Xmax() != 0 || newTup.Ctid() != nw {
		t.Fatalf("new version: %v xmin=%d xmax=%d ctid=%+v", values, newTup.Xmin(), newTup.Xmax(), newTup.Ctid())
	}
	if _, err := rel.Update(old, row(t, 1, "again"), 6); !errors.Is(err, ErrAlreadyDeleted) {
		t.Fatalf("update of deleted err = %v", err)
	}
}

// TestUpdateFailedInsertLeavesOldVersion updates a row to a version so
// large that it needs a page of its own, with the pool's only frame
// pinned so the relation cannot be extended, and checks that the old
// version was not stamped as deleted.
func TestUpdateFailedInsertLeavesOldVersion(t *testing.T) {
	pool := newPool(t, filepath.Join(t.TempDir(), "data"), 1)
	rel, err := Create(pool, relOID, desc)
	if err != nil {
		t.Fatal(err)
	}
	old, err := rel.Insert(row(t, 1, "before"), 1)
	if err != nil {
		t.Fatal(err)
	}

	// The frame is taken, so the insert of the new version cannot get a
	// page to extend into.
	buf, err := pool.Pin(relOID, old.Block)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Unpin(buf)

	big := row(t, 1, strings.Repeat("x", page.MaxItemSize-64))
	if _, err := rel.Update(old, big, 5); !errors.Is(err, bufmgr.ErrNoUnpinnedBuffers) {
		t.Fatalf("Update that has to extend a full pool = %v, want ErrNoUnpinnedBuffers", err)
	}
	got, err := rel.Fetch(old)
	if err != nil {
		t.Fatal(err)
	}
	if got.Xmax() != tuple.InvalidXID {
		t.Fatalf("failed update left the old version deleted: xmax=%d", got.Xmax())
	}
}

func TestScanSkipsFullyDeletedPages(t *testing.T) {
	rel, _, _ := create(t)
	var tids []tuple.TID
	for {
		tid, _ := rel.Insert(row(t, len(tids), "some text"), 1)
		tids = append(tids, tid)
		if n, _ := rel.NBlocks(); n == 3 {
			break
		}
	}
	for _, tid := range tids {
		if tid.Block == 1 {
			rel.Delete(tid, 2)
		}
	}
	scanned, _ := collect(t, rel)
	for _, tid := range scanned {
		if tid.Block == 1 {
			t.Fatalf("scan returned deleted tuple %+v", tid)
		}
	}
	if scanned[len(scanned)-1].Block != 2 {
		t.Fatal("scan stopped before block 2")
	}
}

func TestScanTupleIsCopy(t *testing.T) {
	rel, _, _ := create(t)
	tid, _ := rel.Insert(row(t, 1, "abc"), 1)
	s := rel.Scan()
	if !s.Next() {
		t.Fatal("no tuple")
	}
	tup := s.Tuple()
	tup[len(tup)-1] = 'z'
	s.Close()
	got, _ := rel.Fetch(tid)
	values, _, _ := tuple.Deform(desc, got)
	if values[1] != "abc" {
		t.Fatalf("scan tuple aliased the page: %q", values[1])
	}
}

func TestTupleTooLarge(t *testing.T) {
	rel, _, _ := create(t)
	big := row(t, 1, string(bytes.Repeat([]byte("x"), page.MaxItemSize)))
	if _, err := rel.Insert(big, 1); !errors.Is(err, ErrTupleTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := rel.NBlocks(); n != 0 {
		t.Fatalf("failed insert extended the relation to %d blocks", n)
	}
	// The largest tuple that fits does fit.
	limit := row(t, 1, "")
	fits := row(t, 1, string(bytes.Repeat([]byte("x"), page.MaxItemSize-len(limit))))
	if _, err := rel.Insert(fits, 1); err != nil {
		t.Fatalf("max-size tuple: %v", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	rel, pool, dir := create(t)
	var tids []tuple.TID
	for i := 0; i < 500; i++ {
		tid, _ := rel.Insert(row(t, i, "persist"), 1)
		tids = append(tids, tid)
	}
	rel.Delete(tids[10], 2)
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	pool.Store().Close()

	pool2 := newPool(t, dir, 4)
	rel2 := Open(pool2, relOID, desc)
	_, ids := collect(t, rel2)
	if len(ids) != 499 || ids[10] != 11 {
		t.Fatalf("after reopen: %d tuples, ids[10]=%d", len(ids), ids[10])
	}
	got, _ := rel2.Fetch(tids[10])
	if got.Xmax() != 2 {
		t.Fatal("deletion did not persist")
	}
}

func TestScanLeavesNothingPinned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir, 2)
	rel, _ := Create(pool, relOID, desc)
	for i := 0; i < 400; i++ {
		if _, err := rel.Insert(row(t, i, "some text"), 1); err != nil {
			t.Fatal(err)
		}
	}
	nb, _ := rel.NBlocks()
	if nb < 3 {
		t.Fatalf("NBlocks = %d", nb)
	}
	s := rel.Scan()
	for s.Next() {
		// While the scan is in progress, other pins must still succeed.
		if _, err := rel.Fetch(tuple.TID{Block: 0, Off: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	s.Close()
	// Every frame can be evicted: pin more distinct pages than frames.
	for blk := tuple.BlockNumber(0); blk < nb; blk++ {
		b, err := pool.Pin(relOID, blk)
		if err != nil {
			t.Fatalf("pin %d after scan: %v", blk, err)
		}
		pool.Unpin(b)
	}
}
