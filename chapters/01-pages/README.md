# Chapter 01: Pages

**Goal.** An 8 KiB slotted page that stores variable-length items and
addresses each one by a stable, 1-based item number.

**You edit.** `internal/page/page.go`: 17 functions with
`panic("not implemented")` bodies, `New` through `InsertItem`. Nothing
else changes; `page_test.go` is the contract.

**Needs from earlier chapters.** Nothing. This is the first chapter.

**Done when.** `make test-ch01` passes.

**Effort.** Medium, about 3 hours. Steps 1 and 3 are the hard part: the
first test compares 24 literal header bytes and the second compares a line
pointer's four, so the arithmetic has to be right before anything higher
up works.

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
                                   Buffer manager (04)
                                             │
                                             ▼
                                   Storage manager (03)
                                             │
                                             ▼
                              Pages [01] and tuples (02) on disk
```

PostgreSQL never reads or writes a single tuple from disk. It reads and
writes fixed-size pages (blocks) of 8192 bytes, and every tuple lives inside
one. The page is the unit of I/O, of caching (chapter 04), of locking, and of
write-ahead logging. This chapter builds the page and nothing else.

PostgreSQL source: `storage/page/bufpage.c`, `include/storage/bufpage.h`,
`include/storage/itemid.h`, and `include/storage/off.h`.

## The slotted page

**Predict.** A page holds three items. You delete item 2 and compact the
page. What item number does the third item have afterwards, and how many
bytes did the compaction reclaim?

A page has to hold a variable number of variable-length items and let them
be addressed by a small stable number, so that an index can point at a tuple
with `(block number, item number)` and keep pointing at it even after other
items on the page move around. The design that does this is the slotted
page:

```
 0                       24
 ┌───────────────────────┬────┬────┬────┬─────────────────────┬──────┬──────┬───────┐
 │ header                │lp1 │lp2 │lp3 │ ..... free space .. │item3 │item2 │ item1 │
 └───────────────────────┴────┴────┴────┴─────────────────────┴──────┴──────┴───────┘
                                        ▲                     ▲                     ▲
                                      lower                 upper               special
