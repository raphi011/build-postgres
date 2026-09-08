# Chapter 12: B-tree

**Goal.** An on-disk B+tree over one key column, storing heap TIDs:
leaves of `(key, TID)` in key order, internal pages of downlinks, splits
that propagate up to a new root, and a forward range scan along the leaf
chain.

**You edit.** `internal/btree/btree.go`: 25 functions with
`panic("not implemented")` bodies, `FormIndexTuple` through `Check`.
Nothing else changes; `btree_test.go` is the contract.

**Needs from earlier chapters.** `internal/page` (chapter 01),
`internal/tuple` (chapter 02), `internal/bufmgr` (chapter 04), and
`internal/smgr` (chapter 03), which `Create` and `Check` reach through
`pool.Store()` for `Create`, `NBlocks`, and `smgr.ErrExists`. The
skeleton of `page.InsertItem` was added for this chapter; if you skipped
it in chapter 01 (step 6 there, `TestInsertItem`), implement it now:
`Insert` keeps tree pages in key order with it.

**Done when.** `make test-ch12` passes.

**Effort.** Long, about 8 hours. The split in `Insert` (step 4) is the
hard part: it moves items, writes a new downlink into the parent, and may
have to split the parent too.

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
                              Heap access method (05)   B-tree [12, 13]
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

A sequential scan reads every page of a table to find one row. An index
finds the row by its key: PostgreSQL's default index is a B+tree whose
leaves hold `(key, TID)` pairs in key order, so equality and range
predicates touch a handful of pages instead of all of them. This chapter
builds the tree as an access method over the storage layer of Part 1.
Chapter 13 wires it into `CREATE INDEX` and the executor.

PostgreSQL source: `access/nbtree/README` (read it first),
`access/nbtree/nbtinsert.c` (`_bt_doinsert`, `_bt_insertonpg`,
`_bt_split`, `_bt_newroot`), `nbtsearch.c` (`_bt_search`, `_bt_binsrch`,
`_bt_first`, `_bt_readpage`), `nbtpage.c` (`_bt_initmetapage`,
`_bt_getroot`), `nbtsplitloc.c` (`_bt_findsplitloc`), `nbtutils.c`,
`include/access/nbtree.h` (`BTPageOpaqueData`, `BTMetaPageData`),
`include/access/itup.h` (`IndexTupleData`), and `contrib/amcheck/verify_nbtree.c`.

## The shape of the tree

Every page of the index file is a chapter 01 page with a 16-byte special
space. Block 0 is the metapage: it says where the root is and how tall
the tree is. Every other page is a tree page at some level: leaves are
level 0, the root is the highest level, and every leaf is at the same
depth. Pages at one level form a doubly linked list through the
`prev`/`next` fields in the special space, so a range scan walks the
leaves left to right without going back up.

A leaf page holds index tuples `(key, heap TID)` in key order. An
internal page holds downlinks `(separator key, child block)`, also in key
order; a search on an internal page picks the last downlink whose
separator is `<=` the search key and descends into its child. The first
downlink of every internal page has no key: it stands for minus infinity
and matches everything smaller than the second downlink's separator.

Every page except the rightmost one at its level starts with a high key,
item 1, which is an upper bound: every item on the page is smaller than
it. Data items follow at item 2 (item 1 on a rightmost page). A page's
high key equals the separator of the next downlink in its parent, which
is the invariant `Check` verifies and what would let a concurrent reader
detect that a page split under it (Lehman and Yao's "move right", out of
scope here).

Keys are compared the way chapter 10 compares them: integers
numerically, booleans `false < true`, text bytewise. `NULL` sorts after
every value, PostgreSQL's default `NULLS LAST` for an ascending index,
and is indexed like any other key. Two entries with equal keys are
ordered by their heap TID, so every entry has a unique position in the
tree (PostgreSQL 12 made the heap TID a tiebreaking key column for this
reason). That is what makes the high-key invariant exact even with
duplicates: a high key copied from a leaf item carries that item's heap
TID.

## On-disk formats

Special space, `BTPageOpaqueData`, 16 bytes, little-endian:

| offset | size | field | meaning |
|---|---|---|---|
| 0 | 4 | `prev` | left sibling, `None` (0) if leftmost |
| 4 | 4 | `next` | right sibling, `None` if rightmost |
| 8 | 4 | `level` | 0 for leaves |
| 12 | 2 | `flags` | `FlagLeaf` 1, `FlagRoot` 2, `FlagMeta` 8 |
| 14 | 2 | cycle id | always 0 |

Block 0 is the metapage: flags `FlagMeta`, item 1 is `BTMetaPageData`:

| offset | size | field |
|---|---|---|
| 0 | 4 | magic `0x053162` |
| 4 | 4 | version 1 |
| 8 | 4 | root block, `None` while the tree is empty |
| 12 | 4 | root level (tree height minus one) |

Worked example. `Opaque{Prev: 3, Next: 9, Level: 2, Flags: FlagRoot}`
occupies the last sixteen bytes of the page:

