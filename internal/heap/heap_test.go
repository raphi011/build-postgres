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
	s := rel.Scan(nil)
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
	got, _, err := rel.Fetch(tid, nil)
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
	got, _, _ := rel.Fetch(tid, nil)
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
		if _, _, err := rel.Fetch(tid, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("Fetch(%+v) err = %v", tid, err)
		}
	}
	if err := rel.Delete(tuple.TID{Block: 3, Off: 1}, 1, nil); !errors.Is(err, ErrNotFound) {
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
	if err := rel.Delete(tids[2], 77, nil); err != nil {
		t.Fatal(err)
	}
	_, ids := collect(t, rel)
	if fmt.Sprint(ids) != "[0 1 3 4]" {
		t.Fatalf("after delete: %v", ids)
	}
	got, _, err := rel.Fetch(tids[2], nil)
	if err != nil {
		t.Fatalf("Fetch of deleted tuple: %v", err)
	}
	if got.Xmax() != 77 || id(t, got) != 2 {
		t.Fatalf("deleted tuple: xmax=%d id=%d", got.Xmax(), id(t, got))
	}
	if err := rel.Delete(tids[2], 78, nil); !errors.Is(err, ErrAlreadyDeleted) {
		t.Fatalf("second delete err = %v", err)
	}
	if got, _, _ := rel.Fetch(tids[2], nil); got.Xmax() != 77 {
		t.Fatal("failed delete changed xmax")
	}
}