```

- The header is fixed-size at the start.
- Line pointers (`ItemId` in PostgreSQL) are 4 bytes each and grow upward
  from the header. Line pointer *n* is item number *n*. Item numbers are
  1-based (`OffsetNumber`).
- Item data grows downward from the end. New items are placed just below
  `upper`.
- `lower` and `upper` mark the two ends of the free space between them.
- `special` marks the start of an optional region at the end of the page that
  an access method can use for its own purposes. The B-tree keeps its
  sibling pointers there (chapter 12). Heap pages have no special space.

Because items are addressed through line pointers, `Compact` can slide item
data toward the end of the page to close gaps left by deletions without
changing any item number. That is what lets index entries survive.

Worked example. Start from `New(0)` and add three items, `"alpha"`,
`"be"`, `"gamma"`. Each row is the page state after the call:

| Call | Result | lower | upper | data written |
|---|---|---|---|---|
| `New(0)` | – | 24 | 8192 | – |
| `AddItem("alpha")` | 1 | 28 | 8184 | `61 6c 70 68 61` at 8184 |
| `AddItem("be")` | 2 | 32 | 8176 | `62 65` at 8176 |
| `AddItem("gamma")` | 3 | 36 | 8168 | `67 61 6d 6d 61` at 8168 |

`lower` grows by 4 per item, one line pointer. `upper` falls by 8 per
item even for the 2-byte `"be"`, because item data is 8-byte aligned. The
last 32 bytes of the page now read:

```
8160  00 00 00 00 00 00 00 00  67 61 6d 6d 61 00 00 00  |........gamma...|
8176  62 65 00 00 00 00 00 00  61 6c 70 68 61 00 00 00  |be......alpha...|
```

Items are laid out back to front: item 1 is last in the file, item 3 is
first. Now `DeleteItem(2)` zeroes line pointer 2 and touches nothing
else: `lower` and `upper` stay at 36 and 8168, and `62 65` is still on
the page. Only `Compact` reclaims it. It copies items 1 and 3 out, then
lays them back from `special = 8192` downward in item-number order:

| Item | length | aligned | new offset |
|---|---|---|---|
| 1 `"alpha"` | 5 | 8 | 8192 − 8 = 8184 |
| 3 `"gamma"` | 5 | 8 | 8184 − 8 = 8176 |

`upper` becomes 8176, so the eight bytes `"be"` held are free again.
`lower` stays at 36, because item 3 is still item 3 and its line pointer
is still the third slot. Line pointer 2 stays unused. The page tail:

```
8160  00 00 00 00 00 00 00 00  67 61 6d 6d 61 00 00 00  |........gamma...|
8176  67 61 6d 6d 61 00 00 00  61 6c 70 68 61 00 00 00  |gamma...alpha...|
```

The old copy of `"gamma"` at 8168 is still there. `Compact` does not
zero what it leaves behind; 8168–8175 is simply below `upper` now, which
makes it free space. Only bytes reachable through a normal line pointer
mean anything.

## Header layout

24 bytes, little-endian, identical in size and order to `PageHeaderData`.

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 8 | LSN | Write-ahead log position of the last change. Zero until v2. |
| 8 | 2 | Checksum | Always zero in v1. |
| 10 | 2 | Flags | Reserved, zero. |
| 12 | 2 | Lower | Offset of the end of the line pointer array. |
| 14 | 2 | Upper | Offset of the start of item data. |
| 16 | 2 | Special | Offset of the special space. `PageSize` when there is none. |
| 18 | 2 | PageSizeVersion | `PageSize | 1`. Page size is a multiple of 256, so the low byte is a layout version. |
| 20 | 4 | PruneXID | Reserved for pruning, zero in v1. |

A freshly initialised page with no special space has `Lower = 24`,
`Upper = 8192`, `Special = 8192`.

Worked example. The 24 bytes `New(0)` writes, which is exactly what
`TestNewHeaderBytes` compares against:

| Offset | Bytes | Field | Value |
|---|---|---|---|
| 0 | `00 00 00 00 00 00 00 00` | LSN | 0 |
| 8 | `00 00` | Checksum | 0 |
| 10 | `00 00` | Flags | 0 |
| 12 | `18 00` | Lower | 24 |
| 14 | `00 20` | Upper | 8192 |
| 16 | `00 20` | Special | 8192 |
| 18 | `01 20` | PageSizeVersion | 8193 |
| 20 | `00 00 00 00` | PruneXID | 0 |

Little-endian is the thing to get right: 8192 is `0x2000`, so the low
byte `00` comes first and `20` second. `PageSizeVersion` is
`8192 | 1 = 0x2001`, which is `01 20` on disk, not `20 01`.

A page that is all zero bytes is "new": it has been allocated on disk but
never initialised. `IsNew` reports that. Every other accessor assumes an
initialised page.

## Line pointer layout

One `uint32`, bit-packed exactly as PostgreSQL's `ItemIdData` on a
little-endian machine:

| Bits | Field |
|---|---|
| 0–14 | Offset of the item data from the start of the page |
| 15–16 | Flags |
| 17–31 | Length of the item in bytes, unaligned |

Flags: `0` unused, `1` normal, `2` redirect (unused in v1), `3` dead (unused
in v1). A deleted item's line pointer is set to unused with offset and
length zero; the line pointer itself stays in the array so item numbers
after it do not shift.

Worked example. Line pointer 1 of the page above is the four bytes
`f8 9f 0a 00` at offset 24, which little-endian is the `uint32`
`0x000A9FF8`:

| Expression | Value | Field |
|---|---|---|
| `v & 0x7fff` | `0x1FF8` = 8184 | offset |
| `v >> 15 & 3` | 1 | flags: `ItemNormal` |
| `v >> 17 & 0x7fff` | 5 | length |

So `ItemID{Off: 8184, Len: 5, Flags: ItemNormal}`, the item `"alpha"`.
Length 5 is the true length, not the aligned 8; the padding is not
recorded anywhere. Line pointer 2 is `f0 9f 04 00`, `0x00049FF0`:
offset 8176, flags 1, length 2. After `DeleteItem(2)` those four bytes
are `00 00 00 00`, which decodes to offset 0, flags 0, length 0.

## Alignment

Item data is placed at an 8-byte aligned offset: each item occupies
`align8(len)` bytes below the previous `upper`, though the line pointer
records the true length. PostgreSQL calls this `MAXALIGN`. Special space
size is rounded up to 8 bytes the same way.

## API

`internal/page`

```go
type Page []byte                     // always exactly PageSize bytes
type OffsetNumber uint16             // 1-based item number
type ItemID struct{ Off, Len uint16; Flags ItemFlags }

