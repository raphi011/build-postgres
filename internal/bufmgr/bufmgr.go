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

type tag struct {
	rel tuple.OID
	blk tuple.BlockNumber
}

// Buffer is one frame of the pool.
type Buffer struct {
	content sync.RWMutex
	pool    *Pool

	// The fields below are protected by the pool mutex.
	tag   tag
	valid bool // holds a page
	pins  int
	usage int
	dirty bool
	frame page.Page
}

// Rel returns the relation whose page the buffer holds.
func (b *Buffer) Rel() tuple.OID { return b.tag.rel }

// Block returns the block number of the page the buffer holds.
func (b *Buffer) Block() tuple.BlockNumber { return b.tag.blk }

// Page returns the frame as a page. The slice aliases the frame, so
// writes through it change what Flush writes and what later pins see.
func (b *Buffer) Page() page.Page { return b.frame }

// MarkDirty records that the frame differs from disk. Call it after
// modifying the page under Lock; Flush and eviction write only dirty frames.
// PostgreSQL: MarkBufferDirty in bufmgr.c.
func (b *Buffer) MarkDirty() { b.pool.mu.Lock(); b.dirty = true; b.pool.mu.Unlock() }

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

	mu     sync.Mutex
	frames []*Buffer
	table  map[tag]*Buffer
	nused  int // frames that have ever held a page; lower ones are all used
	hand   int
	stats  Stats
}

// New creates a pool with nframes frames. Every frame is allocated here,
// once; a frame is never reallocated, which is what lets Page alias it.
// PostgreSQL: InitBufferPool in buf_init.c.
func New(store *smgr.DataDir, nframes int) *Pool {
	if nframes < 1 {
		panic("bufmgr: pool needs at least one frame")
	}
	p := &Pool{store: store, table: map[tag]*Buffer{}}
	for i := 0; i < nframes; i++ {
		p.frames = append(p.frames, &Buffer{pool: p, frame: make(page.Page, page.PageSize)})
	}
	return p
}

// NFrames returns the number of frames.
func (p *Pool) NFrames() int { return len(p.frames) }

// Pin returns the buffer holding block blk of rel, loading it if necessary.
// A hit bumps the usage count (capped at MaxUsageCount); a miss evicts by
// clock sweep. Returns an error wrapping smgr.ErrBlockOutOfRange if blk is
// past the end of rel, leaving frames and counters untouched;
// ErrNoUnpinnedBuffers if every frame is pinned; smgr.ErrNotFound if rel
// was never created.
// PostgreSQL: ReadBuffer and BufferAlloc in bufmgr.c.
func (p *Pool) Pin(rel tuple.OID, blk tuple.BlockNumber) (*Buffer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.table[tag{rel, blk}]; ok {
		p.stats.Hits++
		b.pins++
		if b.usage < MaxUsageCount {
			b.usage++
		}
		return b, nil
	}
	// Check the block exists before touching any frame.
	n, err := p.store.NBlocks(rel)
	if err != nil {
		return nil, err
	}
	if blk >= n {
		return nil, smgr.ErrBlockOutOfRange
	}
	b, err := p.victim()
	if err != nil {
		return nil, err
	}
	if err := p.store.Read(rel, blk, b.frame); err != nil {
		b.valid = false
		return nil, err
	}
	p.stats.Misses++
	p.install(b, tag{rel, blk})
	return b, nil
}

// Extend adds a zeroed page to rel and returns it pinned. The zero page
// is on disk when Extend returns; the caller initialises it and marks it
// dirty. Returns ErrNoUnpinnedBuffers if every frame is pinned;
// smgr.ErrNotFound if rel was never created.
// PostgreSQL: ExtendBufferedRel in bufmgr.c (ReadBuffer with P_NEW before 16).
func (p *Pool) Extend(rel tuple.OID) (*Buffer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := p.victim()
	if err != nil {
		return nil, err
	}
	clear(b.frame)
	blk, err := p.store.Extend(rel, b.frame)
	if err != nil {
		b.valid = false
		return nil, err
	}
	p.install(b, tag{rel, blk})
	return b, nil
}