func TestUpdate(t *testing.T) {
	rel, _, _ := create(t)
	old, _ := rel.Insert(row(t, 1, "before"), 1)
	rel.Insert(row(t, 2, "other"), 1)
	nw, err := rel.Update(old, row(t, 1, "after"), 5, nil)
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
	oldTup, _, _ := rel.Fetch(old, nil)
	if oldTup.Xmax() != 5 || oldTup.Ctid() != nw {
		t.Fatalf("old version: xmax=%d ctid=%+v, want 5/%+v", oldTup.Xmax(), oldTup.Ctid(), nw)
	}
	newTup, _, _ := rel.Fetch(nw, nil)
	values, _, _ := tuple.Deform(desc, newTup)
	if values[1] != "after" || newTup.Xmin() != 5 || newTup.Xmax() != 0 || newTup.Ctid() != nw {
		t.Fatalf("new version: %v xmin=%d xmax=%d ctid=%+v", values, newTup.Xmin(), newTup.Xmax(), newTup.Ctid())
	}
	if _, err := rel.Update(old, row(t, 1, "again"), 6, nil); !errors.Is(err, ErrAlreadyDeleted) {
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
	if _, err := rel.Update(old, big, 5, nil); !errors.Is(err, bufmgr.ErrNoUnpinnedBuffers) {
		t.Fatalf("Update that has to extend a full pool = %v, want ErrNoUnpinnedBuffers", err)
	}
	got, visible, err := rel.Fetch(old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !visible || got.Xmax() != tuple.InvalidXID {
		t.Fatalf("failed update left the old version deleted: xmax=%d visible=%v", got.Xmax(), visible)
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
			rel.Delete(tid, 2, nil)
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
	s := rel.Scan(nil)
	if !s.Next() {
		t.Fatal("no tuple")
	}
	tup := s.Tuple()
	tup[len(tup)-1] = 'z'
	s.Close()
	got, _, _ := rel.Fetch(tid, nil)
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
	rel.Delete(tids[10], 2, nil)
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
	got, _, _ := rel2.Fetch(tids[10], nil)
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
	s := rel.Scan(nil)
	for s.Next() {
		// While the scan is in progress, other pins must still succeed.
		if _, _, err := rel.Fetch(tuple.TID{Block: 0, Off: 1}, nil); err != nil {
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

// Chapter 17: snapshots.

// fakeSnap is a Snapshot over a table of verdicts: 'c' committed, 'a'
// aborted, 'r' running. own is the snapshot's transaction. It reads
// the hint bits before the table and sets them after, and counts the
// table lookups. Wait records the ID and applies the verdict in after.
type fakeSnap struct {
	own     tuple.XID
	status  map[tuple.XID]byte
	after   map[tuple.XID]byte
	lookups int
	waited  []tuple.XID
}

func (f *fakeSnap) verdict(xid tuple.XID) byte {
	if xid == f.own {
		return 'c'
	}
	f.lookups++
	st, ok := f.status[xid]
	if !ok {
		panic(fmt.Sprintf("fakeSnap: no verdict for %d", xid))
	}
	return st
}

func (f *fakeSnap) xmin(t tuple.Tuple) byte {
	switch {
	case t.Infomask()&tuple.XminCommitted != 0:
		return 'c'
	case t.Infomask()&tuple.XminInvalid != 0:
		return 'a'
	case t.Xmin() == f.own:
		return 'c'
	}
	v := f.verdict(t.Xmin())
	switch v {
	case 'c':
		t.SetInfomask(t.Infomask() | tuple.XminCommitted)
	case 'a':
		t.SetInfomask(t.Infomask() | tuple.XminInvalid)
	}
	return v
}

func (f *fakeSnap) xmax(t tuple.Tuple) byte {
	switch {
	case t.Infomask()&tuple.XmaxCommitted != 0:
		return 'c'
	case t.Infomask()&tuple.XmaxInvalid != 0:
		return 'a'
	}
	v := f.verdict(t.Xmax())
	switch v {
	case 'c':
		t.SetInfomask(t.Infomask() | tuple.XmaxCommitted)
	case 'a':
		t.SetInfomask(t.Infomask() | tuple.XmaxInvalid)
	}
	return v
}

func (f *fakeSnap) Visible(t tuple.Tuple) (bool, error) {
	if f.xmin(t) != 'c' {
		return false, nil
	}
	switch {
	case t.Xmax() == 0:
		return true, nil
	case t.Xmax() == f.own:
		return false, nil
	}
	return f.xmax(t) != 'c', nil
}

func (f *fakeSnap) Dirty(t tuple.Tuple) (bool, tuple.XID, error) {
	switch f.xmin(t) {
	case 'a':
		return false, 0, nil
	case 'r':
		return false, t.Xmin(), nil
	}
	switch {
	case t.Xmax() == 0:
		return true, 0, nil
	case t.Xmax() == f.own:
		return false, 0, nil
	}
	switch f.xmax(t) {
	case 'r':
		return true, t.Xmax(), nil
	case 'c':
		return false, 0, nil
	}
	return true, 0, nil
}

func (f *fakeSnap) Modify(t tuple.Tuple, tid tuple.TID) (TM, error) {
	if f.xmin(t) != 'c' {
		return TMInvisible, nil
	}
	switch {
	case t.Xmax() == 0:
		return TMOk, nil
	case t.Xmax() == f.own:
		return TMSelfModified, nil
	}
	switch f.xmax(t) {
	case 'r':
		return TMBeingModified, nil
	case 'a':
		return TMOk, nil
	}
	if t.Ctid() == tid {
		return TMDeleted, nil
	}
	return TMUpdated, nil
}

func (f *fakeSnap) Wait(xid tuple.XID) {
	f.waited = append(f.waited, xid)
	f.status[xid] = f.after[xid]
}

// visibility inserts one row per verdict of xmin and xmax and returns the
// relation, the TIDs by row id, and the snapshot of transaction 100.
// Row ids: 1 committed, 2 aborted, 3 running, 4 own; 5 to 8 committed
// inserts deleted by a committed, aborted, running, and the own
// transaction.
func visibility(t *testing.T) (*Relation, []tuple.TID, *fakeSnap) {
	t.Helper()
	rel, _, _ := create(t)
	snap := &fakeSnap{own: 100, status: map[tuple.XID]byte{
		10: 'c', 11: 'a', 12: 'r', 20: 'c', 21: 'a', 22: 'r',
	}, after: map[tuple.XID]byte{}}
	var tids []tuple.TID
	for i, xmin := range []tuple.XID{10, 11, 12, 100, 10, 10, 10, 10} {
		tid, err := rel.Insert(row(t, i+1, "r"), xmin)
		if err != nil {
			t.Fatal(err)
		}
		tids = append(tids, tid)
	}
	for i, xmax := range []tuple.XID{20, 21, 22, 100} {
		if err := rel.Delete(tids[4+i], xmax, nil); err != nil {
			t.Fatal(err)
		}
	}
	return rel, tids, snap
}

func TestScanSnapshot(t *testing.T) {
	rel, tids, snap := visibility(t)
	// Without a snapshot, xmax decides.
	if _, ids := collect(t, rel); fmt.Sprint(ids) != "[1 2 3 4]" {
		t.Errorf("Scan(nil): %v", ids)
	}
	scan := func() []int {
		t.Helper()
		s := rel.Scan(snap)
		defer s.Close()
		var ids []int
		for s.Next() {
			ids = append(ids, id(t, s.Tuple()))
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if ids := scan(); fmt.Sprint(ids) != "[1 4 6 7]" {
		t.Errorf("Scan(snap): %v", ids)
	}
	// The verdicts were written to the page as hint bits: the second scan
	// looks up only the two running transactions, and the bits show in
	// the raw tuples.
	lookups := snap.lookups
	if lookups == 0 {
		t.Fatal("first scan consulted nothing")
	}
	if ids := scan(); fmt.Sprint(ids) != "[1 4 6 7]" || snap.lookups != lookups+2 {
		t.Errorf("second Scan(snap): %v with %d more lookups, want 2", ids, snap.lookups-lookups)
	}
	for i, want := range []uint16{
		tuple.XminCommitted, tuple.XminInvalid, 0, 0,
		tuple.XminCommitted | tuple.XmaxCommitted, tuple.XminCommitted | tuple.XmaxInvalid,
		tuple.XminCommitted, tuple.XminCommitted,
	} {
		got, _, err := rel.Fetch(tids[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Infomask() != want {
			t.Errorf("row %d: infomask %#x, want %#x", i+1, got.Infomask(), want)
		}
	}
	// A running transaction leaves no hint; once it commits, a scan sets
	// one.
	snap.status[12] = 'c'
	if ids := scan(); fmt.Sprint(ids) != "[1 3 4 6 7]" {
		t.Errorf("after 12 committed: %v", ids)
	}
	if got, _, _ := rel.Fetch(tids[2], nil); got.Infomask() != tuple.XminCommitted {
		t.Errorf("row 3: infomask %#x after its transaction committed", got.Infomask())
	}
}

func TestFetchSnapshot(t *testing.T) {
	rel, tids, snap := visibility(t)
	for i, want := range []bool{true, false, false, true, false, true, true, false} {
		got, visible, err := rel.Fetch(tids[i], snap)
		if err != nil {
			t.Fatal(err)
		}
		if visible != want || id(t, got) != i+1 {
			t.Errorf("Fetch(row %d) = id %d visible %v, want %v", i+1, id(t, got), visible, want)
		}
	}
	// Fetch writes hint bits too.
	if got, _, _ := rel.Fetch(tids[0], nil); got.Infomask() != tuple.XminCommitted {
		t.Errorf("row 1: infomask %#x after Fetch", got.Infomask())
	}
	lookups := snap.lookups
	rel.Fetch(tids[0], snap)
	if snap.lookups != lookups {
		t.Error("second Fetch consulted the log")
	}
	// With a nil snapshot visible means xmax == 0.
	for i, want := range []bool{true, true, true, true, false, false, false, false} {
		if _, visible, _ := rel.Fetch(tids[i], nil); visible != want {
			t.Errorf("Fetch(row %d, nil) visible %v, want %v", i+1, visible, want)
		}
	}
}

func TestDeleteSnapshot(t *testing.T) {
	rel, tids, snap := visibility(t)
	// Invisible: the inserting transaction aborted or is running.
	for _, i := range []int{1, 2} {
		if err := rel.Delete(tids[i], 100, snap); !errors.Is(err, ErrInvisible) {
			t.Errorf("Delete(row %d) = %v, want ErrInvisible", i+1, err)
		}
	}
	// Already deleted: by a committed transaction or by our own.
	for _, i := range []int{4, 7} {
		if err := rel.Delete(tids[i], 100, snap); !errors.Is(err, ErrAlreadyDeleted) {
			t.Errorf("Delete(row %d) = %v, want ErrAlreadyDeleted", i+1, err)
		}
	}
	// An aborted xmax is overwritten, and its hint bit goes.
	rel.Fetch(tids[5], snap) // sets XmaxInvalid
	if err := rel.Delete(tids[5], 100, snap); err != nil {
		t.Fatalf("Delete over an aborted xmax: %v", err)
	}
	got, _, _ := rel.Fetch(tids[5], nil)
	if got.Xmax() != 100 || got.Infomask()&(tuple.XmaxInvalid|tuple.XmaxCommitted) != 0 {
		t.Errorf("row 6 after delete: xmax %d infomask %#x", got.Xmax(), got.Infomask())
	}
	// A running xmax makes Delete wait; what happens then depends on the
	// verdict: committed is ErrAlreadyDeleted, aborted lets us through.
	snap.after[22] = 'c'
	if err := rel.Delete(tids[6], 100, snap); !errors.Is(err, ErrAlreadyDeleted) {
		t.Errorf("Delete after the deleter committed = %v", err)
	}
	if fmt.Sprint(snap.waited) != "[22]" {
		t.Errorf("waited for %v, want [22]", snap.waited)
	}
	tid, _ := rel.Insert(row(t, 9, "r"), 10)
	rel.Delete(tid, 23, nil)
	snap.status[23], snap.after[23] = 'r', 'a'
	if err := rel.Delete(tid, 100, snap); err != nil {
		t.Errorf("Delete after the deleter aborted = %v", err)
	}
	if got, _, _ := rel.Fetch(tid, nil); got.Xmax() != 100 {
		t.Errorf("row 9 xmax = %d", got.Xmax())
	}
	if fmt.Sprint(snap.waited) != "[22 23]" {
		t.Errorf("waited for %v, want [22 23]", snap.waited)
	}
	// Live rows delete as before.
	if err := rel.Delete(tids[0], 100, snap); err != nil {
		t.Errorf("Delete(row 1) = %v", err)
	}
}

func TestUpdateSnapshot(t *testing.T) {
	rel, tids, snap := visibility(t)
	if _, err := rel.Update(tids[4], row(t, 5, "new"), 100, snap); !errors.Is(err, ErrAlreadyDeleted) {
		t.Errorf("Update of a deleted row = %v", err)
	}
	if _, err := rel.Update(tids[1], row(t, 2, "new"), 100, snap); !errors.Is(err, ErrInvisible) {
		t.Errorf("Update of an invisible row = %v", err)
	}
	big := row(t, 1, string(bytes.Repeat([]byte("x"), page.MaxItemSize)))
	if _, err := rel.Update(tids[0], big, 100, snap); !errors.Is(err, ErrTupleTooLarge) {
		t.Errorf("Update with a huge tuple = %v", err)
	}
	if got, _, _ := rel.Fetch(tids[0], nil); got.Xmax() != 0 {
		t.Error("failed update stamped the old version")
	}
	// The aborted delete of row 6 is overwritten; the new version points
	// back and forth as before.
	nw, err := rel.Update(tids[5], row(t, 6, "new"), 100, snap)
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := rel.Fetch(tids[5], nil)
	if old.Xmax() != 100 || old.Ctid() != nw {
		t.Errorf("old version: xmax %d ctid %+v, want 100 %+v", old.Xmax(), old.Ctid(), nw)
	}
	if _, visible, _ := rel.Fetch(nw, snap); !visible {
		t.Error("own new version invisible")
	}
	if _, visible, _ := rel.Fetch(tids[5], snap); visible {
		t.Error("own old version still visible")
	}
}

// lostRace answers the first Modify as the snapshot it wraps does and
// every later one TMUpdated: an Update passes its check and then finds
// the row taken by another transaction before it can stamp it.
type lostRace struct {
	*fakeSnap
	calls int
}

func (l *lostRace) Modify(t tuple.Tuple, tid tuple.TID) (TM, error) {
	l.calls++
	if l.calls == 1 {
		return l.fakeSnap.Modify(t, tid)
	}
	return TMUpdated, nil
}

// TestUpdateLostRaceHidesNewVersion checks that an update which loses the
// row after its check leaves nothing behind: the version it has already
// inserted carries its own xmax, so no snapshot ever sees it.
func TestUpdateLostRaceHidesNewVersion(t *testing.T) {
	rel, tids, snap := visibility(t)
	if _, err := rel.Update(tids[0], row(t, 1, "new"), 100, &lostRace{fakeSnap: snap}); !errors.Is(err, ErrAlreadyDeleted) {
		t.Fatalf("Update of a row taken after the check = %v, want ErrAlreadyDeleted", err)
	}
	if got, _, _ := rel.Fetch(tids[0], nil); got.Xmax() != tuple.InvalidXID {
		t.Errorf("lost update stamped the old version: xmax=%d", got.Xmax())
	}
	// The new version is on the page, but dead to its own transaction
	// and to everyone else.
	var found bool
	scan := rel.Scan(Any)
	for scan.Next() {
		if scan.TID() == tids[0] || scan.Tuple().Xmin() != 100 || scan.Tuple().Xmax() == tuple.InvalidXID {
			continue
		}
		found = true
		if _, visible, _ := rel.Fetch(scan.TID(), snap); visible {
			t.Error("the new version of a lost update is visible")
		}
	}
	scan.Close()
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("the new version of a lost update was never inserted")
	}
}

func TestLockSnapshot(t *testing.T) {
	rel, tids, snap := visibility(t)
	// Row 7's deleter is running: Lock waits for it and, after an abort,
	// lets us through; nothing is written either way.
	snap.after[22] = 'a'
	for i, want := range []error{nil, ErrInvisible, ErrInvisible, nil, ErrAlreadyDeleted, nil, nil, ErrAlreadyDeleted} {
		if err := rel.Lock(tids[i], 100, snap); !errors.Is(err, want) {
			t.Errorf("Lock(row %d) = %v, want %v", i+1, err, want)
		}
	}
	if fmt.Sprint(snap.waited) != "[22]" {
		t.Errorf("waited for %v, want [22]", snap.waited)
	}
	if got, _, _ := rel.Fetch(tids[6], nil); got.Xmax() != 22 {
		t.Errorf("Lock changed xmax to %d", got.Xmax())
	}
	if got, _, _ := rel.Fetch(tids[0], nil); got.Xmax() != 0 {
		t.Errorf("Lock changed xmax to %d", got.Xmax())
	}
	if err := rel.Lock(tuple.TID{Block: 9, Off: 1}, 100, snap); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lock of a missing tuple = %v", err)
	}
	// Without a snapshot: any xmax refuses.
	if err := rel.Lock(tids[5], 100, nil); !errors.Is(err, ErrAlreadyDeleted) {
		t.Errorf("Lock(nil) of a deleted row = %v", err)
	}
	if err := rel.Lock(tids[0], 100, nil); err != nil {
		t.Errorf("Lock(nil) of a live row = %v", err)
	}
}