func New(specialSize int) Page
func (p Page) Init(specialSize int)
func (p Page) IsNew() bool

func (p Page) LSN() uint64
func (p Page) SetLSN(lsn uint64)
func (p Page) Lower() uint16
func (p Page) Upper() uint16
func (p Page) Special() uint16
func (p Page) SpecialSpace() []byte

func (p Page) NumItems() OffsetNumber
func (p Page) ItemID(n OffsetNumber) ItemID
func (p Page) FreeSpace() int
func (p Page) AddItem(item []byte) (OffsetNumber, error)
func (p Page) GetItem(n OffsetNumber) ([]byte, error)
func (p Page) DeleteItem(n OffsetNumber) error
func (p Page) Compact()
```

Semantics the tests depend on:

- `AddItem` reuses the lowest-numbered unused line pointer if there is one;
  otherwise it appends a new one. It returns `ErrNoSpace` if the aligned
  item plus (if needed) a new line pointer does not fit between `lower` and
  `upper`, and `ErrItemTooLarge` if the item could never fit on an empty
  page. An empty item is allowed.
- `GetItem` returns a slice aliasing the page, not a copy. It returns
  `ErrInvalidOffset` for item numbers outside `1..NumItems()` and
  `ErrItemUnused` for a deleted item.
- `FreeSpace` is the number of bytes available for a new item's data
  assuming a new line pointer is needed, so `upper - lower - 4`, or zero if
  that would be negative. This mirrors `PageGetFreeSpace`.
- `NumItems` is zero on a page whose `lower` is at or below `HeaderSize`.
  A never-initialised page is all zero bytes, so the subtraction would
  wrap in `uint16` and report 16378 line pointers that are not there;
  `PageGetMaxOffsetNumber` guards the same case. The buffer manager
  writes a zero page to disk before the caller initialises it, so an
  unclean exit leaves one behind for the next scan to walk.
- `Compact` moves the data of all normal items so they are contiguous at
  the end of the page, sets `upper` accordingly, and removes any trailing
  unused line pointers (as `PageTruncateLinePointerArray` does). Item numbers
  of normal items never change.
- Every mutating method keeps `HeaderSize <= lower <= upper <= special
  <= PageSize`.

## Implementation notes

**Go you will need.** `encoding/binary.LittleEndian` (`Uint16`,
`PutUint16`, `Uint32`, `PutUint32`, `Uint64`, `PutUint64`) for every header
field and line pointer; the `clear` builtin to zero a slice; `&^` (AND NOT)
for `align8`; `copy`, which handles overlapping source and destination like
`memmove`; and the fact that a subslice `p[a:b]` aliases the same array,
which is what "returns a slice aliasing the page" means.

**Header.** Give each header field a private offset constant (`offLower =
12`, `offUpper = 14`, `offSpecial = 16`, `offVersion = 18`, ...) and two
private helpers, `u16(off)` and `setU16(off, v)`, that wrap
`binary.LittleEndian`. Every accessor is then one line. `Init` and `Lower`
show the pattern:

```go
func (p Page) Init(specialSize int) {
	if specialSize < 0 || align8(specialSize) > PageSize-HeaderSize {
		panic("page: invalid special space size")
	}
	clear(p[:HeaderSize])
	special := PageSize - align8(specialSize)
	p.setU16(offLower, HeaderSize)
	p.setU16(offUpper, uint16(special))
	p.setU16(offSpecial, uint16(special))
	p.setU16(offVersion, PageSize|LayoutVersion)
}

