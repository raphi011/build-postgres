# Chapter 04: Buffer manager

**Goal.** A fixed pool of page frames between callers and the storage
manager: pages are pinned in memory, looked up by `(relation, block)`,
evicted by clock sweep, and written back only when dirty.

**You edit.** `internal/bufmgr/bufmgr.go`: 13 functions with
`panic("not implemented")` bodies, `Rel` through `Stats`. Nothing else
changes; `bufmgr_test.go` is the contract.

**Needs from earlier chapters.** `page` (chapter 01) for `page.Page` and
`page.PageSize`, `tuple` (chapter 02) for `OID` and `BlockNumber`, and
`smgr.DataDir` (chapter 03) for `Read`, `Write`, `Extend` and `NBlocks`.

**Done when.** `make test-ch04` passes.

**Effort.** Medium, about 4 hours. The clock sweep inside `Pin` (step 1)
is the hard part and the rest follows from it.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser (07) ──► Analyzer (09) ──► Planner (14, 15)
                                     │                 │
                              Catalog (08)             ▼
                                     │           Executor (10, 11)
                                     │                 │
                                     ▼                 ▼
                              Heap access method (05)   B-tree (12, 13)
                                     │                 │
                                     └───────┬─────────┘
                                             ▼
                              Transactions and MVCC (16, 17)
                                             │
                                             ▼
                                   Buffer manager [04]
                                             │
                                             ▼
                                   Storage manager (03)
                                             │
                                             ▼
                              Pages (01) and tuples (02) on disk
```

Reading a page from disk for every tuple access would make everything slow,
and writing it back after every change would make it slower. PostgreSQL
keeps a fixed pool of page-sized frames in shared memory, the buffer pool,
and every access to a page goes through it. This chapter builds that pool.

PostgreSQL source: `storage/buffer/bufmgr.c`, `storage/buffer/freelist.c`,
`storage/buffer/buf_table.c`, `include/storage/buf_internals.h`, and the
`README` in `storage/buffer/`.

## What a buffer is

A buffer is one frame holding one page of one relation, plus bookkeeping:

- the tag `(relation, block number)` that says which page it holds;
- a pin count: how many callers are currently using it;
- a dirty flag: the frame differs from the disk copy;
- a usage count for the replacement policy;
- a content lock that callers take to read or modify the page.

The pool maintains a hash table from tag to frame so that a second request
for the same page finds the copy already in memory.

## Pinning

A caller that wants a page calls `Pin`. On a hit the pin count goes up and
the frame is returned. On a miss the pool picks a frame to reuse, writes it
out if dirty, reads the wanted page into it, and returns it pinned. While a
buffer is pinned it cannot be evicted, so the `*Buffer` and its page slice
stay valid. When the caller is done it calls `Unpin`. After that the pointer
must not be used: the frame may be given to another page at any time.

Pinning is separate from locking. A pin only guarantees the frame stays
put. Callers that read a page take `RLock`, callers that modify it take
`Lock` and call `MarkDirty`. Chapters 05 through 16 have a single writer,
but the buffer pool itself is fully concurrent from now on, and the
chapter 17 tests will drive many sessions through it.

Worked example. One frame, block 0, in a pool with room to spare:

| Call | Hit or miss | Pins after | Usage after | Notes |
|---|---|---|---|---|
| `Pin(rel, 0)` | miss | 1 | 1 | frame taken, page read from disk |
| `Pin(rel, 0)` | hit | 2 | 2 | same `*Buffer` returned |
| `Unpin(b)` | – | 1 | 2 | usage is not touched |
| `Unpin(b)` | – | 0 | 2 | page stays cached and evictable |
| `Pin(rel, 0)` | hit | 1 | 3 | still resident, so still a hit |
| `Unpin(b)` | – | 0 | 3 | |
| `Unpin(b)` | – | – | – | panics: pin count already zero |

`Unpin` moves the pin count and nothing else. The frame keeps its page,
its tag, its dirty flag and its usage count, which is why the pin after
two unpins is still a hit. Only the sweep clears a frame. Unpinning once
too often is a bug in the caller, not a recoverable condition, so it
panics rather than returning an error.

## Clock sweep

**Predict.** Three frames hold blocks 0, 1 and 2. Block 0 has been pinned
five times, blocks 1 and 2 once each. A pin of block 3 needs a frame.
Which block is evicted, and where does the hand stop?

When every frame is in use the pool must evict one. PostgreSQL uses an
approximation of LRU called clock sweep: a hand moves around the frames in
a circle. Each pin bumps the frame's usage count (capped at 5). The hand
decrements the count of each unpinned frame it passes and evicts the first
one it finds at zero. Pages that are pinned often survive several passes;
pages touched once are evicted on the next.

The tests depend on the exact algorithm:

1. If a frame has never been used, take the lowest-numbered such frame.
2. Otherwise, starting at the hand: if the frame is pinned, skip it. If its
   usage count is above zero, decrement and skip. Otherwise it is the
   victim. Advance the hand past the victim.
3. If every frame is pinned, return `ErrNoUnpinnedBuffers` without changing
   any usage count. Detect this by having skipped `NFrames` pinned frames
   in a row.

The hand points at the frame it will inspect next and moves in a circle.
One pass over four frames, evicting the third:

```
      hand
       │
       ▼
    ┌─────────┐   ┌─────────┐   ┌─────────┐   ┌─────────┐
 ┌─►│ frame 0 │──►│ frame 1 │──►│ frame 2 │──►│ frame 3 │──┐
 │  │ pins  1 │   │ pins  0 │   │ pins  0 │   │ pins  0 │  │
 │  │ usage 3 │   │ usage 2 │   │ usage 0 │   │ usage 1 │  │
 │  └─────────┘   └─────────┘   └─────────┘   └─────────┘  │
 └─────────────────────────────────────────────────────────┘
      pinned:       usage 2:      usage 0:      not reached
      skip, no      decrement     victim;
      decrement     to 1, skip    hand ends
                                  at frame 3