```
03 00 00 00  09 00 00 00  02 00 00 00  02 00 00 00
```

with the last four bytes holding the 2-byte flags `02 00` and the 2-byte
cycle id `00 00`. That is `TestOpaqueBytes` exactly.

PostgreSQL writes the metadata straight after the page header and moves
`pd_lower` past it; storing it as an item uses chapter 01's API and
changes nothing else.

Index tuple, `IndexTupleData`, then the key:

| offset | size | field |
|---|---|---|
| 0 | 4 | TID block |
| 4 | 2 | TID offset |
| 6 | 2 | info: bit 15 NULL, bit 13 pivot, bits 0–12 total size |
| 8 | | key: `int4` 4 bytes, `int8` 8, `bool` 1, `text` 4-byte length + bytes; absent when NULL |

Worked example. Four index tuples, byte for byte, from
`TestIndexTupleBytes`:

| Key type, key, TID | Bytes | Size |
|---|---|---|
| `int4` 7, `(3,5)` | `03 00 00 00 05 00 0c 00 07 00 00 00` | 12 |
| `bool` true, `(1,1)` | `01 00 00 00 01 00 09 00 01` | 9 |
| `text` `"ab"`, `(2,9)` | `02 00 00 00 09 00 0e 00 02 00 00 00 61 62` | 14 |
| `int4` NULL, `(4,4)` | `04 00 00 00 04 00 08 80` | 8 |

The info word at offset 6 is the total size or'ed with the flag bits:
`0c 00` is 12 with no flags, and `08 80` is 8 with bit 15, the NULL bit,
set. A NULL tuple is the 8-byte header and nothing else, so the key is
absent rather than zero, and the `bool` tuple is 9 bytes, not padded; the
page aligns items when it stores them, and the recorded size is the true
one.

A leaf tuple's TID is the heap tuple's. A pivot tuple (a downlink or a
high key) has the child block in the TID's block field (unused in a high
key) and carries the heap TID of the leaf tuple it was copied from in
the 6 bytes after the key. The minus-infinity downlink is a pivot with
the NULL bit and no key: 8 bytes. Items are 8-byte aligned on the page
(chapter 01), so the largest leaf tuple is `MaxItemSize = 2704` bytes:
three must fit on a page, and each may grow by a heap TID when copied
into a pivot (`BTMaxItemSize`).

## Search

Worked example, on the two-level tree built in the next section: root
block 3 holds a minus-infinity downlink to block 1 and a downlink `e` to
block 2; block 1 holds `a` and `c`, block 2 holds `e` and `g`.

| Search key | On the root | Descends to | On the leaf |
|---|---|---|---|
| `a` | minus infinity is the last `<= a` | 1 | item 2, the first `>= a` |
| `d` | minus infinity | 1 | past the last item; the insert point |
| `e` | `e` is `<= e` | 2 | item 1 |
| `f` | `e` | 2 | item 2, `g` |

The `d` row is the one to hold on to: a search that finds nothing still
returns a position, and that position is where a new tuple goes. The `e`
row is why the comparison on an internal page is `<=` and not `<`: an
equal separator means the key lives on the right-hand child.

`_bt_search`: start at the root from the metapage; on each internal page
binary-search for the last downlink `<=` the search key and follow it;
stop at a leaf. `_bt_binsrch` on a leaf returns the first item `>=` the
search key, which is where an equal key starts and where a new tuple
goes. The search key for an insert is `(key, heap TID)`; for the lower
bound of a scan it is `(key, minus infinity)` for an inclusive bound
(land on the first duplicate) or `(key, plus infinity)` for an exclusive
one (skip them all).

## Insert

**Predict.** A leaf root holds three items and a fourth does not fit. How
many pages does the file have afterwards, which items end up on each, and
what is the new root's first downlink?

`_bt_doinsert`: search to the leaf, remembering the downlink taken on
each internal page, and insert at the position the binary search found.
If the tuple does not fit, split (`_bt_split`):

1. Gather the page's data items with the new tuple in place and choose a
   split point (`_bt_findsplitloc`): by bytes, as close to half as both
   halves allow, except that an append to the rightmost page leaves the
   left page `FillFactor` (90) percent full, so ascending inserts pack
   pages instead of leaving every one half empty.
2. Allocate the right page. It gets the items from the split point on,
   the old high key (if any), the old `next` as its `next`, and the left
   page as `prev`. On an internal page its first downlink becomes minus
   infinity: its key is redundant with the separator that goes up.
3. Rebuild the left page in a scratch buffer: a new high key, which is a
   pivot copy of the right page's first item, then the items before the
   split point; `next` is the right page. Copy it over the old page.
4. Point the old right sibling's `prev` at the new page.
5. Insert a downlink `(separator, right page)` into the parent, right
   after the downlink that led here. The parent may split in turn.
6. If the page was the root, `_bt_newroot`: a new page one level up with
   a minus-infinity downlink to the left page and the new downlink, and
   the metapage points at it.