func (p Page) Lower() uint16 { return p.u16(offLower) }
```

`Upper`, `Special`, `LSN` and `SetLSN` are the same shape with a different
offset and width. `New` is `make(Page, PageSize)` followed by `Init`.
`IsNew` only tests `Upper() == 0`, as `PageIsNew` does; no initialised page
has that, and scanning all 8192 bytes would cost more than it is worth.

**Line pointers.** Two private functions, `pack(ItemID) uint32` and
`unpackItemID(uint32) ItemID`, do the bit layout from the table above:
offset is `v & 0x7fff`, flags `v >> 15 & 3`, length `v >> 17 & 0x7fff`.
`lpOffset(n)` is `HeaderSize + (n-1)*LinePointerSize`; a private
`setItemID(n, id)` writes `pack(id)` there. `NumItems` derives from
`lower`, so it is never stored anywhere; guard the subtraction, since
`lower` is zero on a page nobody has initialised.

**AddItem.** In this order:

1. Reject `len(item) > MaxItemSize` before touching the page
   (`TestAddItemTooLarge` checks that a failed call changes nothing).
2. Scan line pointers 1..`NumItems()` for the first with `ItemUnused`
   flags. If none, the new item number is `NumItems()+1` and `lower` grows
   by `LinePointerSize`; otherwise `lower` stays.
3. New `upper` is the old one minus `align8(len(item))`. If it is below the
   new `lower`, `ErrNoSpace`. Note `<`, not `<=`: `lower == upper` is a
   full page, and an empty item into 4 free bytes must succeed
   (`TestAddItemNoSpace`).
4. `copy` the item to `p[upper:]`, store the new `lower` and `upper`, write
   the line pointer with the true length and `ItemNormal`.

**FreeSpace.** `upper - lower` as `int`s, minus `LinePointerSize`, clamped
at zero. Do the arithmetic in `int`, not `uint16`, or a full page wraps.

**GetItem.** Range-check `n`, decode the line pointer, require
`ItemNormal`, return `p[off : off+len]`. Callers that keep the slice see
later writes to the page; that is intended.

**DeleteItem.** After the same two checks as `GetItem`, store a zero
`ItemID{}`: flags unused, offset and length zero. `lower` and `upper` do
not move, so `FreeSpace` does not grow until `Compact`.

**Compact.** The trap is overlap: sliding item 1 down toward `special` can
overwrite item 2 before it has been read. The solution avoids it by
copying first and writing second:

1. Walk line pointers 1..`NumItems()`; for each normal one, remember its
   number and a private copy of its data, and note the highest normal
   number seen.
2. Starting at `upper = Special()`, lay the copies back in item-number
   order: subtract `align8(len)`, `copy` the data in, rewrite the line
   pointer with the new offset.
3. Set `upper`, and set `lower` to `lpOffset(last+1)`, which is
   `HeaderSize` when nothing survived.

PostgreSQL's `PageRepairFragmentation` instead sorts live items by offset
and moves them in place with `memmove`; either works, the copy is
simpler.

**InsertItem.** Check size, then `n` in `1..NumItems()+1`, then space with
a line pointer always counted (no reuse). Shift the line pointers
`n..NumItems()` up by one slot with a single `copy` from `p[lpOffset(n):
lpOffset(NumItems()+1)]` to `p[lpOffset(n)+LinePointerSize:]`; `copy` is
safe on the overlap. Then write the item and the new line pointer at
`n` exactly as `AddItem` does. Compute the copy bounds before updating
`lower`.

## Suggested order

One step at a time with the line under it; `make test-ch01` at the end.
Every test not named in that step or an earlier one still panics.

1. Header. `New`, `Init`, `IsNew`, `LSN`, `SetLSN`, `Lower`, `Upper`,
   `Special`, `SpecialSpace`. Green: `TestNewHeaderBytes`, `TestIsNew`,
   `TestSpecialSpaceIsAligned`, `TestLSN`. The first test compares 24
   literal bytes, so get the layout right before anything else.

   ```sh
   go test -race ./internal/page/... -run 'TestNewHeaderBytes|TestIsNew|TestSpecialSpaceIsAligned|TestLSN'
   ```
2. Line pointers. `NumItems`, `ItemID`. Green:
   `TestNumItemsOnNewPage`, `TestItemIDPanicsOutOfRange`.

   ```sh
   go test -race ./internal/page/... -run 'TestNumItemsOnNewPage|TestItemIDPanicsOutOfRange'
   ```
3. Items. `AddItem`, `GetItem`, `FreeSpace`. Green:
   `TestAddItemFirstItemBytes` (literal line pointer bytes),
   `TestAddGetRoundTrip`, `TestGetItemAliasesPage`, `TestGetItemErrors`,
   `TestFreeSpaceIsExact`, `TestFreeSpaceNeverNegative`,
   `TestAddItemNoSpace`, `TestAddItemTooLarge`, `TestAddItemFillsPageExactly`.

   ```sh
   go test -race ./internal/page/... -run 'TestAddItemFirstItemBytes|TestAddGetRoundTrip|TestGetItemAliasesPage|TestGetItemErrors|TestFreeSpaceIsExact|TestFreeSpaceNeverNegative|TestAddItemNoSpace|TestAddItemTooLarge|TestAddItemFillsPageExactly'
   ```
4. Delete. `DeleteItem`. Green: `TestDeleteKeepsItemNumbers`,
   `TestAddItemReusesUnusedLinePointer`, `TestSerializationRoundTrip`.

   ```sh
   go test -race ./internal/page/... -run 'TestDeleteKeepsItemNumbers|TestAddItemReusesUnusedLinePointer|TestSerializationRoundTrip'
   ```
5. Compact. `Compact`. Green: `TestCompact`, `TestCompactEmptyAndAllDeleted`,
   and `TestRandomOperations`, which runs random add/delete/compact
   sequences against a Go map and is the real judge of steps 3 to 5.

   ```sh
   go test -race ./internal/page/... -run 'TestCompact|TestCompactEmptyAndAllDeleted|TestRandomOperations'
   ```
6. `InsertItem` (used by chapter 12; do it now or when you get there).
   Green: `TestInsertItem`.

   ```sh
   go test -race ./internal/page/... -run 'TestInsertItem'
   ```

This is what one step looks like from both ends. Before step 1:

```
$ go test -race ./internal/page/... -run 'TestNewHeaderBytes|TestIsNew|TestSpecialSpaceIsAligned|TestLSN'
--- FAIL: TestNewHeaderBytes (0.00s)
panic: not implemented [recovered, repanicked]
...
github.com/raphi011/build-postgres/internal/page.New(...)
	internal/page/page.go:56
