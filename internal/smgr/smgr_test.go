package smgr

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/tuple"
)

func open(t *testing.T) (*DataDir, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dir
}

func pageOf(b byte) []byte {
	return bytes.Repeat([]byte{b}, page.PageSize)
}

func TestOpenCreatesLayout(t *testing.T) {
	d, dir := open(t)
	if d.Path() != dir {
		t.Fatalf("Path = %q, want %q", d.Path(), dir)
	}
	st, err := os.Stat(filepath.Join(dir, "base"))
	if err != nil || !st.IsDir() {
		t.Fatalf("base dir: %v", err)
	}
	// Reopening an existing directory is fine.
	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d2.Close()
}

func TestOpenRejectsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("Open on a regular file succeeded")
	}
}

func TestCreateExistsUnlink(t *testing.T) {
	d, dir := open(t)
	const rel tuple.OID = 16384
	if ok, err := d.Exists(rel); err != nil || ok {
		t.Fatalf("Exists before create = %v, %v", ok, err)
	}
	if err := d.Create(rel); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Exists(rel); err != nil || !ok {
		t.Fatalf("Exists after create = %v, %v", ok, err)
	}
	st, err := os.Stat(filepath.Join(dir, "base", "16384"))
	if err != nil {
		t.Fatalf("relation file: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("new relation file size = %d", st.Size())
	}
	if n, err := d.NBlocks(rel); err != nil || n != 0 {
		t.Fatalf("NBlocks = %d, %v", n, err)
	}
	if err := d.Create(rel); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create err = %v, want ErrExists", err)
	}
	if err := d.Unlink(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "base", "16384")); !os.IsNotExist(err) {
		t.Fatalf("file after Unlink: %v", err)
	}
	if ok, _ := d.Exists(rel); ok {
		t.Fatal("Exists after Unlink")
	}
	if err := d.Unlink(rel); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Unlink err = %v, want ErrNotFound", err)
	}
	// Can be created again after unlink.
	if err := d.Create(rel); err != nil {
		t.Fatal(err)
	}
}

func TestNotFound(t *testing.T) {
	d, _ := open(t)
	const rel tuple.OID = 99
	buf := make([]byte, page.PageSize)
	if _, err := d.NBlocks(rel); !errors.Is(err, ErrNotFound) {
		t.Errorf("NBlocks: %v", err)
	}
	if err := d.Read(rel, 0, buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read: %v", err)
	}
	if err := d.Write(rel, 0, buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("Write: %v", err)
	}
	if _, err := d.Extend(rel, buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("Extend: %v", err)
	}
	if err := d.Sync(rel); !errors.Is(err, ErrNotFound) {
		t.Errorf("Sync: %v", err)
	}
}

func TestExtendReadWrite(t *testing.T) {
	d, _ := open(t)
	const rel tuple.OID = 1
	if err := d.Create(rel); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		blk, err := d.Extend(rel, pageOf(byte(i)))
		if err != nil {
			t.Fatal(err)
		}
		if int(blk) != i {
			t.Fatalf("Extend #%d returned block %d", i, blk)
		}
	}
	if n, _ := d.NBlocks(rel); n != 5 {
		t.Fatalf("NBlocks = %d", n)
	}
	buf := make([]byte, page.PageSize)
	for i := 0; i < 5; i++ {
		if err := d.Read(rel, tuple.BlockNumber(i), buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, pageOf(byte(i))) {
			t.Fatalf("block %d content wrong", i)
		}
	}
	if err := d.Write(rel, 2, pageOf(0xAA)); err != nil {
		t.Fatal(err)
	}
	d.Read(rel, 2, buf)
	if !bytes.Equal(buf, pageOf(0xAA)) {
		t.Fatal("Write did not take")
	}
	d.Read(rel, 1, buf)
	if !bytes.Equal(buf, pageOf(1)) {
		t.Fatal("Write clobbered a neighbour")
	}
	if n, _ := d.NBlocks(rel); n != 5 {
		t.Fatalf("NBlocks after Write = %d", n)
	}
	if err := d.Sync(rel); err != nil {
		t.Fatal(err)
	}
}