A leaf root that splits, which is the worked example below:

```
before                    after

                          ┌─ 3: root, level 1 ───────────────┐
                          │  -inf ─► block 1    e ─► block 2 │
                          └───────┬────────────────┬─────────┘
┌─ 1: leaf root ─┐                ▼                ▼
│  a   c   e     │        ┌─ 1: leaf ──────┐   ┌─ 2: leaf ──────┐
└────────────────┘        │ high key: e    │   │  e   g         │
  insert g needs          │  a   c         │   │                │
  2612 bytes and          └────────────────┘   └────────────────┘
  288 are free                  next = 2 ────────► prev = 1
```

The left page keeps the low half and gains a high key; the right page is
new; the separator that goes up to the parent is a copy of the right
page's first item. A root split is the only way the tree gets taller.

Worked example. A `text` index whose keys are 2600 bytes long, so that
three fill a leaf. Call them `a`, `c`, `e`, `g`. After inserting `a`, `c`
and `e` the file has two blocks:

| Block | Kind | Level | Flags | Prev | Next | Items |
|---|---|---|---|---|---|---|
| 0 | metapage | – | meta | – | – | root = 1, root level = 0 |
| 1 | leaf root | 0 | leaf, root | 0 | 0 | `a`, `c`, `e` |

Free space on block 1 is 288 bytes and a fourth 2612-byte tuple needs far
more, so inserting `g` splits. Afterwards:

| Block | Kind | Level | Flags | Prev | Next | Items |
|---|---|---|---|---|---|---|
| 0 | metapage | – | meta | – | – | root = 3, root level = 1 |
| 1 | leaf | 0 | leaf | 0 | 2 | high key `e`, then `a`, `c` |
| 2 | leaf | 0 | leaf | 1 | 0 | `e`, `g` |
| 3 | root | 1 | root | 0 | 0 | minus infinity → 1, `e` → 2 |

Four items split two and two. The left page keeps `a` and `c` and gains a
high key, which is a pivot copy of the right page's first item, `e`. The
right page keeps `e` and `g` and has no high key, because it is now the
rightmost page at its level; block 1's `next` points at it and block 2's
`prev` points back. The new root has two items: a minus-infinity pivot of
8 bytes whose TID block is 1, and a separator `e` whose TID block is 2.

The separator in the root and the high key on block 1 are the same bytes,
2618 of them, six more than the leaf tuple `e` because a pivot carries
the heap TID it was copied from after the key. That byte-for-byte
equality is the invariant `Check` verifies. A search for `f` now takes
the last downlink `<= f`, which is `e`, descends to block 2, and lands on
`g`, the first item `>= f`.

The first insert into an empty tree creates a leaf root (`_bt_getroot`
with write access). Nothing is ever deleted (D10).

## Scan

`_bt_first` searches for the lower bound (the leftmost leaf when there
is none) and copies the qualifying entries of that leaf; `_bt_next`
follows `next` to the following leaf. The scan stops at the first entry
above the upper bound. As in the heap scan, no pin is held between
calls. NULL keys sit at the end of the key space, so a scan with no
upper bound reaches them after every value; a caller evaluating `a > 5`
stops at the first `IsNull` entry, which is how `_bt_checkkeys` ends a
scan.

## The checker

`Check` is a small `bt_index_check`: it walks the tree one level at a
time, from the root's level down to the leaves, following sibling links,
and verifies the metapage, that every page has the level, flags, and
`prev` link it should, that items are in strictly increasing `(key, heap
TID)` order, that all of them are below the high key, that each page's
high key is byte-identical to its parent's next downlink (or the page is
rightmost when the parent says so), that the sibling chain at each level
lists exactly the children the level above points to, and that every
block of the file is reached once. Every failure wraps `ErrCorrupt` and
names the block.

## API

`internal/page` gains `InsertItem(n, item)`: `PageAddItem` with an
offset number, so that a tree page keeps its items in key order.

`internal/btree`