// install records b as holding t, pinned once. Caller holds p.mu and b is a
// frame returned by victim.
func (p *Pool) install(b *Buffer, t tag) {
	b.tag = t
	b.valid = true
	b.pins = 1
	b.usage = 1
	b.dirty = false
	p.table[t] = b
}

// victim returns a frame that holds no page, writing out an evicted page if
// it was dirty. Caller holds p.mu.
func (p *Pool) victim() (*Buffer, error) {
	if p.nused < len(p.frames) {
		b := p.frames[p.nused]
		p.nused++
		return b, nil
	}
	pinned := 0
	for pinned < len(p.frames) {
		b := p.frames[p.hand]
		p.hand = (p.hand + 1) % len(p.frames)
		if b.pins > 0 {
			pinned++
			continue
		}
		pinned = 0
		if !b.valid {
			// Freed by Discard or a failed load.
			return b, nil
		}
		if b.usage > 0 {
			b.usage--
			continue
		}
		if err := p.flush(b); err != nil {
			return nil, err
		}
		delete(p.table, b.tag)
		b.valid = false
		p.stats.Evictions++
		return b, nil
	}
	return nil, ErrNoUnpinnedBuffers
}

// flush writes b if dirty. Caller holds p.mu.
func (p *Pool) flush(b *Buffer) error {
	if !b.dirty {
		return nil
	}
	if err := p.store.Write(b.tag.rel, b.tag.blk, b.frame); err != nil {
		return err
	}
	p.stats.Writes++
	b.dirty = false
	return nil
}

// Unpin releases one pin on b. The page stays cached; only the frame's
// eligibility for eviction changes. Panics if b is not pinned.
// PostgreSQL: UnpinBuffer in bufmgr.c.
func (p *Pool) Unpin(b *Buffer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.pins <= 0 {
		panic("bufmgr: Unpin on an unpinned buffer")
	}
	b.pins--
}

// Flush writes b to disk if it is dirty and clears the dirty flag. A clean
// buffer is not written and not counted. Returns the store's write error.
// PostgreSQL: FlushBuffer in bufmgr.c.
func (p *Pool) Flush(b *Buffer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flush(b)
}

// FlushAll writes every dirty frame to disk, stopping at the first write
// error. Unlike an eviction it can meet a frame another goroutine is
// writing to, so it pins each one and takes its content lock shared for
// the write.
// PostgreSQL: BufferSync in bufmgr.c.
func (p *Pool) FlushAll() error {
	for _, b := range p.frames {
		p.mu.Lock()
		if !b.valid || !b.dirty {
			p.mu.Unlock()
			continue
		}
		// The pin keeps the frame from being evicted and refilled while
		// the mutex is down; the content lock is taken before the mutex,
		// the order MarkDirty already imposes.
		b.pins++
		p.mu.Unlock()

		b.RLock()
		p.mu.Lock()
		err := p.flush(b)
		b.pins--
		p.mu.Unlock()
		b.RUnlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// Discard drops every frame of rel from the lookup table without writing;
// dirty changes are lost, and no later Pin finds them. A frame another
// goroutine still holds pinned keeps its pins, so that goroutine can
// finish reading it and Unpin it; the frame is reused only once the last
// pin is released. Other relations' frames are untouched.
// PostgreSQL: DropRelationBuffers in bufmgr.c.
func (p *Pool) Discard(rel tuple.OID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.frames {
		if b.valid && b.tag.rel == rel {
			// The pins stay: a reader may be halfway through the page,
			// and the frame is not reusable until it lets go.
			delete(p.table, b.tag)
			b.valid = false
			b.dirty = false
		}
	}
}

// Stats returns a snapshot of the counters. Hits and Misses count Pin
// calls only (Extend is neither); Evictions counts frames that held a
// page, so filling free frames is not an eviction; Writes counts every
// page write from Flush, FlushAll or eviction.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Store returns the data directory the pool reads and writes.
func (p *Pool) Store() *smgr.DataDir { return p.store }
