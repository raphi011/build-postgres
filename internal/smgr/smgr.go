// Package smgr is the storage manager: it maps (relation, block number) to
// bytes in files under the data directory. See chapters/03-storage-manager.
package smgr

import (
	"errors"

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
}

// Open opens the data directory at path, creating it if necessary.
// Creates path and path/base; fails if path exists and is not a directory.
func Open(path string) (*DataDir, error) {
	panic("not implemented")
}

// Path returns the data directory path.
func (d *DataDir) Path() string { return d.path }

// Close releases all open files and returns the first close error.
// The DataDir must not be used afterwards.
// PostgreSQL: smgrcloseall in smgr.c.
func (d *DataDir) Close() error {
	panic("not implemented")
}

// Create makes an empty relation file and flushes base/ so the new
// directory entry survives a crash.
// Returns ErrExists if base/<rel> already exists.
// PostgreSQL: mdcreate in md.c.
func (d *DataDir) Create(rel tuple.OID) error {
	panic("not implemented")
}

// Exists reports whether the relation file exists.
// A missing file is (false, nil), not an error.
// PostgreSQL: mdexists in md.c.
func (d *DataDir) Exists(rel tuple.OID) (bool, error) {
	panic("not implemented")
}

// Unlink removes the relation file, drops its cached descriptor, and
// flushes base/ so the removal survives a crash.
// Returns ErrNotFound if there is no such file.
// PostgreSQL: mdunlink in md.c.
func (d *DataDir) Unlink(rel tuple.OID) error {
	panic("not implemented")
}

// NBlocks returns the number of pages in the relation, from a fresh stat.
// Returns ErrNotFound if the relation was never created; ErrCorrupt if the
// file length is not a multiple of page.PageSize.
// PostgreSQL: mdnblocks in md.c.
func (d *DataDir) NBlocks(rel tuple.OID) (tuple.BlockNumber, error) {
	panic("not implemented")
}

// Read fills buf, which must be page.PageSize bytes, with block blk.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrBlockOutOfRange if blk >= NBlocks; ErrCorrupt as
// NBlocks does.
// PostgreSQL: mdread in md.c (mdreadv since 17).
func (d *DataDir) Read(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error {
	panic("not implemented")
}

// Write stores buf, which must be page.PageSize bytes, as block blk.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrBlockOutOfRange if blk >= NBlocks (Write never
// grows the file); ErrCorrupt as NBlocks does.
// PostgreSQL: mdwrite in md.c (mdwritev since 17).
func (d *DataDir) Write(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error {
	panic("not implemented")
}

// Extend appends buf as a new block and returns its block number.
// Panics on any other buffer size. Returns ErrNotFound for a relation that
// was never created; ErrCorrupt as NBlocks does. Concurrent calls on one
// relation return distinct block numbers.
// PostgreSQL: mdextend in md.c.
func (d *DataDir) Extend(rel tuple.OID, buf []byte) (tuple.BlockNumber, error) {
	panic("not implemented")
}

// Sync flushes the relation file to stable storage.
// Returns ErrNotFound for a relation that was never created.
// PostgreSQL: mdimmedsync in md.c.
func (d *DataDir) Sync(rel tuple.OID) error {
	panic("not implemented")
}

// SyncAll flushes every relation file this DataDir has open to stable
// storage, stopping at the first error. A relation it never opened has
// nothing of this process's in the page cache.
// PostgreSQL: ProcessSyncRequests in sync.c, which a checkpoint runs.
func (d *DataDir) SyncAll() error {
	panic("not implemented")
}

// SyncDir flushes the directory at path, so that files created in or
// removed from it survive a crash. Syncing a file makes its contents
// durable; only syncing its directory makes its name durable.
// PostgreSQL: fsync_fname on a directory in fd.c.
func SyncDir(path string) error {
	panic("not implemented")
}