```go
const SpecialSize = 16
const MetaBlock, None tuple.BlockNumber = 0, 0
const Magic, Version uint32 = 0x053162, 1
const MetaSize, HeaderSize, TIDSize = 16, 8, 6
const MaxItemSize = 2704
const FillFactor = 90
const FlagLeaf, FlagRoot, FlagMeta uint16 = 1, 2, 8
const IndexSizeMask, IndexPivotMask, IndexNullMask uint16 = 0x1FFF, 0x2000, 0x8000
var ErrKeyType, ErrKeyTooLarge, ErrCorrupt error

type IndexTuple []byte
func FormIndexTuple(typ tuple.TypeID, key tuple.Datum, tid tuple.TID) (IndexTuple, error)
func (t IndexTuple) TID() tuple.TID
func (t IndexTuple) HeapTID() tuple.TID
func (t IndexTuple) Key(typ tuple.TypeID) tuple.Datum
func (t IndexTuple) Size() int
func (t IndexTuple) IsNull() bool
func (t IndexTuple) IsPivot() bool
func (t IndexTuple) IsMinusInf() bool

type Opaque struct { Prev, Next tuple.BlockNumber; Level uint32; Flags uint16 }
func ReadOpaque(p page.Page) Opaque
func WriteOpaque(p page.Page, o Opaque)
type Meta struct { Root tuple.BlockNumber; Level uint32 }

type Tree struct{ ... }
func Create(pool *bufmgr.Pool, oid tuple.OID, typ tuple.TypeID) (*Tree, error)
func Open(pool *bufmgr.Pool, oid tuple.OID, typ tuple.TypeID) *Tree
func (t *Tree) OID() tuple.OID
func (t *Tree) KeyType() tuple.TypeID
func (t *Tree) Meta() (Meta, error)
func (t *Tree) Insert(key tuple.Datum, tid tuple.TID) error
func (t *Tree) Search(key tuple.Datum) (tuple.BlockNumber, page.OffsetNumber, error)
func (t *Tree) Scan(lo, hi *Bound) *Scan
func (t *Tree) Check() error

type Bound struct { Key tuple.Datum; Inclusive bool }
type Scan struct{ ... }
func (s *Scan) Next() bool
func (s *Scan) TID() tuple.TID
func (s *Scan) Key() tuple.Datum
func (s *Scan) IsNull() bool
func (s *Scan) Err() error
func (s *Scan) Close()
```

Semantics the tests depend on:

- The byte layouts above, checked as golden slices: `FormIndexTuple`,
  `WriteOpaque`, and the metapage after `Create`.
- `FormIndexTuple` and `Insert` reject a key whose Go type does not match
  the tree's type with `ErrKeyType` (`int32`, `int64`, `bool`, `string`;
  `nil` is NULL) and a tuple longer than `MaxItemSize` with
  `ErrKeyTooLarge`; a failed insert changes nothing.
- `Create` writes only the metapage (one block) and fails with
  `smgr.ErrExists` if the file exists. `Open` does no I/O. `Meta`,
  `Insert`, and `Check` on a file whose block 0 is not a metapage return
  `ErrCorrupt`.
- The first insert makes block 1 a leaf root; `Meta` reports it with
  level 0. Splits allocate blocks in order, and `Meta.Root` and
  `Meta.Level` change exactly when a new root is made.
- `Search` returns the leaf block and the item number of the first entry
  `>=` the key, one past the last item when there is none, and `None`
  on an empty tree.
- `Scan` yields entries in `(key, heap TID)` order, includes NULL keys
  only after every value and only when there is no upper bound, and
  reports `ErrKeyType` through `Err` for a bound of the wrong type or a
  nil bound key.
- Ascending inserts of 20000 `int4` keys use 57 blocks: 55 leaves of 366
  tuples (90 percent of the 407 that fit), one root, the metapage.
- `Check` accepts every tree the tests build and rejects each of the
  corruptions in `TestCheckDetectsCorruption` with `ErrCorrupt`.
- Everything survives `FlushAll` and reopening.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** How `Insert` remembers the path it descended, so that
a split can put a downlink into the parent and split the parent in turn.
The tests pin the tree on disk, not the way you got there: `Check` walks
the result and the golden bytes fix the page format. The folded notes take
one approach; a stack of block numbers, a recursive descent that returns
the new downlink, and a re-search from the root all pass.

**Go you will need.** `encoding/binary.LittleEndian` (`PutUint32`,
`AppendUint32`, `Uint16`) for every on-disk field. A type switch on `typ`
with a `, ok` assertion (`key.(int32)`) checks a key's Go type.
`strings.Compare` for text keys. `fmt.Errorf("%w: block %d: ...",
ErrCorrupt, blk)` wraps the sentinel so `errors.Is` finds it.
`bytes.Equal`. `append(page.Page(nil), buf.Page()...)` copies a page out
of a frame and `copy(dst, p)` writes one back whole. A
`map[tuple.BlockNumber]bool` is a set. `(n+7)&^7` rounds up to 8.

<details><summary><b>FormIndexTuple.</b></summary>

Header first: `putTID` into bytes 0..5, then a private `appendKey(b, typ,
key)` that type-switches on `typ`, asserts the matching Go type with `,
ok`, and appends the encoding; any other combination is `ErrKeyType`. Check
`len > MaxItemSize` (`ErrKeyTooLarge`) before the info word, which is the
length or'ed with `IndexNullMask` for a nil key. `appendKey(nil, typ, key)`
doubles as the key-type check later.
</details>

<details><summary><b>Accessors.</b></summary>

All read the info word. `IsMinusInf` is pivot with `Size() == HeaderSize`.
`HeapTID` returns the TID field of a leaf tuple, the last `TIDSize` bytes
of a pivot, zero for minus infinity. `Key` returns nil when the NULL bit is
set, which covers minus infinity too. A private `keyBytes()` is the key
alone: after the header, minus a pivot's trailing TID.
</details>