func TestBlockOutOfRange(t *testing.T) {
	d, _ := open(t)
	const rel tuple.OID = 1
	d.Create(rel)
	d.Extend(rel, pageOf(0))
	buf := make([]byte, page.PageSize)
	if err := d.Read(rel, 1, buf); !errors.Is(err, ErrBlockOutOfRange) {
		t.Errorf("Read past end: %v", err)
	}
	if err := d.Write(rel, 1, buf); !errors.Is(err, ErrBlockOutOfRange) {
		t.Errorf("Write past end: %v", err)
	}
	if err := d.Write(rel, 1000, buf); !errors.Is(err, ErrBlockOutOfRange) {
		t.Errorf("Write far past end: %v", err)
	}
	if n, _ := d.NBlocks(rel); n != 1 {
		t.Fatalf("out-of-range Write grew the file: NBlocks = %d", n)
	}
}

func TestWrongBufferSizePanics(t *testing.T) {
	d, _ := open(t)
	d.Create(1)
	for _, size := range []int{0, page.PageSize - 1, page.PageSize + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Extend with %d-byte buffer did not panic", size)
				}
			}()
			d.Extend(1, make([]byte, size))
		}()
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Create(7)
	d.Extend(7, pageOf(1))
	d.Extend(7, pageOf(2))
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if n, err := d.NBlocks(7); err != nil || n != 2 {
		t.Fatalf("NBlocks after reopen = %d, %v", n, err)
	}
	buf := make([]byte, page.PageSize)
	if err := d.Read(7, 1, buf); err != nil || !bytes.Equal(buf, pageOf(2)) {
		t.Fatalf("block 1 after reopen: %v", err)
	}
}

func TestSyncAll(t *testing.T) {
	d, _ := open(t)
	// Nothing open yet.
	if err := d.SyncAll(); err != nil {
		t.Fatalf("SyncAll with no open files: %v", err)
	}
	d.Create(7)
	d.Create(8)
	d.Extend(7, pageOf(1))
	if err := d.SyncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
}

func TestSyncDir(t *testing.T) {
	_, dir := open(t)
	if err := SyncDir(filepath.Join(dir, "base")); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
	if err := SyncDir(filepath.Join(dir, "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncDir of a missing directory = %v, want ErrNotExist", err)
	}
}

func TestRelationsAreIndependent(t *testing.T) {
	d, _ := open(t)
	d.Create(1)
	d.Create(2)
	d.Extend(1, pageOf(1))
	d.Extend(1, pageOf(1))
	d.Extend(2, pageOf(2))
	if n, _ := d.NBlocks(1); n != 2 {
		t.Fatalf("rel 1 NBlocks = %d", n)
	}
	if n, _ := d.NBlocks(2); n != 1 {
		t.Fatalf("rel 2 NBlocks = %d", n)
	}
	buf := make([]byte, page.PageSize)
	d.Read(2, 0, buf)
	if !bytes.Equal(buf, pageOf(2)) {
		t.Fatal("rel 2 block 0 wrong")
	}
	d.Unlink(1)
	if n, err := d.NBlocks(2); err != nil || n != 1 {
		t.Fatalf("rel 2 after unlinking rel 1: %d, %v", n, err)
	}
}

func TestCorruptFileSize(t *testing.T) {
	d, dir := open(t)
	d.Create(5)
	d.Extend(5, pageOf(0))
	if err := os.Truncate(filepath.Join(dir, "base", "5"), page.PageSize-100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NBlocks(5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NBlocks on truncated file: %v", err)
	}
}

func TestConcurrentExtend(t *testing.T) {
	d, _ := open(t)
	d.Create(1)
	const workers, perWorker = 8, 25
	var mu sync.Mutex
	seen := map[tuple.BlockNumber]byte{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				blk, err := d.Extend(1, pageOf(byte(w)))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if _, dup := seen[blk]; dup {
					t.Errorf("block %d returned twice", blk)
				}
				seen[blk] = byte(w)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if n, _ := d.NBlocks(1); int(n) != workers*perWorker {
		t.Fatalf("NBlocks = %d, want %d", n, workers*perWorker)
	}
	buf := make([]byte, page.PageSize)
	for blk, w := range seen {
		if err := d.Read(1, blk, buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, pageOf(w)) {
			t.Fatalf("block %d has wrong content", blk)
		}
	}
}