FAIL	github.com/raphi011/build-postgres/internal/page	0.452s
FAIL
```

The panic line names the stub to write next, here `New` at
`page.go:56`. After step 1:

```
$ go test -race ./internal/page/... -run 'TestNewHeaderBytes|TestIsNew|TestSpecialSpaceIsAligned|TestLSN'
ok  	github.com/raphi011/build-postgres/internal/page	1.361s
```

`make test-ch01` still panics at this point, in `TestAddItemFirstItemBytes`.
That is the loop for every step in every chapter.

### When a test fails

- `TestNewHeaderBytes` — the last four header bytes are right but bytes
  12–19 are byte-swapped: you wrote big-endian. Every field is
  `binary.LittleEndian`, so `PageSizeVersion` 0x2001 is `01 20`.
- `TestAddItemFirstItemBytes` — line pointer is `f8 1f 06 00` instead of
  `f8 9f 06 00`: the flags shift is 15, and `ItemNormal` is 1, so a
  normal item contributes `1 << 15`.
- `TestAddItemNoSpace` — the empty item into exactly 4 free bytes gets
  `ErrNoSpace`. The check is `newUpper < newLower`, not `<=`; a page with
  `lower == upper` is full but still legal.
- `TestAddItemTooLarge` — the error is right but a later assertion says
  the page changed. Check `len(item) > MaxItemSize` before you touch
  `lower`, `upper`, or any line pointer.
- `TestFreeSpaceNeverNegative` — you get 65533 rather than 0: the
  subtraction wrapped in `uint16`. Convert to `int` first, then clamp.
- `TestGetItemAliasesPage` — a write through the returned slice is not
  visible on the page: you returned a copy. Return `p[off : off+len]`.
- `TestAddItemReusesUnusedLinePointer` — the item lands at number 4 when
  2 is free. Scan line pointers 1..`NumItems()` for the first unused one
  before appending, and leave `lower` alone when you reuse.
- `TestCompact` — an item comes back holding another item's bytes. You
  moved the data in place and one `copy` overwrote a later item's source
  before it was read. Copy every surviving item out first, then lay the
  copies back.
- `TestCompactEmptyAndAllDeleted` — `lower` is left at its old value on a
  page where nothing survived. It has to fall back to `HeaderSize`, which
  is what `lpOffset(last+1)` gives when `last` is 0.
- `TestRandomOperations` — passes hundreds of operations and then
  diverges right after a `Compact`. The usual cause is truncating the
  line pointer array to the count of surviving items rather than to the
  highest surviving item number; item 5 must stay item 5 when 1–4 are
  gone.
- `TestInsertItem` — line pointers after `n` are duplicated or lost.
  Compute the `copy` bounds from the old `NumItems()` before you update
  `lower`, and copy the whole range `lpOffset(n)..lpOffset(NumItems()+1)`
  in one call; `copy` handles the overlap.

The page you are building is what `go run ./cmd/pgdb dump FILE` prints:
its header fields, then one line per line pointer. It has nothing to read
until chapter 02 can form a tuple to put on a page, so the tour of it is
there.

## Why not grow the header when a field is needed

The page header carries an LSN that nothing writes until v2's WAL, and
`pd_flags` bits that nothing sets. The alternative, adding a field in the
chapter that first needs it, would rewrite the storage layer and every
golden-byte test each time, and would hide why PostgreSQL's header looks the
way it does. On-disk formats are final from the chapter that introduces them
(D3), which is why chapter 02's tuple header has the same kind of dead
weight in it.

## Out of scope

Checksums (`pg_checksum_page`), redirect and dead line pointers, pruning,
`PageIndexTupleDelete` style deletion that shifts item numbers, the
`PD_ALL_VISIBLE` and `PD_HAS_FREE_LINES` flags.

## Check your understanding

1. A page has `lower = 40` and `upper = 8100`. How many more 30-byte
   items fit?
   <details><summary>Answer</summary>

   223. Each item costs `align8(30) = 32` bytes below `upper` and 4 bytes
   of line pointer above `lower`, so 36 bytes of the 8060 between them,
   and `AddItem` fails only when the new `upper` would be below the new
   `lower`. `8060 / 36 = 223.9`, so 223 items fit and the 224th gets
   `ErrNoSpace`. Note that `FreeSpace()` reports 8056, not 8060: it
   charges for one line pointer up front.
   </details>
2. A line pointer reads `f0 9f 04 00`. Which item does it describe?
   <details><summary>Answer</summary>

   Little-endian, that is `0x00049FF0`. Offset `v & 0x7fff` = 8176, flags
   `v >> 15 & 3` = 1 (`ItemNormal`), length `v >> 17` = 2. So a live
   2-byte item whose data starts at page offset 8176 and which, because
   of alignment, owns the 8 bytes 8176–8183.
   </details>
3. Why does `Compact` not renumber line pointers, given that it is free
   to move the item data anywhere?
   <details><summary>Answer</summary>

   Because the item number is the only stable name an item has. A B-tree
   entry (chapter 12) points at a heap tuple with `(block number, item
   number)` and is not visited when the heap page is compacted. Renumber
   and every index in the database silently points at the wrong row. The
   whole reason for the indirection is that data offsets may move and
   item numbers may not.
   </details>
4. `IsNew` tests only `Upper() == 0` instead of checking that all 8192
   bytes are zero. Why is that sound?
   <details><summary>Answer</summary>

   `Init` always writes `upper = PageSize - align8(specialSize)`, and it
   rejects a special space large enough to push that below `HeaderSize`.
   No initialised page can therefore have `upper == 0`, and a page that
   the storage manager has extended but never initialised is all zeroes.
   One 2-byte load decides it; scanning 8192 bytes on every buffer read
   would not.
   </details>
5. PostgreSQL's `PageRepairFragmentation` sorts the live items by offset
   and slides them with `memmove`, where we copy every item out and lay
   the copies back. What does PostgreSQL buy for the extra complexity?
   <details><summary>Answer</summary>

   No allocation. Compaction runs with the buffer pinned and locked
   exclusively, in the middle of an insert that found no room, so it is
   on the hot path and must not depend on the allocator or on how much
   memory the page's live data happens to need. Sorting by offset and
   moving from the high end down means every `memmove` writes into space
   already vacated, which is why the in-place version is correct at all.
   Our copy-first version has the same result and is easier to be sure
   about, which is the trade this book makes throughout (D4).
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Checksums.** Implement `pg_checksum_page`'s FNV-based algorithm over
   the page with `pd_checksum` zeroed, and decide who would call it: the
   checksum belongs at the point a page leaves and enters memory, which is
   chapter 03's `Read` and `Write`.
2. **Deletion that renumbers.** Implement `PageIndexTupleDelete`, which
   removes a line pointer and shifts the item numbers above it down, and
   write the test that shows why the heap cannot use it and a B-tree can.
3. **Read `PageRepairFragmentation` in `bufpage.c`.** It sorts the live
   items by offset before moving anything, where our `Compact` may walk in
   item order. What does the sort buy, and what would break without it if
   the copy were not through a scratch buffer?