<details><summary><b>Opaque.</b></summary>

`p.SpecialSpace()` is the 16 bytes; the four fields sit at offsets 0, 4, 8,
and 12, and `WriteOpaque` zeroes the cycle id at 14.
`page.New(SpecialSize)` and `Init(SpecialSize)` make the space.
</details>

<details><summary><b>Page access.</b></summary>

Four private helpers do all buffer work, so tree code never holds a pin:
`readPage(blk)` pins, `RLock`s, copies the page, unlocks, unpins;
`modify(blk, fn)` pins, `Lock`s, runs `fn` on the frame, `MarkDirty`s on
success; `newPage()` is `pool.Extend` plus `Init(SpecialSize)` under the
lock, returning the block; `writePage(blk, p)` is `modify` with `copy`.
Splits build pages in `page.New(SpecialSize)` scratch pages and write them
whole.
</details>

<details><summary><b>Create.</b></summary>

`pool.Store().Create(oid)` (the store supplies `smgr.ErrExists`), `Open`,
`pool.Extend`, and under the lock `Init(SpecialSize)`, `AddItem` the 16
bytes of a private `metaBytes(Meta{Root: None})`, `WriteOpaque` with
`FlagMeta` alone, `MarkDirty`; unlock and unpin.
</details>

<details><summary><b>Meta.</b></summary>

`readPage(MetaBlock)` into a private `decodeMeta(p)`: check `p.Special() ==
page.PageSize-SpecialSize` before touching the special space (a heap page's
is empty; `ReadOpaque` would panic), then `FlagMeta`, item 1 of `MetaSize`
bytes, magic, version; each failure wraps `ErrCorrupt`. A private
`writeMeta(m)` is `modify` copying `metaBytes(m)` over item 1 in place.
</details>

<details><summary><b>Comparison.</b></summary>

`compareKey(typ, a, b)`: `int32` and `int64` by value, bool `false < true`,
text `strings.Compare`. `compareNullable` puts nil after everything;
`compareTID` orders by block, then offset; `compareTuples(a, b)` is key,
then `HeapTID`: the order `Check` verifies.
</details>

<details><summary><b>Search keys.</b></summary>

A private `skey` is what `binsrch` compares an item against: `key` (nil is
NULL), `tid`, a `tidMode` of -1 (below every heap TID: inclusive lower
bound, `Search`), 0 (that TID: inserts), or +1 (above every TID: exclusive
lower bound), and a `minusInf` flag for the leftmost leaf (no lower bound).
`compare(it, k)` returns 1 for `minusInf`, else the key order, else the
`tidMode` rule.
</details>

<details><summary><b>binsrch.</b></summary>

`firstData(o)` is 2 when `o.Next != None` (the page has a high key), else
1. Lower-bound binary search over `firstData..NumItems+1`: on a leaf move
right while the item is `< k` and return `low`; on an internal page start
one past the minus-infinity downlink, move right on `<=`, and return
`low-1`, the last downlink `<= k`. A private `item(p, off)` wraps
`GetItem`, nil on error.
</details>

<details><summary><b>descend.</b></summary>

`_bt_search`. `Meta` (its `ErrCorrupt` is how a heap file fails); a `None`
root is an empty tree (return a nil page). Loop: `readPage`, `ReadOpaque`;
a leaf ends it; else `binsrch`, append a `pathEntry{blk, off}` (the
internal page and the downlink taken) to the path slice and follow
`item.TID().Block`; a child of `None` or the page itself is `ErrCorrupt`.
Return the leaf copy, its block, and the path.
</details>

<details><summary><b>Pivot tuples.</b></summary>

`pivot(it, child)` copies the header and `keyBytes()` of any tuple, appends
its `HeapTID()`, sets the block field to `child` (`None` for a high key),
and or's `IndexPivotMask` into the info word with the new length; the NULL
bit stays. `minusInf(child)` is the 8-byte header alone with the pivot and
NULL bits set.
</details>

<details><summary><b>Insert.</b></summary>

`FormIndexTuple` first, so a bad key does no I/O. `descend` with `skey{key,
tid}`; a nil leaf means `newRootLeaf`: `newPage`, a `page.New` leaf holding
the tuple with `FlagLeaf|FlagRoot`, `writePage`, `writeMeta(Meta{Root:
blk})`. Else `binsrch` for the position and `insertAt(blk, leaf, pos, it,
path)` (`_bt_insertonpg`): if `FreeSpace()` covers the 8-aligned length,
`modify` with `InsertItem(pos, it)`; else `split`.
</details>

<details><summary><b>Split, the pages.</b></summary>

