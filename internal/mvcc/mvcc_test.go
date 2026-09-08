package mvcc

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/tuple"
	"github.com/raphi011/build-postgres/internal/txn"
)

// fakeLog is a commit log in a map. It counts the lookups so that tests
// can check that hint bits spare the log, and records the waits.
type fakeLog struct {
	status map[tuple.XID]txn.Status
	reads  int
	waits  []tuple.XID
}

func (l *fakeLog) Status(xid tuple.XID) (txn.Status, error) {
	l.reads++
	st, ok := l.status[xid]
	if !ok {
		return 0, txn.ErrFutureXID
	}
	return st, nil
}

func (l *fakeLog) Wait(xid tuple.XID) { l.waits = append(l.waits, xid) }

// snap is the snapshot the tables below are written against: 10 and
// everything below finished, 12 running, 20 not started, and 15 is the
// snapshot's own transaction, running too but not listed.
func snap(l *fakeLog) *Snapshot {
	return &Snapshot{Xmin: 10, Xmax: 20, Xip: []tuple.XID{12}, Xid: 15, Log: l}
}

// The verdicts the log holds; 12 and 15 are still running.
func newLog() *fakeLog {
	return &fakeLog{status: map[tuple.XID]txn.Status{
		1: txn.Committed, 2: txn.Committed, 5: txn.Committed, 6: txn.Aborted,
		11: txn.Committed, 12: txn.InProgress, 13: txn.Aborted, 14: txn.Committed,
		15: txn.InProgress, 16: txn.Committed, 17: txn.Aborted,
	}}
}

var desc = tuple.NewDesc(tuple.Attr{Name: "a", Type: tuple.Int4})

