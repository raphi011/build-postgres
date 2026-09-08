# Chapter 05: Heap access method

**Goal.** Insert, fetch, delete, update, and scan tuples in a relation
through the buffer manager, addressing each one by its TID.

**You edit.** `internal/heap/heap.go`: 13 functions with
`panic("not implemented")` bodies, `Create` through `Close`. Nothing else
changes; `heap_test.go` is the contract.

**Needs from earlier chapters.** `internal/bufmgr` (chapter 04) to pin,
lock, and extend pages; `internal/page` (chapter 01) to store items;
`internal/tuple` (chapter 02) for the header fields `Insert` stamps. The
pool's store reports `smgr.ErrExists` (chapter 03) on a second `Create`.

**Done when.** `make test-ch05` passes.

**Effort.** Medium, about 3 hours. `Update` (step 4) is the hard part: it
stamps the old version and inserts the new one, possibly on another page,
and both pages have to be pinned and dirtied correctly.

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
                              Heap access method [05]   B-tree (12, 13)
                                     │                 │
                                     └───────┬─────────┘
                                             ▼
                              Transactions and MVCC (16, 17)
                                             │
                                             ▼
                                   Buffer manager (04)
                                             │
                                             ▼
                                   Storage manager (03)
                                             │
                                             ▼
                              Pages (01) and tuples (02) on disk
```

A table in PostgreSQL is a heap: an unordered pile of tuples spread over the
pages of one relation file. The heap access method is the code that knows
how to put a tuple into a page, find it again by its TID, mark it deleted,
and walk every tuple in the relation. With this chapter, the storage layer
is complete enough to hold real tables.

PostgreSQL source: `access/heap/heapam.c` (`heap_insert`, `heap_delete`,
`heap_update`, `heap_fetch`, `heap_getnext`), `access/heap/hio.c`
(`RelationGetBufferForTuple`), `access/heap/heapam_handler.c`.

## Tuple identity

A tuple's address is its TID: block number plus item number on that block.
It is what indexes store (chapter 12) and what `ctid` in the tuple header
holds. Because the page never renumbers items (chapter 01), a TID stays
valid until the tuple is gone for good.

## Where does an insert go?

**Predict.** A relation has one page holding 157 rows and no room for a
158th. Where does the next insert go, and what happens to the space
freed by a row deleted from page 0 an hour ago?

PostgreSQL asks the free space map for a page with room, and falls back to
extending the relation. The free space map is a separate fork of the
relation and is out of scope, so our rule is the one PostgreSQL uses when
the map has nothing to say: remember the block the last insert went to
(`RelationGetTargetBlock`), try it, and if the tuple does not fit, extend
the relation with a new page. Space freed by deletions on earlier pages is
not reused until VACUUM exists (v2). That matches PostgreSQL: a deleted
tuple stays on the page until vacuum removes it, because other
transactions may still need to see it (chapter 17).

Worked example. The descriptor is `(id int4, name text)` and every row is
`(n, "some text")`, which `Form` turns into 41 bytes. On a page that
costs `align8(41) = 48` bytes of item data plus a 4-byte line pointer, so
52 bytes out of the 8168 between the header and the end of the page:
`8168 / 52 = 157` rows per page.

| Insert | Target block | Fits? | TID returned | Blocks after |
|---|---|---|---|---|
| row 1 | none yet, extend | – | `(0, 1)` | 1 |
| row 2 | 0 | yes | `(0, 2)` | 1 |
| row 3 | 0 | yes | `(0, 3)` | 1 |
| ... | 0 | yes | ... | 1 |
| row 157 | 0 | yes | `(0, 157)` | 1 |
| row 158 | 0 | **no**, extend | `(1, 1)` | 2 |

That is the count `TestInsertFillsPagesBeforeExtending` pins: the second
block appears on the 158th row, not earlier. An implementation that
extends whenever the page looks nearly full, or that consults `NBlocks`
instead of remembering the target block, gets a different number here.

Deleting row 3 afterwards frees nothing that a later insert can use.
The line pointer stays, the bytes stay, and the target block has already
moved on to block 1. Only VACUUM would reclaim it, and that is v2.

## Delete and update

Deleting a tuple does not remove it. It stamps `xmax` with the deleting
transaction and leaves the bytes in place. Readers decide whether they can
see it. Until chapter 17 the rule is: a tuple is visible if and only if
`xmax` is zero.

An update is a delete of the old version plus an insert of the new one.
The old version's `ctid` is pointed at the new version, so a reader that
arrives at the old one via an index or a stale TID can follow the chain.
A freshly inserted tuple's `ctid` points at itself.

Worked example. Insert two rows as transaction 10, then update the first
as transaction 11. `Update` returns `(0, 3)`, the TID of the new version:

| TID | id, name | xmin | xmax | ctid | Visible |
|---|---|---|---|---|---|
| `(0, 1)` | 1, `"alpha"` | 10 | 11 | `(0, 3)` | no |
| `(0, 2)` | 2, `"beta"` | 10 | 0 | `(0, 2)` | yes |
| `(0, 3)` | 1, `"ALPHA"` | 11 | 0 | `(0, 3)` | yes |

Three rows are on the page and a scan returns two of them, `(0, 2)` and
`(0, 3)`, in item order. The old version is still there in full: its
bytes were never touched, only its `xmax` was stamped and its `ctid`
redirected at the new version. A stale TID or an index entry that still
points at `(0, 1)` therefore finds a tuple, sees `xmax` set, and can
follow `ctid` forward. `Delete((0, 1), 12, nil)` now fails:

```
heap: tuple already deleted
```

A freshly inserted row points its `ctid` at itself, which is why
`(0, 2)` and `(0, 3)` read as they do.

Transaction IDs are parameters in this chapter and simply stored. Chapter
16 supplies real ones; chapter 17 makes readers consult them.

## Locking

Every page access pins the buffer and takes its content lock: shared for
reads, exclusive for changes. Locks are held only while touching the page,
never across a call that may pin another buffer. A scan copies the visible
tuples off a page while holding the shared lock and releases the buffer
before returning them, so callers never hold pins.

## API

`internal/heap`

```go
type Relation struct{ ... }