The "Insert" section above has the six steps; the details: `items` is the
page's data items with `it` at `pos`; `oldHigh` is item 1 when `Next !=
None`; `k := splitPoint(...)`; `sep := items[k]`. `newPage` the right
block, then fill two scratch pages: right gets `oldHigh`, then `items[k:]`
with the first replaced by `minusInf` of its child on an internal page;
left gets `pivot(sep, None)`, then `items[:k]`. Both take the level and
`Flags &^ FlagRoot`. Write right, then left, then the old sibling's `prev`
through `modify`.
</details>

<details><summary><b>Split, the parent.</b></summary>

`down := pivot(sep, rightBlk)`. Empty path: `newRoot(blk, down,
o.Level+1)`. Else pop the last `pathEntry`, `readPage` the parent, and
`insertAt(parent.blk, pp, parent.off+1, down, path[:len-1])`: the downlink
goes right after the one taken on the way down, and the recursion splits
the parent with the rest of the stack. `newRoot`: a scratch page with
`minusInf(left)` and `down`, `FlagRoot` only, the level; `writePage`, then
`writeMeta`.
</details>

<details><summary><b>splitPoint.</b></summary>

By bytes: an item costs its 8-aligned length plus `page.LinePointerSize`;
`usable` is `PageSize - HeaderSize - SpecialSize`. The target is half the
total, or `usable*FillFactor/100` when the split is an append to the
rightmost page (`o.Next == None` and `pos == NumItems+1`): that alone gives
the 57 blocks. For k in `1..len-1`, left is the first k items plus
`items[k]` plus `TIDSize` (its new high key), right is the rest plus
`oldHigh`; skip a k where either exceeds `usable`, keep the k nearest the
target; fall back to `len/2`.
</details>

<details><summary><b>Search.</b></summary>

A non-nil key goes through `checkKey` (`ErrKeyType`). `descend` with
`tidMode` -1, so it lands on the first duplicate; an empty tree returns
`None, page.InvalidOffsetNumber, nil`; else `binsrch` on the leaf.
`Search(nil)` finds the first NULL, which sorts last.
</details>

<details><summary><b>Check, the walk.</b></summary>

The "checker" section says what is verified. `pool.Store().NBlocks` and
`Meta`; an empty tree must be one block. State: `seen` (a block set,
metapage in it), `order` (the blocks the level above's downlinks name, left
to right; the root alone at the top), and `expect` (block to its expected
high key, or rightmost). Per level from `meta.Level` down to 0: follow
`Next` from `order[0]` and check each page (below), collecting `nextOrder`
and `nextExpect` from its downlinks: child i's high key is downlink i+1,
the last child inherits the page's own high key or rightmost. After the
level the chain must be `len(order)` long; at the end every block
`1..nblocks-1` must be seen.
</details>

<details><summary><b>Check, one page.</b></summary>

Block below `nblocks`, unseen, equal to `order[i]`; special size; level;
`FlagLeaf` iff level 0, no `FlagMeta`, `FlagRoot` iff the root; `Prev` is
the previous block; rightmost iff `Next == None`; at least one data item; a
high key that is a pivot whose bytes after the header (and NULL bit) equal
the expected downlink's; items strictly increasing by `compareTuples`, the
first not below the previous page's high key, the last below this page's;
on an internal page every item a pivot and minus infinity only first.
Failures name the block.
</details>

<details><summary><b>Scan.</b></summary>

`checkKey` both bounds (a nil `Key` fails too) into `err`; the `skey` is
`minusInf` for no lower bound, else the key with `tidMode` -1 (inclusive)
or +1 (exclusive); `descend`; an empty tree sets `done`; else `pos = -1`
and `load` from `binsrch`. A private `load(blk, p, off)` copies items
`off..NumItems` into `items`, setting `done` at the first above `hi`
(`compareNullable > 0`, or equal and exclusive; a NULL is above every
bound), and records `o.Next` in `next`.
</details>

<details><summary><b>Next.</b></summary>

False once `err` is set. Increment `pos`; while it is past `items`: if
`done` or `next == None`, set `done` and return false; else
`readPage(next)`, `load` from `firstData`, `pos = 0`. `readPage` copies, so
nothing stays pinned between calls. `TID`, `Key`, and `IsNull` index
`items[pos]`; `Close` sets `done` and drops `items`.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch12` at the end.
Every test not named in that step or an earlier one still panics.

1. Index tuples. `FormIndexTuple`, `TID`, `HeapTID`, `Key`, `Size`,
   `IsNull`, `IsPivot`, `IsMinusInf`. Green: `TestIndexTupleBytes`,
   `TestIndexTupleErrors`. The first test compares literal bytes: the
   info word is size or'ed with the flag bits, and a NULL tuple is the
   8-byte header alone.

   ```sh
   go test -race ./internal/btree/... -run 'TestIndexTupleBytes|TestIndexTupleErrors'
   ```
2. Special space. `ReadOpaque`, `WriteOpaque`. Green: `TestOpaqueBytes`
   (16 literal bytes, cycle id zero).

   ```sh
   go test -race ./internal/btree/... -run 'TestOpaqueBytes'
   ```
3. Metapage. `Create`, `Open`, `OID`, `KeyType`, `Meta`. Green: nothing
   on its own; every tree test starts here. See "Create" and "Meta"
   above, and "Page access" for the helpers everything else uses.
