// Package smgr is the storage manager: it maps (relation, block number) to
// bytes in files under the data directory. See chapters/03-storage-manager.
package smgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var (
	ErrExists          = errors.New("smgr: relation already exists")
	ErrNotFound        = errors.New("smgr: relation does not exist")
	ErrBlockOutOfRange = errors.New("smgr: block number out of range")
	ErrCorrupt         = errors.New("smgr: relation file size is not a multiple of the page size")
)

// DataDir is an open data directory. It is safe for concurrent use.
type DataDir struct {
	path string

	mu    sync.Mutex
	files map[tuple.OID]*os.File
}

// Open opens the data directory at path, creating it if necessary.
// Creates path and path/base; fails if path exists and is not a directory.
func Open(path string) (*DataDir, error) {
	if err := os.MkdirAll(filepath.Join(path, "base"), 0o755); err != nil {
		return nil, err
	}
	return &DataDir{path: path, files: map[tuple.OID]*os.File{}}, nil
}

// Path returns the data directory path.
func (d *DataDir) Path() string { return d.path }

// Close releases all open files and returns the first close error.
// The DataDir must not be used afterwards.
// PostgreSQL: smgrcloseall in smgr.c.
func (d *DataDir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var first error
	for rel, f := range d.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(d.files, rel)
	}
	return first
}

func (d *DataDir) relPath(rel tuple.OID) string {
	return filepath.Join(d.path, "base", strconv.FormatUint(uint64(rel), 10))
}

// Create makes an empty relation file and flushes base/ so the new
// directory entry survives a crash.
// Returns ErrExists if base/<rel> already exists.
// PostgreSQL: mdcreate in md.c.
func (d *DataDir) Create(rel tuple.OID) error {
	f, err := os.OpenFile(d.relPath(rel), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	if err := SyncDir(filepath.Join(d.path, "base")); err != nil {
		f.Close()
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.files[rel]; ok {
		old.Close()
	}
	d.files[rel] = f
	return nil
}

// Exists reports whether the relation file exists.
// A missing file is (false, nil), not an error.
// PostgreSQL: mdexists in md.c.
func (d *DataDir) Exists(rel tuple.OID) (bool, error) {
	_, err := os.Stat(d.relPath(rel))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Unlink removes the relation file, drops its cached descriptor, and
// flushes base/ so the removal survives a crash.
// Returns ErrNotFound if there is no such file.
// PostgreSQL: mdunlink in md.c.
func (d *DataDir) Unlink(rel tuple.OID) error {
	d.mu.Lock()
	if f, ok := d.files[rel]; ok {
		f.Close()
		delete(d.files, rel)
	}
	d.mu.Unlock()
	if err := os.Remove(d.relPath(rel)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return SyncDir(filepath.Join(d.path, "base"))
}

// file returns the open descriptor for rel, opening it on first use.
func (d *DataDir) file(rel tuple.OID) (*os.File, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.files[rel]; ok {
		return f, nil
	}
	f, err := os.OpenFile(d.relPath(rel), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.files[rel] = f
	return f, nil
}

func nblocks(f *os.File) (tuple.BlockNumber, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size()%page.PageSize != 0 {
		return 0, fmt.Errorf("%w: %s is %d bytes", ErrCorrupt, f.Name(), st.Size())
	}
	return tuple.BlockNumber(st.Size() / page.PageSize), nil
}

func checkBuf(buf []byte) {
	if len(buf) != page.PageSize {
		panic(fmt.Sprintf("smgr: buffer is %d bytes, want %d", len(buf), page.PageSize))
	}
}

// NBlocks returns the number of pages in the relation, from a fresh stat.
// Returns ErrNotFound if the relation was never created; ErrCorrupt if the
// file length is not a multiple of page.PageSize.
// PostgreSQL: mdnblocks in md.c.
func (d *DataDir) NBlocks(rel tuple.OID) (tuple.BlockNumber, error) {
	f, err := d.file(rel)
	if err != nil {
		return 0, err
	}
	return nblocks(f)
}

// Read fills buf, which must be page.PageSize bytes, with block blk.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrBlockOutOfRange if blk >= NBlocks; ErrCorrupt as
// NBlocks does.
// PostgreSQL: mdread in md.c (mdreadv since 17).
func (d *DataDir) Read(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error {
	checkBuf(buf)
	f, err := d.file(rel)
	if err != nil {
		return err
	}
	n, err := nblocks(f)
	if err != nil {
		return err
	}
	if blk >= n {
		return fmt.Errorf("%w: block %d of %d", ErrBlockOutOfRange, blk, n)
	}
	_, err = f.ReadAt(buf, int64(blk)*page.PageSize)
	return err
}

// Write stores buf, which must be page.PageSize bytes, as block blk.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrBlockOutOfRange if blk >= NBlocks (Write never
// grows the file); ErrCorrupt as NBlocks does.
// PostgreSQL: mdwrite in md.c (mdwritev since 17).
func (d *DataDir) Write(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error {
	checkBuf(buf)
	f, err := d.file(rel)
	if err != nil {
		return err
	}
	n, err := nblocks(f)
	if err != nil {
		return err
	}
	if blk >= n {
		return fmt.Errorf("%w: block %d of %d", ErrBlockOutOfRange, blk, n)
	}
	_, err = f.WriteAt(buf, int64(blk)*page.PageSize)
	return err
}

// Extend appends buf as a new block and returns its block number.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrCorrupt as NBlocks does. Concurrent calls on one
// relation return distinct block numbers.
// PostgreSQL: mdextend in md.c.
func (d *DataDir) Extend(rel tuple.OID, buf []byte) (tuple.BlockNumber, error) {
	checkBuf(buf)
	f, err := d.file(rel)
	if err != nil {
		return 0, err
	}
	// Hold the lock so two concurrent extends do not pick the same block.
	d.mu.Lock()
	defer d.mu.Unlock()
	n, err := nblocks(f)
	if err != nil {
		return 0, err
	}
	if _, err := f.WriteAt(buf, int64(n)*page.PageSize); err != nil {
		return 0, err
	}
	return n, nil
}

// Sync flushes the relation file to stable storage.
// Returns ErrNotFound for a relation that was never created.
// PostgreSQL: mdimmedsync in md.c.
func (d *DataDir) Sync(rel tuple.OID) error {
	f, err := d.file(rel)
	if err != nil {
		return err
	}
	return f.Sync()
}

// SyncAll flushes every relation file this DataDir has open to stable
// storage, stopping at the first error. A relation it never opened has
// nothing of this process's in the page cache.
// PostgreSQL: ProcessSyncRequests in sync.c, which a checkpoint runs.
func (d *DataDir) SyncAll() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.files {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// SyncDir flushes the directory at path, so that files created in or
// removed from it survive a crash. Syncing a file makes its contents
// durable; only syncing its directory makes its name durable.
// PostgreSQL: fsync_fname on a directory in fd.c.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