func Create(pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) (*Relation, error)
func Open(pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) *Relation
func (r *Relation) OID() tuple.OID
func (r *Relation) Desc() *tuple.Desc
func (r *Relation) NBlocks() (tuple.BlockNumber, error)

func (r *Relation) Insert(t tuple.Tuple, xid tuple.XID) (tuple.TID, error)
func (r *Relation) Fetch(tid tuple.TID) (tuple.Tuple, error)
func (r *Relation) Delete(tid tuple.TID, xid tuple.XID) error
func (r *Relation) Update(tid tuple.TID, t tuple.Tuple, xid tuple.XID) (tuple.TID, error)

func (r *Relation) Scan() *Scan
func (s *Scan) Next() bool
func (s *Scan) TID() tuple.TID
func (s *Scan) Tuple() tuple.Tuple
func (s *Scan) Err() error
func (s *Scan) Close()
```

Semantics the tests depend on:

- `Create` makes the relation file through the pool's store and fails with
  `smgr.ErrExists` if it is there already. `Open` does no I/O.
- `Insert` copies the tuple, stamps `xmin`, clears `xmax` and the hint
  bits, sets `ctid` to the tuple's own TID, and returns that TID. A tuple
  longer than `page.MaxItemSize` is `ErrTupleTooLarge` and leaves the
  relation unchanged. Heap pages have no special space.
- `Fetch` returns a copy of the tuple at `tid`, deleted or not. A block
  past the end, an item number of zero or past the page's line pointers, or
  an unused line pointer is `ErrNotFound`.
- `Delete` stamps `xmax`. A tuple that already has a non-zero `xmax` is
  `ErrAlreadyDeleted`. A missing tuple is `ErrNotFound`.
- `Update` is `Delete` then `Insert`, and additionally points the old
  version's `ctid` at the new TID. It returns the new TID.
- `Scan` visits blocks in order and tuples in item order, skipping unused
  line pointers and tuples with a non-zero `xmax`. The number of blocks is
  read once when the scan starts. `Tuple` returns a copy; modifying it does
  not change the relation. After `Next` returns false, `Err` reports the
  first error, if any.

## Implementation notes

**Go you will need.** `append(tuple.Tuple(nil), t...)` copies a slice into
fresh memory; `page.GetItem` aliases the page, so every tuple handed out
must be copied while the lock is still held. `sync.Mutex` with `defer
r.mu.Unlock()`. Deferred calls run last-in-first-out, so `defer Unpin`
followed by `defer Unlock` unlocks before it unpins. `(n+7)&^7` rounds up
to a multiple of 8 (`&^` is AND NOT). A `*tuple.TID` parameter that may
be nil stands in for an optional argument. `s.buffer[:0]` reuses a
slice's backing array.

**Relation.** Besides pool, OID and descriptor, keep a `sync.Mutex` and a
`target` block number: the block the last insert went to,
`tuple.InvalidBlockNumber` on a fresh handle (`RelationGetTargetBlock`).
`Create` calls `pool.Store().Create(oid)` and returns `Open`; the store
supplies `smgr.ErrExists`. `NBlocks` is `pool.Store().NBlocks(oid)`.

**Insert.** `heap_insert`, with `heap_prepare_insert` for the header and
`RelationGetBufferForTuple` for the page choice.

1. Reject `len(t) > page.MaxItemSize` before any I/O.
2. Copy `t`; set xmin to `xid`, xmax to `tuple.InvalidXID`, and keep only
   `tuple.HasNull` in the infomask (PostgreSQL clears `HEAP_XACT_MASK`).
   The ctid is set once the TID is known.
3. Take the mutex. Pick the target: `target` if set, else the last block
   of the relation if there is one.
4. If there is a target, pin it, try to add the tuple (below), unpin. On
   success remember the target and return.
5. Otherwise `pool.Extend`, `Init(0)` the zeroed page under the exclusive
   lock (heap pages have no special space), add the tuple, and remember
   the new block.

A private `putTuple(buf, tup)` does the page work under the content lock:
compare `FreeSpace` with the tuple length rounded up to 8 (or let
`AddItem` fail), `AddItem`, then `GetItem` the stored copy and `SetCtid`
on it to `{buf.Block(), off}`, `MarkDirty`. It reports whether the tuple
fit. A tuple of exactly `MaxItemSize` fits on an empty page.

**Fetch.** A private `pinTuple(tid)` checks `tid.Block < NBlocks()` and
`tid.Off != page.InvalidOffsetNumber` (both `ErrNotFound`), then pins the
block and returns it unlocked. `Fetch` takes the shared lock, maps any
`GetItem` error (item number past `NumItems`, unused line pointer) to
`ErrNotFound`, and copies the item before unlocking.

**Delete and Update.** A private `markDeleted(tid, xid, ctid)` does the
shared work: `pinTuple`, exclusive lock, `GetItem` (error is
`ErrNotFound`), `ErrAlreadyDeleted` if xmax is non-zero, leaving the tuple
untouched; else `SetXmax`, `SetCtid` if `ctid` is not the zero TID,
`MarkDirty`. An `InvalidXID` asks it to check and write nothing.
`Update`:

1. Check `len(t)` against `page.MaxItemSize` first, so a failed update
   changes nothing.
2. `markDeleted` the old version with `InvalidXID`: check only.
3. `Insert` the new version with `xid`.
4. `markDeleted` the old version again, this time with `xid` and the new
   TID, so xmax and `ctid` are written under one lock.

No lock is held across the steps; `Insert` pins its own buffer. The
check in step 2 is what makes a failed insert harmless: stamping the old
version first and then failing to insert would leave a row deleted with
no replacement, and there is no undo. Chapter 17 puts its wait for
another transaction into `markDeleted`, so both step 2 and step 4 wait.

**Scan.** `heap_beginscan` and `heap_getnext`; the per-page copy is what
`heapgetpage` does (`heap_prepare_pagescan` since PostgreSQL 17). Private
state: `nblocks`, read once by `Scan` (an error there goes into `err`);
`next`, the next block to load; `buffer`, a slice of `{tid, tuple}` for
the visible tuples of the current block; `pos`, the index of the current
one; `err`; `done`. `Next`:

1. Return false if done or an error is set.
2. Increment `pos`. While `pos` is past the buffer: if `next == nblocks`,
   mark done and return false; else load block `next`, advance `next`,
   reset `pos` to 0.

A private `loadBlock(blk)` pins the block, takes the shared lock, walks
item numbers 1 through `NumItems`, skips `GetItem` errors (unused line
pointers) and non-zero xmax, and appends a copy of each survivor with its
TID. It unpins before returning, so nothing stays pinned between `Next`
calls; the 2-frame pool in `TestScanLeavesNothingPinned` catches a leaked
pin. `TID` and `Tuple` index the buffer at `pos`; `Close` sets done and
drops the buffer.

## Suggested order

One step at a time with the line under it; `make test-ch05` at the end.
Every test not named in that step or an earlier one still panics.

1. Relation and insert. `Create`, `Open`, `NBlocks`, `Insert`, `Fetch`.
   Green: `TestInsertFetch`, `TestInsertStampsHeaderAndCopies`,
   `TestTupleTooLarge`. See **Insert** above for the header reset and the
   size check.

   ```sh
   go test -race ./internal/heap/... -run 'TestInsertFetch|TestInsertStampsHeaderAndCopies|TestTupleTooLarge'
   ```
2. Scan. `Scan`, `Next`, `TID`, `Tuple`, `Err`, `Close`. Green:
   `TestCreateAndOpen`, `TestScanManyPages`,
   `TestInsertFillsPagesBeforeExtending`, `TestScanTupleIsCopy`,
   `TestScanLeavesNothingPinned`. The third counts 158 rows before the
   second block appears, so `Insert` has to fill the target block before
   extending; see **Scan** above for the pin discipline.

   ```sh
   go test -race ./internal/heap/... -run 'TestCreateAndOpen|TestScanManyPages|TestInsertFillsPagesBeforeExtending|TestScanTupleIsCopy|TestScanLeavesNothingPinned'
   ```
3. Delete. `Delete`. Green: `TestFetchErrors` (it also checks `Delete` on a
   missing TID), `TestDelete`, `TestScanSkipsFullyDeletedPages`,
   `TestPersistsAcrossReopen`, which flushes the pool and reads the file
   back through a fresh `Open`.

   ```sh
   go test -race ./internal/heap/... -run 'TestFetchErrors|TestDelete$|TestScanSkipsFullyDeletedPages|TestPersistsAcrossReopen'
   ```
4. Update. `Update`. Green: `TestUpdate`,
   `TestUpdateFailedInsertLeavesOldVersion`. See **Delete and Update**
   above; the second test pins the only frame of a one-frame pool and
   updates a row to a version that needs a page of its own, so the
   insert cannot extend the relation.

   ```sh
   go test -race ./internal/heap/... -run 'TestUpdate$|TestUpdateFailedInsertLeavesOldVersion'
   ```

The chapter's package also holds `TestScanSnapshot`, `TestFetchSnapshot`,
`TestDeleteSnapshot`, `TestUpdateSnapshot` and `TestLockSnapshot`. They
belong to chapter 17 and are not part of `make test-ch05`.

### When a test fails

- `TestInsertStampsHeaderAndCopies` — the caller's tuple comes back
  modified, or the stored row keeps a hint bit the caller set. `Insert`
  copies the tuple and then resets the header: `xmin` to `xid`, `xmax` to
  `InvalidXID`, and the infomask down to `tuple.HasNull` alone. Keeping
  the caller's other infomask bits is how a stale `XminCommitted` gets
  onto a fresh row and misleads chapter 17.
- `TestInsertFetch` — the row is stored but its `ctid` is zero. The TID
  is not known until `AddItem` returns, so `SetCtid` happens on the copy
  *on the page*, through `GetItem`, not on the local buffer you formed
  before the insert.
- `TestTupleTooLarge` — the relation gained a block. Check
  `len(t) > page.MaxItemSize` before any pin or extend.
- `TestInsertFillsPagesBeforeExtending` — the second block appears after
  something other than 157 rows. Either the target block is not being
  remembered between inserts, so every insert re-derives it, or the space
  test is not the one `AddItem` uses. Let `AddItem` decide and extend only
  when it returns `ErrNoSpace`.
- `TestScanLeavesNothingPinned` — `ErrNoUnpinnedBuffers` out of a
  two-frame pool. The scan copies a whole block's visible tuples while
  holding the shared lock and unpins before returning from `Next`.
  Holding the buffer across `Next` calls leaks a pin per block.
- `TestScanTupleIsCopy` — the returned tuple changes when the page does.
  `page.GetItem` aliases the frame, so each survivor must be copied while
  the content lock is still held.
- `TestScanManyPages` — rows from the last block are missing, or the scan
  runs past the end. The block count is read once when the scan starts;
  a scan must not see rows inserted after it began.
- `TestScanSkipsFullyDeletedPages` — the scan stops at the first page
  where nothing is visible. An empty buffer for one block is not the end
  of the scan; the loop has to keep loading blocks until `next` reaches
  the block count.
- `TestDelete` — the second delete of the same TID succeeds. A non-zero
  `xmax` is `ErrAlreadyDeleted`, and the check comes before any write, so
  a rejected delete leaves the tuple exactly as it was.
- `TestFetchErrors` — a block past the end panics or returns a zero
  tuple. Range-check `tid.Block` against `NBlocks` and treat item number
  zero, an item past `NumItems`, and an unused line pointer all as
  `ErrNotFound`.
- `TestUpdate` — the new version is written but the old one's `ctid`
  still points at itself. `Update` locks the old page after the insert to
  stamp both `xmax` and `ctid`, because the new TID does not exist until
  the insert returns.
- `TestUpdate` — a deadlock or a nested pin. No lock may be held across
  `Insert`, which pins a buffer of its own; check the old version, drop
  everything, insert, then take the old page again to stamp it.
- `TestUpdateFailedInsertLeavesOldVersion` — the old row comes back with
  a non-zero `xmax` after an insert that could not get a buffer. The
  check has to run before the insert and write nothing, and the stamp
  after it.
- `TestPersistsAcrossReopen` — rows are missing after the reopen. The
  test flushes the pool and opens a fresh handle, so anything still only
  in a dirty frame is lost; every page change needs its `MarkDirty`.

`go run ./cmd/pgdb dump FILE` prints the tuple headers this chapter
writes, so an `Update` is visible as the old version's `xmax` and its
`ctid` pointing at the new one, on the same page or a later block. From
chapter 11 you can point it at the files the REPL creates.

## Why not decide visibility here

`Insert`, `Delete` and `Update` take a transaction ID and store it without
ever asking what it means, and `Scan` returns every tuple on the page.
Deciding here whether a tuple is visible needs the commit log (chapter 16)
and a snapshot (chapter 17); writing half that rule now would mean writing
it twice, once wrong. So the heap takes an explicit XID from this chapter on
and its signatures do not change when the meaning arrives (D13). Until then
a deleted tuple is one with a non-zero `xmax`, the tests say exactly that,
and chapter 17 adds a snapshot argument rather than rewriting the scan.

## Out of scope

The free space map, heap-only tuples (HOT), pruning, `heap_lock_tuple`,
bulk insert, the visibility map, TOAST, and anything about transactions
beyond storing the IDs you are given.

## Check your understanding

1. The rows in the worked example are 41 bytes. How many fit on a page,
   and how does the number change if the `name` column holds 40 bytes of
   text instead of 9?
   <details><summary>Answer</summary>

   41 bytes round up to 48 and cost a 4-byte line pointer, so 52 bytes
   each out of 8168: 157 rows. With a 40-byte string the tuple is 72
   bytes, which is already a multiple of 8, plus 4: 76 bytes each, so
   `8168 / 76 = 107` rows. The line pointer is charged once per row no
   matter how small the row is, which is why very narrow tables waste
   proportionally more.
   </details>
2. Insert a row, update it, then update the result. What are the three
   tuples' `xmax` and `ctid` values, and how many does a scan return?
   <details><summary>Answer</summary>

   With transactions 10, 11 and 12 and TIDs `(0,1)`, `(0,2)`, `(0,3)`:
   the first has `xmax` 11 and `ctid` `(0,2)`, the second `xmax` 12 and
   `ctid` `(0,3)`, the third `xmax` 0 and `ctid` pointing at itself. A
   scan returns one row, the last. The chain is a forward-linked list
   that a reader arriving at any version can walk to the end.
   </details>
3. Why does `Delete` stamp `xmax` instead of calling the page's
   `DeleteItem`, which would actually free the space?
   <details><summary>Answer</summary>

   Because the deleting transaction may roll back, and because other
   transactions may still be entitled to see the row. Removing the bytes
   makes both impossible. The version stays until every snapshot that
   could want it is gone, which is a decision only VACUUM can make. This
   is the same reason an update writes a new version rather than
   overwriting the old one, and it is the foundation of chapter 17.
   </details>
4. `Update` stamps the old version before inserting the new one, and
   holds no lock across the two steps. Why that order, and why no lock?
   <details><summary>Answer</summary>

   The order is what chapter 17 needs: stamping the old version is the
   point at which a second transaction updating the same row must block,
   so the wait belongs there, before any new version exists to clean up.
   No lock is held across the steps because `Insert` pins a buffer of its
   own, and holding a content lock while acquiring another pin is how a
   buffer-level deadlock happens. The rule throughout is that a page lock
   is held only while touching that page.
   </details>
5. PostgreSQL keeps a free space map so an insert can find a page with
   room anywhere in the table, and heap-only tuples so an update that
   stays on one page does not touch the indexes. What does our
   last-block-plus-extend rule cost?
   <details><summary>Answer</summary>

   Space freed by deletes is never reused, so a table that is heavily
   updated grows without bound until VACUUM exists. Without HOT, every
   update writes a new index entry for every index on the table even when
   only an unindexed column changed, which roughly doubles the write cost
   of an update. Both are performance, not correctness, which is why they
   are deferrable, and both need machinery this chapter does not have:
   the free space map is another fork of the relation, and HOT needs the
   redirect line pointers chapter 01 left unused.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **A free space map.** Give each relation a second file with one byte per
   block recording the free space there, and make `Insert` consult it
   instead of scanning from block 0. Chapter 05's
   `TestInsertFillsPagesBeforeExtending` is the behaviour to preserve.
2. **HOT updates.** When the new version fits on the same page and no
   indexed column changed, chain it from the old line pointer instead of
   giving it its own item number. You need chapter 13's indexes to see the
   point, so this one is best kept until after it.
3. **Read `heap_update` in `heapam.c`.** Ours pins the old page, stamps it,
   and inserts the new version; PostgreSQL takes a tuple lock, checks for a
   concurrent update, and may pin two pages in a fixed order. What is that
   order, and why does it exist?