4. Insert and search. `Insert`, `Search`. Green: `TestSearch`. It fits on
   one leaf, but write the splits, the parent downlink, and the new root
   now: step 5 inserts 3000 keys before it checks anything. See
   "Comparison" through "Search" above.

   ```sh
   go test -race ./internal/btree/... -run 'TestSearch'
   ```
5. Checker. `Check`. Green: `TestOpenNotATree`,
   `TestCheckDetectsCorruption`. Every remaining test calls the checker,
   so it is also how you debug the splits; each corruption case must fail
   with an error wrapping `ErrCorrupt`.

   ```sh
   go test -race ./internal/btree/... -run 'TestOpenNotATree|TestCheckDetectsCorruption'
   ```
6. Scan. `Scan`, `Next`, `Scan.TID`, `Scan.Key`, `Scan.IsNull`, `Err`,
   `Close`. Green: `TestCreateMetapage`, `TestInsertAndScan`,
   `TestDuplicatesOrderByTID`, `TestNullsSortLast`,
   `TestErrorsLeaveTreeUnchanged`, `TestSplitsGrowTree`,
   `TestAscendingInsertsPackPages`, `TestPersistence`, and
   `TestRandomRangeScans`, which inserts thousands of random keys of
   each type, with duplicates and NULLs, and compares two hundred random
   range scans with a sorted slice: the real judge of steps 4 to 6. The
   split test inserts keys near the size limit so the tree reaches four
   levels within a few hundred inserts; the ascending test expects
   exactly 57 blocks (see "splitPoint").

   ```sh
   go test -race ./internal/btree/... -run 'TestCreateMetapage|TestInsertAndScan|TestDuplicatesOrderByTID|TestNullsSortLast|TestErrorsLeaveTreeUnchanged|TestSplitsGrowTree|TestAscendingInsertsPackPages|TestPersistence|TestRandomRangeScans'
   ```

### When a test fails

- `TestIndexTupleBytes` — the size in the info word is right and the
  flags are lost, or a NULL tuple is 12 bytes. The info word is the
  total size or'ed with the flags, and a NULL tuple stores no key at all,
  so it is exactly the 8-byte header.
- `TestIndexTupleBytes` — the `bool` tuple is 16 bytes. The tuple itself
  is not padded; chapter 01's page aligns items when it stores them, and
  `Size` must report the true length or the page walk goes wrong.
- `TestOpaqueBytes` — the flags land at offset 12 and the cycle id is not
  zero. The special space is exactly 16 bytes and the last two are the
  cycle id, always zero here.
- `TestSearch` — an equal separator sends the search left. An internal
  page takes the *last* downlink `<=` the key, so an exact match descends
  to the right-hand child. Using `<` puts every duplicate on the wrong
  side of the tree.
- `TestSearch` — the search for a key that is not present returns "not
  found" instead of a position. A leaf search returns the first item
  `>=` the key, which is both where an equal key starts and where a new
  one goes.
- `TestSplitsGrowTree` — the tree grows but `Check` reports that a high
  key does not match its parent's downlink. The left page's high key is a
  pivot *copy of the right page's first item*, and the separator inserted
  into the parent is the same bytes. Building one from the last item of
  the left page instead is the usual mistake, and it is only detectable
  with duplicates, which is why the heap TID is part of the key.
- `TestSplitsGrowTree` — the right page keeps a key on its first
  downlink after an internal split. An internal page's first downlink is
  always minus infinity: its key is exactly what went up to the parent as
  the separator, so keeping it would duplicate it.
- `TestSplitsGrowTree` — the sibling chain breaks after a split. Four
  links change: the left page's `next`, the right page's `prev` and
  `next`, and the old right sibling's `prev`. Forgetting the last one is
  invisible until a scan walks backwards through the level, which is
  what `Check` does.
- `TestSplitsGrowTree` — a root split loses the tree. `_bt_newroot`
  allocates a new page one level up with a minus-infinity downlink to the
  old root and the new separator, and only then does the metapage point
  at it.
- `TestAscendingInsertsPackPages` — 57 blocks expected, far more
  produced. An append to the rightmost page splits at the fill factor,
  90 percent, not in the middle; splitting ascending inserts down the
  middle leaves every page half empty forever.
- `TestDuplicatesOrderByTID` — duplicates come back in insertion order
  or the checker reports items out of order. The comparison key is
  `(key, heap TID)`, so equal keys are ordered by TID and every entry has
  a unique position.
- `TestNullsSortLast` — a scan with no upper bound stops before the NULL
  entries, or NULLs come first. NULL sorts after every value, and it is
  indexed like any other key; a scan reaches the nulls after everything
  else.
- `TestErrorsLeaveTreeUnchanged` — a rejected oversized tuple leaves a
  half-built page. Check the size before the search, so the failure
  happens before anything is pinned.