```

A newly loaded page starts with usage count 1. A hit increments it.

Worked example. A pool of three frames over a four-block relation, which
is `TestClockSweepEvictsUsageZero`. Pin and unpin block 0 five times,
then block 1 once and block 2 once. Every frame has now been used, so
`nused` is 3 and the hand is at frame 0:

| Frame | Holds | Pins | Usage |
|---|---|---|---|
| 0 | block 0 | 0 | 5 |
| 1 | block 1 | 0 | 1 |
| 2 | block 2 | 0 | 1 |

Pinning block 3 is a miss and runs the sweep. Each row is one turn of the
loop; the hand advances before the frame is inspected, so "hand after" is
already past the frame in the same row:

| Turn | Hand | Frame | Holds | Usage before | Action | Usage after | Hand after |
|---|---|---|---|---|---|---|---|
| 1 | 0 | 0 | block 0 | 5 | decrement, skip | 4 | 1 |
| 2 | 1 | 1 | block 1 | 1 | decrement, skip | 0 | 2 |
| 3 | 2 | 2 | block 2 | 1 | decrement, skip | 0 | 0 |
| 4 | 0 | 0 | block 0 | 4 | decrement, skip | 3 | 1 |
| 5 | 1 | 1 | block 1 | 0 | **evict** | – | 2 |

Block 1 is the victim, the hand ends at 2, and frame 1 is reloaded with
block 3 at usage 1. Block 0 survived two decrements because it was pinned
five times; that is the whole point of the usage count. Between blocks 1
and 2, which are equally cold, the tie is broken by where the hand
happened to be. The pool afterwards:

| Frame | Holds | Usage |
|---|---|---|
| 0 | block 0 | 3 |
| 1 | block 3 | 1 |
| 2 | block 2 | 0 |

So the next pins of blocks 0, 2 and 3 are hits and the next pin of block
1 is a miss, which is exactly what the test asserts. Note that two full
turns of the circle were needed for one eviction; a sweep does not stop
after `NFrames` inspections, only after `NFrames` *pinned* frames in a
row.

## Writing back

A dirty frame is written when it is evicted, when `Flush` is called on it,
or when `FlushAll` runs. Writing clears the dirty flag. A clean frame is
never written. Nothing here calls `Sync`; ordering writes against the
write-ahead log is v2's concern, which is why the page LSN exists.

## API

`internal/bufmgr`

```go
type Buffer struct{ ... }
func (b *Buffer) Rel() tuple.OID
func (b *Buffer) Block() tuple.BlockNumber
func (b *Buffer) Page() page.Page       // aliases the frame
func (b *Buffer) MarkDirty()
func (b *Buffer) Lock() / Unlock() / RLock() / RUnlock()

