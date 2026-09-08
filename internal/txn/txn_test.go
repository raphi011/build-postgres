package txn

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// newDir returns a data directory with a control file whose next XID is
// next.
func newDir(t *testing.T, next tuple.XID) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := catalog.WriteControl(dir, catalog.Control{NextOID: catalog.FirstUserOID, NextXID: next}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func open(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func begin(t *testing.T, m *Manager) *Transaction {
	t.Helper()
	tx, err := m.Begin()
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func status(t *testing.T, m *Manager, xid tuple.XID) Status {
	t.Helper()
	s, err := m.Status(xid)
	if err != nil {
		t.Fatalf("Status(%d): %v", xid, err)
	}
	return s
}

func readSegment(t *testing.T, dir string, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := Open(dir); !errors.Is(err, catalog.ErrNotBootstrapped) {
		t.Fatalf("Open on empty dir: %v, want ErrNotBootstrapped", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "global"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "global", "control"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, catalog.ErrControlCorrupt) {
		t.Fatalf("Open on corrupt control: %v, want ErrControlCorrupt", err)
	}

	dir = newDir(t, 42)
	m := open(t, dir)
	if got := m.NextXID(); got != 42 {
		t.Errorf("NextXID = %d, want 42", got)
	}
	if info, err := os.Stat(filepath.Join(dir, Dir)); err != nil || !info.IsDir() {
		t.Errorf("pg_xact not created: %v", err)
	}
}

func TestBegin(t *testing.T) {
	dir := newDir(t, tuple.FirstNormalXID)
	m := open(t, dir)
	for want := tuple.FirstNormalXID; want < tuple.FirstNormalXID+5; want++ {
		tx := begin(t, m)
		if tx.XID != want {
			t.Fatalf("Begin: XID %d, want %d", tx.XID, want)
		}
		if got := m.NextXID(); got != want+1 {
			t.Errorf("NextXID after Begin = %d, want %d", got, want+1)
		}
		ctl, err := catalog.ReadControl(dir)
		if err != nil {
			t.Fatal(err)
		}
		if ctl.NextXID != want+1 {
			t.Errorf("control NextXID = %d, want %d", ctl.NextXID, want+1)
		}
		if got := status(t, m, tx.XID); got != InProgress {
			t.Errorf("Status(%d) = %v, want in progress", tx.XID, got)
		}
	}
}

func TestCommitAbort(t *testing.T) {
	dir := newDir(t, tuple.FirstNormalXID)
	m := open(t, dir)
	a, b, c := begin(t, m), begin(t, m), begin(t, m)
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Abort(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		xid  tuple.XID
		want Status
	}{
		{a.XID, Committed},
		{b.XID, Aborted},
		{c.XID, InProgress},
		{tuple.InvalidXID, Aborted},
		{tuple.BootstrapXID, Committed},
		{tuple.FrozenXID, Committed},
	} {
		if got := status(t, m, tc.xid); got != tc.want {
			t.Errorf("Status(%d) = %v, want %v", tc.xid, got, tc.want)
		}
	}
	if _, err := m.Status(c.XID + 1); !errors.Is(err, ErrFutureXID) {
		t.Errorf("Status of unassigned XID: %v, want ErrFutureXID", err)
	}

	// Finishing twice, or the other way round, is refused and changes
	// nothing.
	if err := a.Commit(); !errors.Is(err, ErrFinished) {
		t.Errorf("second Commit: %v, want ErrFinished", err)
	}
	if err := a.Abort(); !errors.Is(err, ErrFinished) {
		t.Errorf("Abort after Commit: %v, want ErrFinished", err)
	}
	if err := b.Commit(); !errors.Is(err, ErrFinished) {
		t.Errorf("Commit after Abort: %v, want ErrFinished", err)
	}
	if got := status(t, m, a.XID); got != Committed {
		t.Errorf("Status(%d) = %v after refused Abort, want committed", a.XID, got)
	}
	if got := status(t, m, b.XID); got != Aborted {
		t.Errorf("Status(%d) = %v after refused Commit, want aborted", b.XID, got)
	}
}

// The commit log packs four transactions per byte, lowest XID in the
// lowest bits: 01 committed, 10 aborted, 00 in progress.
func TestLogGolden(t *testing.T) {
	dir := newDir(t, tuple.FirstNormalXID)
	m := open(t, dir)
	var txs []*Transaction
	for i := 0; i < 7; i++ { // XIDs 3..9
		txs = append(txs, begin(t, m))
	}
	// 3 commits, 4 aborts, 5 stays open, 6 aborts, 7 commits, 8 commits,
	// 9 stays open.
	for i, s := range []Status{Committed, Aborted, InProgress, Aborted, Committed, Committed, InProgress} {
		var err error
		switch s {
		case Committed:
			err = txs[i].Commit()
		case Aborted:
			err = txs[i].Abort()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []byte{
		0b01_00_00_00, // XIDs 0-3: only 3 has a status
		0b01_10_00_10, // XIDs 4-7: 10 00 10 01 from the low bits up
		0b00_00_00_01, // XIDs 8-11
	}
	got := readSegment(t, dir, "0000")
	if len(got) != len(want) {
		t.Fatalf("pg_xact/0000 is %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("byte %d = %08b, want %08b", i, got[i], want[i])
		}
	}
}

func TestRestart(t *testing.T) {
	dir := newDir(t, tuple.FirstNormalXID)
	m := open(t, dir)
	committed, aborted, open1 := begin(t, m), begin(t, m), begin(t, m)
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	// A new Manager on the same directory: statuses persist, the open
	// transaction died with the process, IDs continue after the last one.
	m = open(t, dir)
	if got := status(t, m, committed.XID); got != Committed {
		t.Errorf("committed XID after restart: %v", got)
	}
	if got := status(t, m, aborted.XID); got != Aborted {
		t.Errorf("aborted XID after restart: %v", got)
	}
	if got := status(t, m, open1.XID); got != Aborted {
		t.Errorf("XID left open at shutdown after restart: %v, want aborted", got)
	}
	tx := begin(t, m)
	if tx.XID != open1.XID+1 {
		t.Errorf("first XID after restart = %d, want %d", tx.XID, open1.XID+1)
	}
	// The old handle is dead: its Manager no longer knows the XID.
	if err := open1.Commit(); !errors.Is(err, ErrFinished) {
		t.Errorf("Commit through the closed Manager: %v, want ErrFinished", err)
	}
	if got := status(t, m, open1.XID); got != Aborted {
		t.Errorf("XID left open after refused Commit: %v, want aborted", got)
	}
}

// XIDs at the end of a page and of a segment land in the right byte and
// file.
func TestSegments(t *testing.T) {
	for _, tc := range []struct {
		xid  tuple.XID
		file string
		off  int
	}{
		{XactsPerPage - 1, "0000", PageSize - 1},
		{XactsPerPage, "0000", PageSize},
		{XactsPerSegment - 1, "0000", SegmentSize - 1},
		{XactsPerSegment, "0001", 0},
		{3*XactsPerSegment + 4*17, "0003", 17},
	} {
		dir := newDir(t, tc.xid)
		m := open(t, dir)
		tx := begin(t, m)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := status(t, m, tc.xid); got != Committed {
			t.Errorf("Status(%d) = %v, want committed", tc.xid, got)
		}
		b := readSegment(t, dir, tc.file)
		if len(b) != tc.off+1 {
			t.Errorf("XID %d: %s is %d bytes, want %d", tc.xid, tc.file, len(b), tc.off+1)
			continue
		}
		shift := (tc.xid % XactsPerByte) * BitsPerXact
		if b[tc.off] != byte(Committed)<<shift {
			t.Errorf("XID %d: byte %d of %s = %08b", tc.xid, tc.off, tc.file, b[tc.off])
		}
		for i := 0; i < tc.off; i++ {
			if b[i] != 0 {
				t.Errorf("XID %d: byte %d of %s = %08b, want 0", tc.xid, i, tc.file, b[i])
				break
			}
		}
	}
}

func TestConcurrent(t *testing.T) {
	dir := newDir(t, tuple.FirstNormalXID)
	m := open(t, dir)
	const workers, each = 8, 25
	var wg sync.WaitGroup
	results := make([][]Status, workers)
	xids := make([][]tuple.XID, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				tx, err := m.Begin()
				if err != nil {
					t.Error(err)
					return
				}
				want := Committed
				if (w+i)%3 == 0 {
					want = Aborted
					err = tx.Abort()
				} else {
					err = tx.Commit()
				}
				if err != nil {
					t.Error(err)
					return
				}
				xids[w] = append(xids[w], tx.XID)
				results[w] = append(results[w], want)
			}
		}(w)
	}
	wg.Wait()
	seen := map[tuple.XID]bool{}
	for w := range xids {
		for i, xid := range xids[w] {
			if seen[xid] {
				t.Errorf("XID %d handed out twice", xid)
			}
			seen[xid] = true
			if got := status(t, m, xid); got != results[w][i] {
				t.Errorf("Status(%d) = %v, want %v", xid, got, results[w][i])
			}
		}
	}
	if len(seen) != workers*each {
		t.Errorf("%d distinct XIDs, want %d", len(seen), workers*each)
	}
	if got := m.NextXID(); got != tuple.FirstNormalXID+workers*each {
		t.Errorf("NextXID = %d, want %d", got, tuple.FirstNormalXID+workers*each)
	}
}
