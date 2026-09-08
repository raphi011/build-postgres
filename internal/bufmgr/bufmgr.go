// Package bufmgr caches relation pages in a fixed pool of frames with
// clock-sweep replacement. See chapters/04-buffer-manager.
package bufmgr

import (
	"errors"
	"sync"

	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// MaxUsageCount caps the clock-sweep usage counter, as BM_MAX_USAGE_COUNT.
const MaxUsageCount = 5

var ErrNoUnpinnedBuffers = errors.New("bufmgr: no unpinned buffers available")

// Buffer is one frame of the pool.
type Buffer struct {
	content sync.RWMutex
}

// Rel returns the relation whose page the buffer holds.
func (b *Buffer) Rel() tuple.OID {
	panic("not implemented")
}

// Block returns the block number of the page the buffer holds.
func (b *Buffer) Block() tuple.BlockNumber {
	panic("not implemented")
}

// Page returns the frame as a page. The slice aliases the frame, so
// writes through it change what Flush writes and what later pins see.
func (b *Buffer) Page() page.Page {
	panic("not implemented")
}

// MarkDirty records that the frame differs from disk. Call it after
// modifying the page under Lock; Flush and eviction write only dirty frames.
// PostgreSQL: MarkBufferDirty in bufmgr.c.
func (b *Buffer) MarkDirty() {
	panic("not implemented")
}

// Lock takes the content lock exclusively.
func (b *Buffer) Lock() { b.content.Lock() }

// Unlock releases the exclusive content lock.
func (b *Buffer) Unlock() { b.content.Unlock() }

// RLock takes the content lock shared.
func (b *Buffer) RLock() { b.content.RLock() }

// RUnlock releases the shared content lock.
func (b *Buffer) RUnlock() { b.content.RUnlock() }

// Stats are cumulative counters for a pool.
type Stats struct {
	Hits, Misses, Evictions, Writes uint64
}

// Pool is a buffer pool over a data directory. It is safe for concurrent use.
type Pool struct {
	store *smgr.DataDir
}

// New creates a pool with nframes frames. Every frame is allocated here,
// once; a frame is never reallocated, which is what lets Page alias it.
// PostgreSQL: InitBufferPool in buf_init.c.
func New(store *smgr.DataDir, nframes int) *Pool {
	panic("not implemented")
}

// NFrames returns the number of frames.
func (p *Pool) NFrames() int {
	panic("not implemented")
}

// Pin returns the buffer holding block blk of rel, loading it if necessary.
// A hit bumps the usage count (capped at MaxUsageCount); a miss evicts by
// clock sweep. Returns an error wrapping smgr.ErrBlockOutOfRange if blk is
// past the end of rel, leaving frames and counters untouched;
// ErrNoUnpinnedBuffers if every frame is pinned; smgr.ErrNotFound if rel
// was never created.
// PostgreSQL: ReadBuffer and BufferAlloc in bufmgr.c.
func (p *Pool) Pin(rel tuple.OID, blk tuple.BlockNumber) (*Buffer, error) {
	panic("not implemented")
}

// Extend adds a zeroed page to rel and returns it pinned. The zero page
// is on disk when Extend returns; the caller initialises it and marks it
// dirty. Returns ErrNoUnpinnedBuffers if every frame is pinned;
// smgr.ErrNotFound if rel was never created.
// PostgreSQL: ExtendBufferedRel in bufmgr.c (ReadBuffer with P_NEW before 16).
func (p *Pool) Extend(rel tuple.OID) (*Buffer, error) {
	panic("not implemented")
}

// Unpin releases one pin on b. The page stays cached; only the frame's
// eligibility for eviction changes. Panics if b is not pinned.
// PostgreSQL: UnpinBuffer in bufmgr.c.
func (p *Pool) Unpin(b *Buffer) {
	panic("not implemented")
}

// Flush writes b to disk if it is dirty and clears the dirty flag. A clean
// buffer is not written and not counted. Returns the store's write error.
// PostgreSQL: FlushBuffer in bufmgr.c.
func (p *Pool) Flush(b *Buffer) error {
	panic("not implemented")
}

// FlushAll writes every dirty frame to disk, stopping at the first write
// error. Unlike an eviction it can meet a frame another goroutine is
// writing to, so it pins each one and takes its content lock shared for
// the write.
// PostgreSQL: BufferSync in bufmgr.c.
func (p *Pool) FlushAll() error {
	panic("not implemented")
}

// Discard drops every frame of rel from the lookup table without writing;
// dirty changes are lost, and no later Pin finds them. A frame another
// goroutine still holds pinned keeps its pins, so that goroutine can
// finish reading it and Unpin it; the frame is reused only once the last
// pin is released. Other relations' frames are untouched.
// PostgreSQL: DropRelationBuffers in bufmgr.c.
func (p *Pool) Discard(rel tuple.OID) {
	panic("not implemented")
}

// Stats returns a snapshot of the counters. Hits and Misses count Pin
// calls only (Extend is neither); Evictions counts frames that held a
// page, so filling free frames is not an eviction; Writes counts every
// page write from Flush, FlushAll or eviction.
func (p *Pool) Stats() Stats {
	panic("not implemented")
}

// Store returns the data directory the pool reads and writes.
func (p *Pool) Store() *smgr.DataDir { return p.store }