type Pool struct{ ... }
func New(store *smgr.DataDir, nframes int) *Pool
func (p *Pool) NFrames() int
func (p *Pool) Pin(rel tuple.OID, blk tuple.BlockNumber) (*Buffer, error)
func (p *Pool) Extend(rel tuple.OID) (*Buffer, error)
func (p *Pool) Unpin(b *Buffer)
func (p *Pool) Flush(b *Buffer) error
func (p *Pool) FlushAll() error
func (p *Pool) Discard(rel tuple.OID)
func (p *Pool) Stats() Stats           // Hits, Misses, Evictions, Writes
```

Semantics the tests depend on:

- `Pin` on a block past the end of the relation returns an error wrapping
  `smgr.ErrBlockOutOfRange` and leaves the pool unchanged: no frame is
  consumed, no count is bumped.
- `Extend` appends an all-zero page to the relation on disk and returns it
  pinned. The caller initialises it and marks it dirty. This is
  `ReadBuffer(rel, P_NEW)` in PostgreSQL.
- `Unpin` on a buffer with a zero pin count is a programming error and
  panics.
- `Discard` drops every frame belonging to a relation without writing,
  pinned or not. It exists for `DROP TABLE` (chapter 08); callers must not
  hold pins on that relation.
- `Stats` counts hits and misses in `Pin`, evictions of any frame that held
  a page, and every page write from any cause.

## Implementation notes

**Go you will need.** `sync.Mutex` with `defer p.mu.Unlock()`; it is not
reentrant, so a public method that holds it must call private helpers,
never another public method. A struct with comparable fields can be a map
key, which is how the tag becomes `map[tag]*Buffer`. `make(page.Page,
page.PageSize)` allocates a frame; the `clear` builtin zeroes it. `%` for
the wrap-around of the clock hand. Returning a struct by value copies it,
which is all a `Stats` snapshot needs.

**Private state.** A private `tag{rel, blk}` struct. `Buffer` adds a
back-pointer to its pool (`MarkDirty` needs the pool mutex), the tag, a
`valid` flag (holds a page), pin and usage counts, the dirty flag, and the
frame itself. `Pool` adds one `sync.Mutex` guarding all of that, the
`frames` slice, the `table` map, a `nused` count of frames that have ever
held a page, the clock `hand`, and the `Stats`. Three private helpers,
all called with the mutex held: `victim()`, `install(b, tag)`, and
`flush(b)`. PostgreSQL: `BufferDesc` in `buf_internals.h`, the tag table
in `buf_table.c`.

**New.** Allocate `nframes` buffers, each with its back-pointer and a
`page.PageSize` frame, and the empty map. A frame is allocated once and
never replaced; `Page` returns it as is, which is why the slice aliases
(`TestPageAliasesFrame`).

**MarkDirty.** Set the flag under the pool mutex, not the content lock:
`flush` reads it under the pool mutex.

**Pin.** Under the mutex, in this order:

1. Look the tag up. Hit: `Hits++`, `pins++`, `usage++` unless already
   `MaxUsageCount`, return.
2. Miss: call `NBlocks` and return `smgr.ErrBlockOutOfRange` if `blk` is
   past the end, before touching any frame or counter. `TestPinOutOfRange`
   checks that `Misses` stays zero and both frames remain usable.
3. `victim()`, then `store.Read` into its frame. A read error leaves the
   frame invalid so the next sweep reclaims it.
4. `Misses++`, `install`.

**victim.** The clock sweep section has the algorithm; what it does not
say: step 1 is `nused < NFrames`, take `frames[nused]` and increment. In
step 2 advance the hand *before* inspecting the frame, so it always ends
past the victim. A frame with `valid == false` (after `Discard` or a
failed load) is taken at once. A frame with pins resets nothing and bumps
a `pinned` counter; any unpinned frame resets that counter to zero, and
reaching `NFrames` returns `ErrNoUnpinnedBuffers`. The victim is
`flush`ed, deleted from the table, marked invalid, and `Evictions++`; free
frames from step 1 are not evictions (`TestClockSweepFreeFramesFirst`).
PostgreSQL: `StrategyGetBuffer` in `freelist.c`.

**install.** Set the tag, `valid`, `pins = 1`, `usage = 1`, `dirty =
false`, and add the table entry. Both `Pin` and `Extend` end here.

**Unpin.** Under the mutex, panic if `pins` is already zero, else
decrement. Do not touch the table or the usage count: the page stays
cached and the next `Pin` is a hit (`TestPinHitAndMiss`).

**Flush and FlushAll.** `flush(b)` returns at once when clean; otherwise
`store.Write`, `Writes++`, clear the flag. `Flush` is the mutex plus
`flush`. `FlushAll` walks `frames`, skips invalid ones, and returns the
first error. Writes happen under the pool mutex without the content lock;
that is safe because eviction only picks unpinned frames and every writer
holds a pin (`TestConcurrentPins`).

**Extend.** `victim()` first, then `clear` the frame and pass it to
`store.Extend`, which returns the new block number for the tag. A store
error leaves the frame invalid. `install` as `Pin` does, but count neither
a hit nor a miss. `TestExtend` checks `IsNew` on the returned page, the
disk block count, and that the next `Pin` of that block is a hit.

**Discard.** Walk `frames`; for each valid one whose tag names `rel`,
delete the table entry and reset `valid`, `pins`, and `dirty`. Nothing is
written. The sweep takes invalid frames first, so the freed frames are
reused before any eviction.

## Suggested order

One step at a time with the line under it; `make test-ch04` at the end.
Every test not named in that step or an earlier one still panics.

1. Pool and pins. `New`, `NFrames`, `Stats`, `Rel`, `Block`, `Page`,
   `MarkDirty`, `Pin`, `Unpin`. Green: `TestPinHitAndMiss`,
   `TestPinOutOfRange`, `TestClockSweepFreeFramesFirst`,
   `TestUnpinTooManyPanics`, `TestPageAliasesFrame`, which need hits and
   free frames only; then, once the sweep exists,
   `TestClockSweepEvictsUsageZero` (traces every decrement, so follow the
   README's algorithm literally), `TestPinnedNeverEvicted` (expects
   `ErrNoUnpinnedBuffers` with usage counts untouched) and
   `TestDirtyEvictionWritesBack` (counts `Writes` and `Evictions`
   separately). Every test pins, so nothing is green before this step.

   ```sh
   go test -race ./internal/bufmgr/... -run 'TestPinHitAndMiss|TestPinOutOfRange|TestClockSweepFreeFramesFirst|TestUnpinTooManyPanics|TestPageAliasesFrame|TestClockSweepEvictsUsageZero|TestPinnedNeverEvicted|TestDirtyEvictionWritesBack'
   ```
2. Write-back. `Flush`, `FlushAll`. Green: `TestConcurrentPins`, which
   runs many goroutines that pin, lock, modify, unpin random pages through
   a pool far smaller than the working set, then flushes and checks that
   every increment survived. It is the reason for `-race`.

   ```sh
   go test -race ./internal/bufmgr/... -run 'TestConcurrentPins'
   ```
3. Extend. `Extend`. Green: `TestExtend`; see Implementation notes for
   what it checks.

   ```sh
   go test -race ./internal/bufmgr/... -run 'TestExtend'
   ```
4. Discard. `Discard`. Green: `TestFlush` (it ends by discarding and checks
   that no write happened), `TestDiscard`.

   ```sh
   go test -race ./internal/bufmgr/... -run 'TestFlush|TestDiscard'
   ```

### When a test fails

- `TestClockSweepEvictsUsageZero` — block 2 is evicted instead of block
  1, or the eviction happens one turn early. Advance the hand *before*
  inspecting the frame, so that after the victim is chosen the hand
  already points past it. Advancing afterwards leaves the hand on the
  frame you just reused and shifts every later victim by one.
- `TestClockSweepEvictsUsageZero` — nothing is ever evicted and the sweep
  spins. The usage count must be decremented on every unpinned frame the
  hand passes, and a frame at zero must be taken rather than decremented
  again. Both branches have to be in the loop for it to terminate.
- `TestClockSweepFreeFramesFirst` — `Evictions` is 3 instead of 0 on a
  pool that was never full. Frames that have never held a page are taken
  by index through the `nused` counter and are not evictions; only a
  frame whose page is being thrown away counts.
- `TestClockSweepFreeFramesFirst` — two blocks land in the same frame.
  `nused` has to increment every time a fresh frame is handed out, and
  it is the index of the next unused frame, not a count of live pages.
- `TestPinnedNeverEvicted` — the sweep loops forever with every frame
  pinned, or it returns `ErrNoUnpinnedBuffers` but the usage counts have
  changed. Count consecutive pinned frames and give up at `NFrames`; a
  pinned frame is skipped without touching its usage count, and any
  unpinned frame resets that counter to zero.
- `TestPinOutOfRange` — `Misses` is 1, or a later pin finds the pool one
  frame short. Check `blk` against `NBlocks` before calling `victim`. An
  out-of-range pin must leave the pool exactly as it found it.
- `TestDirtyEvictionWritesBack` — `Writes` is 0 and the disk page still
  holds the old bytes. `MarkDirty` has to set the flag under the pool
  mutex, because `flush` reads it under that mutex; setting it under the
  content lock instead means the evicting goroutine can read a stale
  `false`.
- `TestDirtyEvictionWritesBack` — the page is written but `Evictions` and
  `Writes` are both 1 on a clean eviction too. `flush` returns
  immediately when the frame is clean and only then counts a write.
- `TestExtend` — the returned page is not `IsNew`, or it holds the bytes
  of whatever was in the frame before. `clear` the frame before handing
  it to `store.Extend`; a recycled frame arrives full of the evicted
  page.
- `TestFlush` — the final `Discard` writes the dirty page out. `Discard`
  throws frames away without writing them; that is what makes it right
  for `DROP TABLE` and wrong for anything else.
- `TestDiscard` — a frame freed by `Discard` is skipped by the sweep and
  an eviction happens instead. Clearing `valid` is what marks the frame
  as free, and `victim` must take an invalid frame the moment the hand
  reaches it, before looking at pins or usage.
- `TestConcurrentPins` — a deadlock, usually on the first eviction. The
  pool mutex is not reentrant, so a public method that holds it must call
  the private `victim`, `install` and `flush` helpers and never another
  public method.
- `TestConcurrentPins` — the race detector reports a write to a frame
  during a write-back. Every writer holds a pin, and `victim` only picks
  unpinned frames, so the invariant holds only if the pin count is
  decremented after the last use of the page, not before.

## Why not LRU

True LRU keeps the frames in a list ordered by last access, so every hit has
to move a node to the front: the hot path takes a lock and writes to a
shared list, and the list becomes the contended structure in a pool that
many backends share. Clock sweep approximates the same answer with one
counter per frame and a hand, so a hit increments an integer in the frame
the caller already holds. We take PostgreSQL's choice because it is
PostgreSQL's (D4), and because this is the chapter where concurrency starts
(D9). The approximation is visible: between two equally cold pages the
victim is whichever the hand reaches first, which is why the worked
example's tie between blocks 1 and 2 is broken by position and not by age.

## Out of scope

The background writer, checkpointer, buffer access strategies (ring
buffers), local buffers for temporary tables, the free-list spinlock
partitioning, I/O without holding the pool lock (we read and write under
the lock, which is simple and correct but serialises I/O).

## Check your understanding

1. Four frames hold blocks 0 to 3 with usage counts 2, 0, 3, 1, the hand
   is at frame 2, and nothing is pinned. A pin of block 4 arrives. Which
   block is evicted, and what are the usage counts afterwards?
   <details><summary>Answer</summary>

   Block 1. The hand inspects frame 2 (usage 3 to 2), frame 3 (1 to 0),
   frame 0 (2 to 1), then frame 1, which is already at 0 and is the
   victim. Afterwards frame 0 has usage 1, frame 1 holds block 4 at usage
   1, frame 2 has 2 and frame 3 has 0. The hand ends at frame 2.
   </details>
2. A pool of two frames, both holding pinned pages. What does `Pin` of a
   third block return, and how many usage counts changed?
   <details><summary>Answer</summary>

   `ErrNoUnpinnedBuffers`, with no usage count changed at all. The sweep
   skips a pinned frame without decrementing it and counts how many
   pinned frames it has seen in a row; at `NFrames` it gives up. Callers
   see this as "the working set does not fit in the pool", which is a
   real condition in PostgreSQL too.
   </details>
3. Why does `Unpin` leave the usage count and the table entry alone,
   rather than treating the last unpin as "done with this page"?
   <details><summary>Answer</summary>

   Because the pool's whole purpose is that the page stays in memory
   after the caller is finished with it, so the next request for it is a
   hit. A pin says "do not move this frame while I am looking at it",
   not "I want this page cached". Eviction is decided by the sweep, on
   the basis of how often the page has been wanted, and an unpinned frame
   is merely a candidate.
   </details>
4. `Pin` returns a `*Buffer` whose `Page()` aliases the frame, and the
   contract says the pointer must not be used after `Unpin`. Why not
   return a copy of the page and avoid the rule?
   <details><summary>Answer</summary>

   A copy costs 8 KiB per access and, worse, makes writes invisible: a
   caller that modifies its copy has changed nothing, so every writer
   would need a write-back call and the pool could not tell which frames
   are dirty. Aliasing is what makes `MarkDirty` meaningful. The price is
   the discipline that the alias is valid exactly as long as the pin,
   which is also PostgreSQL's rule.
   </details>
5. We do our reads and writes while holding the pool mutex. PostgreSQL
   releases its equivalent lock around the I/O and marks the buffer as
   in-progress instead. What does that buy, and why can we skip it?
   <details><summary>Answer</summary>

   It stops one slow disk read from blocking every other backend's buffer
   lookups, which on a real server is the difference between one I/O at a
   time and many in flight. The cost is a whole extra state: a buffer
   that is being read is neither valid nor free, other backends have to
   wait on it rather than re-issue the read, and an I/O error has to be
   propagated to everyone waiting. Our tests use temporary files that are
   effectively in the page cache, so the serialisation costs nothing
   measurable, and the simpler invariant is worth more here (D4).
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **LRU anyway.** Implement it behind `Pin` and compare the two on a
   workload that repeats a small hot set inside a large scan. Count hits
   with the `Stats` you already have.
2. **I/O outside the lock.** Mark a frame as under I/O, release the pool
   lock, do the read, then re-take it, with other pins on that frame waiting
   for the flag. This is the one thing our pool does that PostgreSQL would
   not, and the race detector is the judge.
3. **Read `StrategyGetBuffer` in `freelist.c`.** Find the ring buffer a
   large sequential scan uses (`BufferAccessStrategy`) and work out what
   would go wrong in our pool when a scan of a table larger than the pool
   runs next to a busy index.