// tup forms a tuple stamped xmin, xmax, and the hint bits in mask.
func tup(t *testing.T, xmin, xmax tuple.XID, mask uint16) tuple.Tuple {
	t.Helper()
	tp, err := tuple.Form(desc, []tuple.Datum{int32(1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tp.SetXmin(xmin)
	tp.SetXmax(xmax)
	tp.SetInfomask(tp.Infomask() | mask)
	return tp
}

func TestInProgress(t *testing.T) {
	s := snap(newLog())
	for xid, want := range map[tuple.XID]bool{
		1: false, 9: false, 10: false, 11: false, 12: true, 13: false,
		15: false, 19: false, 20: true, 21: true, 1000: true,
	} {
		if got := s.InProgress(xid); got != want {
			t.Errorf("InProgress(%d) = %v, want %v", xid, got, want)
		}
	}
	if got, want := s.String(), "10:20:12"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (&Snapshot{Xmin: 3, Xmax: 3}).String(), "3:3:"; got != want {
		t.Errorf("String() of an empty snapshot = %q, want %q", got, want)
	}
}

func TestVisible(t *testing.T) {
	const (
		xc = tuple.XminCommitted
		xi = tuple.XminInvalid
		mc = tuple.XmaxCommitted
		mi = tuple.XmaxInvalid
	)
	for _, tc := range []struct {
		name       string
		xmin, xmax tuple.XID
		mask       uint16
		visible    bool
		hint       uint16 // hint bits set on the tuple
		reads      int    // log lookups
	}{
		{"committed before xmin", 5, 0, 0, true, xc, 1},
		{"aborted before xmin", 6, 0, 0, false, xi, 1},
		{"committed inside the window", 11, 0, 0, true, xc, 1},
		{"aborted inside the window", 13, 0, 0, false, xi, 1},
		{"running at snapshot time", 12, 0, 0, false, 0, 0},
		{"not started", 20, 0, 0, false, 0, 0},
		{"own insert", 15, 0, 0, true, 0, 0},
		{"bootstrap", 1, 0, 0, true, xc, 1},
		{"frozen", 2, 0, 0, true, xc, 1},
		{"own delete", 5, 15, 0, false, xc, 1},
		{"deleted by a committed transaction", 5, 11, 0, false, xc | mc, 2},
		{"deleted by an aborted transaction", 5, 13, 0, true, xc | mi, 2},
		{"deleted by a transaction running at snapshot time", 5, 12, 0, true, xc, 1},
		{"deleted by a transaction not started", 5, 20, 0, true, xc, 1},
		{"xmin hinted committed", 5, 0, xc, true, 0, 0},
		{"xmin hinted committed but after the snapshot", 12, 0, xc, false, 0, 0},
		{"xmin hinted aborted", 5, 0, xi, false, 0, 0},
		{"xmax hinted committed", 5, 11, xc | mc, false, 0, 0},
		{"xmax hinted committed but after the snapshot", 5, 12, xc | mc, true, 0, 0},
		{"xmax hinted aborted", 5, 11, xc | mi, true, 0, 0},
	} {
		l := newLog()
		tp := tup(t, tc.xmin, tc.xmax, tc.mask)
		got, err := snap(l).Visible(tp)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.visible {
			t.Errorf("%s: visible = %v, want %v", tc.name, got, tc.visible)
		}
		if hint := tp.Infomask() &^ tc.mask &^ tuple.HasNull; hint != tc.hint {
			t.Errorf("%s: hint bits %#x, want %#x", tc.name, hint, tc.hint)
		}
		if l.reads != tc.reads {
			t.Errorf("%s: %d log lookups, want %d", tc.name, l.reads, tc.reads)
		}
		// Asked again, the hint bits answer.
		l.reads = 0
		if again, _ := snap(l).Visible(tp); again != got || l.reads != 0 {
			t.Errorf("%s: second call visible = %v with %d lookups", tc.name, again, l.reads)
		}
	}
}

func TestVisibleFutureXID(t *testing.T) {
	l := newLog()
	// An ID the log has not handed out is invisible without a lookup: it
	// is at or above Xmax.
	if got, err := snap(l).Visible(tup(t, 100, 0, 0)); got || err != nil || l.reads != 0 {
		t.Errorf("future xmin: visible %v err %v reads %d", got, err, l.reads)
	}
	// An ID inside the window that the log does not know is an error.
	if _, err := snap(l).Visible(tup(t, 18, 0, 0)); err == nil {
		t.Error("unknown xmin inside the window did not fail")
	}
}

func TestDirty(t *testing.T) {
	for _, tc := range []struct {
		name       string
		xmin, xmax tuple.XID
		visible    bool
		wait       tuple.XID
	}{
		{"committed", 5, 0, true, 0},
		{"committed after the snapshot", 16, 0, true, 0},
		{"aborted", 13, 0, false, 0},
		{"own insert", 15, 0, true, 0},
		{"being inserted", 12, 0, false, 12},
		{"own delete", 5, 15, false, 0},
		{"deleted", 5, 16, false, 0},
		{"delete aborted", 5, 17, true, 0},
		{"being deleted", 5, 12, true, 12},
	} {
		l := newLog()
		got, wait, err := snap(l).Dirty(tup(t, tc.xmin, tc.xmax, 0))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.visible || wait != tc.wait {
			t.Errorf("%s: visible %v wait %d, want %v %d", tc.name, got, wait, tc.visible, tc.wait)
		}
	}
}

func TestModify(t *testing.T) {
	self := tuple.TID{Block: 0, Off: 1}
	other := tuple.TID{Block: 0, Off: 2}
	for _, tc := range []struct {
		name       string
		xmin, xmax tuple.XID
		ctid       tuple.TID
		want       heap.TM
	}{
		{"live", 5, 0, self, heap.TMOk},
		{"inserted after the snapshot", 16, 0, self, heap.TMOk},
		{"own insert", 15, 0, self, heap.TMOk},
		{"inserting transaction aborted", 13, 0, self, heap.TMInvisible},
		{"inserting transaction running", 12, 0, self, heap.TMInvisible},
		{"own delete", 5, 15, self, heap.TMSelfModified},
		{"deleting transaction running", 5, 12, self, heap.TMBeingModified},
		{"deleting transaction aborted", 5, 17, self, heap.TMOk},
		{"deleted and committed", 5, 16, self, heap.TMDeleted},
		{"updated and committed", 5, 16, other, heap.TMUpdated},
	} {
		tp := tup(t, tc.xmin, tc.xmax, 0)
		tp.SetCtid(tc.ctid)
		got, err := snap(newLog()).Modify(tp, self)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	l := newLog()
	snap(l).Wait(12)
	if !reflect.DeepEqual(l.waits, []tuple.XID{12}) {
		t.Errorf("Wait passed %v to the log", l.waits)
	}
}

func TestTake(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := catalog.WriteControl(dir, catalog.Control{NextOID: catalog.FirstUserOID, NextXID: 10}); err != nil {
		t.Fatal(err)
	}
	m, err := txn.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var txs []*txn.Transaction
	for i := 0; i < 4; i++ {
		tx, err := m.Begin()
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
	}
	txs[1].Commit()
	txs[2].Abort()
	// 10, 12, and 13 are running; 11 committed; 14 is next.
	s := Take(m, 12)
	want := &Snapshot{Xmin: 10, Xmax: 14, Xip: []tuple.XID{10, 13}, Xid: 12, Log: m}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("Take = %+v, want %+v", s, want)
	}
	if got := s.String(); got != "10:14:10,13" {
		t.Errorf("String() = %q", got)
	}
	// The snapshot is fixed: what commits later stays invisible to it.
	txs[0].Commit()
	if v, err := s.Visible(tup(t, 10, 0, 0)); v || err != nil {
		t.Errorf("commit after the snapshot visible: %v, %v", v, err)
	}
	if v, err := s.Visible(tup(t, 11, 0, 0)); !v || err != nil {
		t.Errorf("commit before the snapshot invisible: %v, %v", v, err)
	}
	if v, err := s.Visible(tup(t, 12, 0, 0)); !v || err != nil {
		t.Errorf("own insert invisible: %v, %v", v, err)
	}
	// A fresh snapshot sees it, and with nothing running xmin is xmax.
	s = Take(m, tuple.InvalidXID)
	if v, err := s.Visible(tup(t, 10, 0, 0)); !v || err != nil {
		t.Errorf("committed transaction invisible to a new snapshot: %v, %v", v, err)
	}
	txs[3].Commit()
	if s := Take(m, tuple.InvalidXID); s.String() != "14:14:" {
		t.Errorf("snapshot with nothing running = %s", s)
	}
}
