package bufmgr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"

	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

const rel tuple.OID = 100

// setup returns a pool and a relation with nblocks pages whose first byte
// is the block number.
func setup(t *testing.T, nframes, nblocks int) (*Pool, *smgr.DataDir) {
	t.Helper()
	store, err := smgr.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Create(rel); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nblocks; i++ {
		p := page.New(0)
		p.AddItem([]byte{byte(i)})
		if _, err := store.Extend(rel, p); err != nil {
			t.Fatal(err)
		}
	}
	return New(store, nframes), store
}

func firstItem(t *testing.T, p page.Page) byte {
	t.Helper()
	it, err := p.GetItem(1)
	if err != nil {
		t.Fatal(err)
	}
	return it[0]
}

func diskPage(t *testing.T, store *smgr.DataDir, blk tuple.BlockNumber) page.Page {
	t.Helper()
	p := make(page.Page, page.PageSize)
	if err := store.Read(rel, blk, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinHitAndMiss(t *testing.T) {
	pool, _ := setup(t, 4, 4)
	if pool.NFrames() != 4 {
		t.Fatalf("NFrames = %d", pool.NFrames())
	}
	b, err := pool.Pin(rel, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b.Rel() != rel || b.Block() != 2 {
		t.Fatalf("tag = %d/%d", b.Rel(), b.Block())
	}
	if firstItem(t, b.Page()) != 2 {
		t.Fatal("wrong page content")
	}
	b2, err := pool.Pin(rel, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b2 != b {
		t.Fatal("second pin returned a different buffer")
	}
	if s := pool.Stats(); s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v", s)
	}
	pool.Unpin(b)
	pool.Unpin(b2)
	// Still cached after unpin.
	b3, _ := pool.Pin(rel, 2)
	if b3 != b {
		t.Fatal("page dropped after unpin")
	}
	if s := pool.Stats(); s.Hits != 2 || s.Misses != 1 {
		t.Fatalf("stats = %+v", s)
	}
	pool.Unpin(b3)
}

func TestPinOutOfRange(t *testing.T) {
	pool, _ := setup(t, 2, 1)
	_, err := pool.Pin(rel, 5)
	if !errors.Is(err, smgr.ErrBlockOutOfRange) {
		t.Fatalf("err = %v", err)
	}
	if s := pool.Stats(); s.Misses != 0 || s.Evictions != 0 {
		t.Fatalf("stats = %+v", s)
	}
	// Both frames still usable.
	a, err := pool.Pin(rel, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Pin(rel, 5); !errors.Is(err, smgr.ErrBlockOutOfRange) {
		t.Fatal(err)
	}
	pool.Unpin(a)
	if _, err := pool.Pin(rel, 0); err != nil {
		t.Fatal(err)
	}
}

func TestClockSweepEvictsUsageZero(t *testing.T) {
	pool, _ := setup(t, 3, 4)
	pin := func(blk tuple.BlockNumber) {
		b, err := pool.Pin(rel, blk)
		if err != nil {
			t.Fatal(err)
		}
		pool.Unpin(b)
	}
	// A is hot (usage 5), B and C cold (usage 1).
	for i := 0; i < 5; i++ {
		pin(0)
	}
	pin(1)
	pin(2)
	// D needs a frame. Sweep: A 5->4, B 1->0, C 1->0, A 4->3, B is victim.
	pin(3)
	s := pool.Stats()
	if s.Evictions != 1 {
		t.Fatalf("evictions = %d", s.Evictions)
	}
	pin(0) // hit
	pin(2) // hit
	pin(3) // hit
	if s2 := pool.Stats(); s2.Hits != s.Hits+3 {
		t.Fatalf("A, C, D should be hits: %+v then %+v", s, s2)
	}
	pin(1) // miss
	if s3 := pool.Stats(); s3.Misses != s.Misses+1 {
		t.Fatalf("B should be a miss: %+v", s3)
	}
}

func TestClockSweepFreeFramesFirst(t *testing.T) {
	pool, _ := setup(t, 3, 3)
	var bufs []*Buffer
	for blk := tuple.BlockNumber(0); blk < 3; blk++ {
		b, err := pool.Pin(rel, blk)
		if err != nil {
			t.Fatal(err)
		}
		bufs = append(bufs, b)
		pool.Unpin(b)
	}
	if s := pool.Stats(); s.Evictions != 0 {
		t.Fatalf("evicted while free frames remained: %+v", s)
	}
	if bufs[0] == bufs[1] || bufs[1] == bufs[2] || bufs[0] == bufs[2] {
		t.Fatal("frames reused while free frames remained")
	}
}

func TestDirtyEvictionWritesBack(t *testing.T) {
	pool, store := setup(t, 1, 3)
	b, _ := pool.Pin(rel, 0)
	b.Lock()
	b.Page().AddItem([]byte("dirty"))
	b.MarkDirty()
	b.Unlock()
	pool.Unpin(b)

	// Evict block 0 by pinning block 1 (clean), then block 2.
	c, _ := pool.Pin(rel, 1)
	pool.Unpin(c)
	if s := pool.Stats(); s.Writes != 1 || s.Evictions != 1 {
		t.Fatalf("stats after dirty eviction = %+v", s)
	}
	if got, _ := diskPage(t, store, 0).GetItem(2); string(got) != "dirty" {
		t.Fatalf("disk block 0 item 2 = %q", got)
	}
	d, _ := pool.Pin(rel, 2)
	pool.Unpin(d)
	if s := pool.Stats(); s.Writes != 1 || s.Evictions != 2 {
		t.Fatalf("clean eviction wrote: %+v", s)
	}
}

func TestPinnedNeverEvicted(t *testing.T) {
	pool, _ := setup(t, 2, 3)
	a, _ := pool.Pin(rel, 0)
	b, _ := pool.Pin(rel, 1)
	if _, err := pool.Pin(rel, 2); !errors.Is(err, ErrNoUnpinnedBuffers) {
		t.Fatalf("err = %v, want ErrNoUnpinnedBuffers", err)
	}
	if firstItem(t, a.Page()) != 0 || firstItem(t, b.Page()) != 1 {
		t.Fatal("pinned frame was reused")
	}
	pool.Unpin(b)
	c, err := pool.Pin(rel, 2)
	if err != nil {
		t.Fatal(err)
	}
	if c != b {
		t.Fatal("did not reuse the unpinned frame")
	}
	if firstItem(t, a.Page()) != 0 {
		t.Fatal("pinned frame was reused")
	}
	pool.Unpin(a)
	pool.Unpin(c)
}

func TestFlush(t *testing.T) {
	pool, store := setup(t, 4, 2)
	a, _ := pool.Pin(rel, 0)
	b, _ := pool.Pin(rel, 1)
	a.Page().AddItem([]byte("a"))
	a.MarkDirty()
	b.Page().AddItem([]byte("b"))
	b.MarkDirty()

	if err := pool.Flush(a); err != nil {
		t.Fatal(err)
	}
	if s := pool.Stats(); s.Writes != 1 {
		t.Fatalf("Writes after Flush = %d", s.Writes)
	}
	if got, _ := diskPage(t, store, 0).GetItem(2); string(got) != "a" {
		t.Fatal("block 0 not on disk after Flush")
	}
	if _, err := diskPage(t, store, 1).GetItem(2); err == nil {
		t.Fatal("block 1 on disk before FlushAll")
	}
	if err := pool.Flush(a); err != nil {
		t.Fatal(err)
	}
	if s := pool.Stats(); s.Writes != 1 {
		t.Fatalf("Flush of a clean buffer wrote: %d", s.Writes)
	}
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if s := pool.Stats(); s.Writes != 2 {
		t.Fatalf("Writes after FlushAll = %d", s.Writes)
	}
	if got, _ := diskPage(t, store, 1).GetItem(2); string(got) != "b" {
		t.Fatal("block 1 not on disk after FlushAll")
	}
	pool.Unpin(a)
	pool.Unpin(b)
	// Nothing dirty: evictions do not write.
	for blk := tuple.BlockNumber(0); blk < 2; blk++ {
		pool.Discard(rel)
	}
	if s := pool.Stats(); s.Writes != 2 {
		t.Fatalf("Writes after discard = %d", s.Writes)
	}
}

func TestExtend(t *testing.T) {
	pool, store := setup(t, 2, 1)
	b, err := pool.Extend(rel)
	if err != nil {
		t.Fatal(err)
	}
	if b.Block() != 1 || b.Rel() != rel {
		t.Fatalf("Extend tag = %d/%d", b.Rel(), b.Block())
	}
	if !b.Page().IsNew() {
		t.Fatal("extended page is not zeroed")
	}
	if n, _ := store.NBlocks(rel); n != 2 {
		t.Fatalf("NBlocks after Extend = %d", n)
	}
	b.Page().Init(0)
	b.Page().AddItem([]byte("new"))
	b.MarkDirty()
	pool.Unpin(b)
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if got, _ := diskPage(t, store, 1).GetItem(1); string(got) != "new" {
		t.Fatalf("disk block 1 = %q", got)
	}
	// The new page is cached: pinning it is a hit.
	before := pool.Stats()
	c, _ := pool.Pin(rel, 1)
	pool.Unpin(c)
	if s := pool.Stats(); s.Hits != before.Hits+1 {
		t.Fatalf("Pin after Extend was not a hit: %+v", s)
	}
}

func TestUnpinTooManyPanics(t *testing.T) {
	pool, _ := setup(t, 2, 1)
	b, _ := pool.Pin(rel, 0)
	pool.Unpin(b)
	defer func() {
		if recover() == nil {
			t.Fatal("Unpin below zero did not panic")
		}
	}()
	pool.Unpin(b)
}

func TestDiscard(t *testing.T) {
	pool, store := setup(t, 4, 2)
	store.Create(rel + 1)
	p := page.New(0)
	store.Extend(rel+1, p)

	a, _ := pool.Pin(rel, 0)
	a.Page().AddItem([]byte("lost"))
	a.MarkDirty()
	pool.Unpin(a)
	o, _ := pool.Pin(rel+1, 0)
	pool.Unpin(o)

	pool.Discard(rel)
	if s := pool.Stats(); s.Writes != 0 {
		t.Fatalf("Discard wrote: %+v", s)
	}
	if _, err := diskPage(t, store, 0).GetItem(2); err == nil {
		t.Fatal("discarded change reached disk")
	}
	before := pool.Stats()
	a2, _ := pool.Pin(rel, 0)
	if s := pool.Stats(); s.Misses != before.Misses+1 {
		t.Fatal("Pin after Discard was not a miss")
	}
	if _, err := a2.Page().GetItem(2); err == nil {
		t.Fatal("discarded change survived in memory")
	}
	pool.Unpin(a2)
	o2, _ := pool.Pin(rel+1, 0)
	if o2 != o {
		t.Fatal("Discard dropped another relation's frame")
	}
	pool.Unpin(o2)
}

func TestConcurrentPins(t *testing.T) {
	const nblocks, nframes, workers, ops = 16, 4, 8, 300
	pool, store := setup(t, nframes, nblocks)

	// Each page carries a counter in item 2.
	for blk := tuple.BlockNumber(0); blk < nblocks; blk++ {
		b, err := pool.Pin(rel, blk)
		if err != nil {
			t.Fatal(err)
		}
		b.Page().AddItem(make([]byte, 8))
		b.MarkDirty()
		pool.Unpin(b)
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < ops; i++ {
				blk := tuple.BlockNumber(rng.Intn(nblocks))
				b, err := pool.Pin(rel, blk)
				if errors.Is(err, ErrNoUnpinnedBuffers) {
					i--
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if b.Block() != blk {
					t.Errorf("pinned %d, got %d", blk, b.Block())
				}
				b.Lock()
				item, _ := b.Page().GetItem(2)
				binary.LittleEndian.PutUint64(item, binary.LittleEndian.Uint64(item)+1)
				b.MarkDirty()
				b.Unlock()
				pool.Unpin(b)
			}
		}(int64(w))
	}
	wg.Wait()
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	var total uint64
	for blk := tuple.BlockNumber(0); blk < nblocks; blk++ {
		p := diskPage(t, store, blk)
		item, err := p.GetItem(2)
		if err != nil {
			t.Fatal(err)
		}
		if firstItem(t, p) != byte(blk) {
			t.Fatalf("block %d has another block's content", blk)
		}
		total += binary.LittleEndian.Uint64(item)
	}
	if total != workers*ops {
		t.Fatalf("total increments on disk = %d, want %d", total, workers*ops)
	}
	if s := pool.Stats(); s.Evictions == 0 {
		t.Fatalf("expected evictions with %d frames for %d blocks: %+v", nframes, nblocks, s)
	}
}

func TestPageAliasesFrame(t *testing.T) {
	pool, _ := setup(t, 2, 1)
	b, _ := pool.Pin(rel, 0)
	p1 := b.Page()
	p2 := b.Page()
	p1.AddItem([]byte("x"))
	if !bytes.Equal(p1, p2) {
		t.Fatal("Page() returned a copy")
	}
	pool.Unpin(b)
}