- `TestCheckDetectsCorruption` — one case is not detected. Each
  invariant needs its own check: the metapage, the per-page level and
  flags, strictly increasing `(key, heap TID)`, everything below the high
  key, the high key equal to the parent's next downlink, the sibling
  chain matching the children the level above names, and every block
  reached exactly once.
- `TestRandomRangeScans` — passes on int4 and fails on text, or fails
  only with duplicates. Compare with the same rules chapter 10 uses, then
  break ties on the heap TID; a comparison that ignores the TID is
  consistent with itself and inconsistent with the high keys.
- `TestPersistence` — the tree is empty after a reopen. `Open` reads the
  root from the metapage; nothing may be cached in the `Tree` across a
  reopen, and every page change needs its `MarkDirty`.

## Why a tree that never deletes

`Insert` and `Search` with splits and root growth are the ideas in a B-tree;
deletion is where the page-level bookkeeping lives, and it does not stand
alone. A page emptied by a delete cannot simply be freed while another scan
may be walking towards it, so PostgreSQL defers the work to VACUUM, and
half-dead pages, page recycling and the `btpo_xact` stamp are the machinery
for that deferral. Doing it here would mean building VACUUM and the
transaction machinery it needs, four chapters early; doing it wrong would
mean a tree that loses rows under a concurrent scan. So the tree is insert-
only in v1 with a structural checker to prove the invariants (D10), and an
index entry for a deleted row stays until v2.

## Out of scope

Deletion and page recycling (VACUUM, v2), Lehman-Yao concurrency
(right-link moves, page locks across levels), the fast root, suffix
truncation of pivot tuples, deduplication, multi-column keys (D10),
descending order and `NULLS FIRST`, and backward scans.

## Check your understanding

1. A leaf root holds three 2600-byte items `a`, `c`, `e` and you insert
   `g`. How many blocks does the file have afterwards, and what is on
   each?
   <details><summary>Answer</summary>

   Four. Block 0 is the metapage, now naming block 3 as the root at level
   1. Block 1 is a leaf holding a high key `e` and the items `a` and `c`,
   with `next` = 2. Block 2 is a leaf holding `e` and `g`, with `prev` =
   1 and no high key because it is rightmost. Block 3 is the new root
   holding a minus-infinity downlink to block 1 and a separator `e`
   pointing at block 2.
   </details>
2. A search for `d` in that tree. Which downlink does the root take,
   which leaf does it reach, and what does the leaf search return?
   <details><summary>Answer</summary>

   The minus-infinity downlink, because `e` is not `<= d`; it reaches
   block 1; and the leaf search returns the position after `c`, which is
   where `d` would be inserted. A search that finds nothing still returns
   a position, which is what makes the same routine serve both `Search`
   and `Insert`.
   </details>
3. Why is the heap TID part of the comparison key, rather than leaving
   equal keys in whatever order they were inserted?
   <details><summary>Answer</summary>

   Because it makes every entry's position in the tree unique, and that
   is what lets the high-key invariant be exact. A high key is a copy of
   the right page's first item, and with duplicates spanning a page
   boundary there is no key value that separates the two pages; the pair
   `(key, TID)` always does. It also bounds the work of finding one
   specific entry among many duplicates, which is why PostgreSQL 12 made
   the change.
   </details>
4. Why does an append to the rightmost page split at 90 percent instead
   of in the middle?
   <details><summary>Answer</summary>

   Because ascending keys never come back. Splitting in the middle leaves
   the left page permanently half empty, since nothing will ever be
   inserted into it again, so an index on a serial column would occupy
   twice the space it needs. Splitting near the end fills the left page
   and starts a fresh right one. `TestAscendingInsertsPackPages` pins the
   resulting block count at 57.
   </details>
5. We never delete from the tree and never move a page right under a
   concurrent reader, where PostgreSQL does both. What do those cost it,
   and what do they cost us to skip?
   <details><summary>Answer</summary>

   Deletion needs VACUUM to prove no snapshot can still want an entry,
   then a two-phase page deletion so a descending reader cannot land on a
   page that is being unlinked; without it our index only grows, which is
   a space leak, not a correctness bug. Lehman and Yao's right-link
   trick is what lets a reader hold no lock across levels: if the page it
   lands on has split under it, the high key tells it so and it moves
   right instead of restarting from the root. Skipping it means we hold
   a page locked while we descend, which serialises writers against
   readers; the right-link fields are already in the special space, so
   the design does not have to change to add it.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Multi-column keys.** Extend the comparison to a list of columns with
   the heap TID still last. The plumbing is in `FormIndexTuple`, `Compare`
   and the search; the ideas do not change, which is why it is out of scope
   rather than a chapter.
2. **Suffix truncation.** A pivot tuple only needs enough of the key to
   separate the two pages. Truncate it in `_bt_split`'s separator and watch
   the fan-out of an index on long text keys.
3. **Read `_bt_moveright` and the Lehman-Yao right-links.** Ours holds no
   lock across levels because there is one writer. Work out exactly which
   interleaving of a concurrent split and a descending search the right-link
   rescues, and what our checker would say afterwards.
