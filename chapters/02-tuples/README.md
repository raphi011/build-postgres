# Chapter 02: Heap tuples

**Goal.** The on-disk heap tuple: a 19-byte header, an optional null
bitmap, and the aligned encoding of `int4`, `int8`, `bool`, and `text`
values, with `Form` and `Deform` converting between Go values and bytes.

**You edit.** `internal/tuple/tuple.go`: 16 functions with
`panic("not implemented")` bodies, `String` through `SetInfomask`. Nothing
else changes; `tuple_test.go` is the contract, and `make test-ch02` also
runs the tests of the finished `cmd/pgdb` over your code.

**Needs from earlier chapters.** `internal/page` (chapter 01):
`OffsetNumber` for `TID`, and `New`, `AddItem`, `GetItem` in one test that
stores a tuple on a page.

**Done when.** `make test-ch02` passes.

**Effort.** Medium, about 3 hours. `Form` and `Deform` (steps 2 and 3) are
the hard part; almost every failure is the null bitmap's length or an
alignment gap.

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
                              Pages (01) and tuples [02] on disk
```

A page stores opaque items. This chapter defines what a heap item is: a
tuple header followed by the column values, encoded so that the storage
layer, the executor, and eventually MVCC all agree on the bytes.

PostgreSQL source: `access/common/heaptuple.c`,
`include/access/htup_details.h`, `include/access/tupdesc.h`,
`include/storage/itemptr.h`, `include/access/transam.h`.

## Why the header matters now

Two of the header fields, `xmin` and `xmax`, are how PostgreSQL implements
MVCC. They record the transaction that created the tuple and the transaction
that deleted it, and every reader decides visibility by comparing them to its
snapshot. That is chapter 17. But the fields need to exist on disk from the
first tuple you write, so we lay them out now and leave them zero.

`ctid` is the tuple's own address `(block, item number)` until the tuple is
updated, at which point it is changed to point at the new version. Readers
that find an outdated version can follow the chain. Also unused until
chapter 17.

## Tuple layout

**Predict.** A four-column row `(int4, text, bool, int8)` holding
`(1, "hi", true, 2)` with no nulls, and the same row with the first and
third columns NULL. Which of the two tuples is longer, and by how much?

```
 0        4        8              14       16       18   19          hoff
 ┌────────┬────────┬──────────────┬────────┬────────┬────┬───────────┬──────────────
 │ xmin   │ xmax   │ ctid         │ imask2 │ imask  │hoff│ null bmap │ column data
 └────────┴────────┴──────────────┴────────┴────────┴────┴───────────┴──────────────
```

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 4 | xmin | Inserting transaction ID. Zero until chapter 16. |
| 4 | 4 | xmax | Deleting transaction ID. Zero until chapter 16. |
| 8 | 6 | ctid | `uint32` block number then `uint16` item number. Zero until chapter 17. |
| 14 | 2 | infomask2 | Low 11 bits: number of attributes. |
| 16 | 2 | infomask | Flag bits, below. |
| 18 | 1 | hoff | Offset of column data from the start of the tuple. |
| 19 | var | null bitmap | Present only if `HasNull` is set. One bit per attribute, rounded up to whole bytes. |
| hoff | var | data | Attributes in order, each aligned. |

`hoff` is the fixed 19 bytes plus the bitmap, rounded up to a multiple of 8.
A tuple with no nulls therefore has `hoff = 24`, as does one with up to 40
attributes and nulls.

Infomask bits, with PostgreSQL's values:

| Bit | Name | Meaning |
|---|---|---|
| 0x0001 | HasNull | Null bitmap present |
| 0x0100 | XminCommitted | Hint: xmin known committed (chapter 17) |
| 0x0200 | XminInvalid | Hint: xmin known aborted (chapter 17) |
| 0x0400 | XmaxCommitted | Hint: xmax known committed (chapter 17) |
| 0x0800 | XmaxInvalid | Hint: xmax invalid or aborted (chapter 17) |

In the null bitmap a set bit means the attribute is present; a clear bit
means NULL. Attribute *i* (0-based) is bit `i & 7` of byte `i >> 3`. That is
PostgreSQL's convention.

Worked example. The descriptor is `(a int4, b text, c bool, d int8)`,
which is `desc4` in the tests. `Form(desc4, {1, "hi", true, 2}, nil)`
produces 48 bytes, the golden value of `TestFormGoldenNoNulls`:

| Offset | Bytes | Field |
|---|---|---|
| 0 | `00 00 00 00` | xmin |
| 4 | `00 00 00 00` | xmax |
| 8 | `00 00 00 00 00 00` | ctid |
| 14 | `04 00` | infomask2: natts = 4 |
| 16 | `00 00` | infomask: no bits |
| 18 | `18` | hoff = 24 |
| 19 | `00 00 00 00 00` | padding to hoff |
| 24 | `01 00 00 00` | a = 1 |
| 28 | `02 00 00 00` | b length = 2 |
| 32 | `68 69` | b = `"hi"` |
| 34 | `01` | c = true |
| 35 | `00 00 00 00 00` | padding to 8-align d |
| 40 | `02 00 00 00 00 00 00 00` | d = 2 |

Now `Form(desc4, {nil, "hi", nil, 2}, {true, false, true, false})`, the
golden value of `TestFormGoldenWithNulls`, 40 bytes:

| Offset | Bytes | Field |
|---|---|---|
| 14 | `04 00` | infomask2: natts = 4, unchanged |
| 16 | `01 00` | infomask: `HasNull` |
| 18 | `18` | hoff = 24, still |
| 19 | `0A` | null bitmap |
| 20 | `00 00 00 00` | padding to hoff |
| 24 | `02 00 00 00` | b length = 2 |
| 28 | `68 69` | b = `"hi"` |
| 30 | `00 00` | padding to 8-align d |
| 32 | `02 00 00 00 00 00 00 00` | d = 2 |

Three things to read off this. `natts` is 4 in both: a NULL attribute is
still an attribute. The bitmap byte `0x0A` is `0000 1010`, and a *set*
bit means present, so bit 0 clear is `a` NULL, bit 1 set is `b` present,
bit 2 clear is `c` NULL, bit 3 set is `d` present. And `hoff` is 24 in
both tuples, because the fixed 19 bytes round up to 24 with or without a
one-byte bitmap; the bitmap costs nothing until the table has 41 columns,
where 19 + 6 = 25 rounds to 32. That is what `TestHoffGrowsWithWideBitmap`
pins.

The layout drops PostgreSQL's `t_cid` and the `xvac` overlay. Everything
else is in the same order at the same size.

## Types and their encoding

| Type | Go value | Size | Alignment | Encoding |
|---|---|---|---|---|
| `int4` | `int32` | 4 | 4 | little-endian |
| `int8` | `int64` | 8 | 8 | little-endian |
| `bool` | `bool` | 1 | 1 | 0 or 1 |
| `text` | `string` | var | 4 | `uint32` byte length, then the bytes |

Before each non-null attribute, the write position is rounded up to the
type's alignment, measured from the start of the tuple. NULL attributes
occupy no space and do no alignment. PostgreSQL's `text` uses a packed
varlena header; ours is a plain 4-byte length, which keeps the code short at
the cost of 3 bytes per short string.

Worked example. The four attributes of the no-nulls tuple above, in
descriptor order, with the write position measured from byte 0 of the
tuple:

| Attr | Type | Position | Aligned to | Bytes appended | Position after |
|---|---|---|---|---|---|
| a | int4 | 24 | 24 | `01 00 00 00` | 28 |
| b | text | 28 | 28 | `02 00 00 00 68 69` | 34 |
| c | bool | 34 | 34 | `01` | 35 |
| d | int8 | 35 | 40 | `02 00 00 00 00 00 00 00` | 48 |

Only `d` needs padding: 35 is not a multiple of 8, so five zero bytes go
in first. `b` is 4-aligned and already sits at 28, and its own length
prefix is what makes 34 the next position, not 32; the string bytes are
not padded. In the second tuple `a` and `c` are NULL, so they append
nothing and do not align anything: `b` starts at 24 instead of 28, and
`d` lands at 32 instead of 40. That is the whole eight-byte difference
between the two tuples.

The tuple's total length is not padded. The page (chapter 01) aligns items
when it stores them.

## API

`internal/tuple`

```go
type XID uint32                     // transaction ID; InvalidXID, BootstrapXID, FrozenXID
type BlockNumber uint32
type TID struct{ Block BlockNumber; Off page.OffsetNumber }

type TypeID uint8                   // Int4, Int8, Bool, Text
func (t TypeID) String() string     // "int4", "int8", "bool", "text"
func (t TypeID) Size() int          // -1 for text
func (t TypeID) Align() int

type Attr struct{ Name string; Type TypeID; NotNull bool }
type Desc struct{ Attrs []Attr }
func NewDesc(attrs ...Attr) *Desc
func (d *Desc) Len() int

type Datum = any                    // int32 | int64 | bool | string

type Tuple []byte
func Form(d *Desc, values []Datum, nulls []bool) (Tuple, error)
func Deform(d *Desc, t Tuple) (values []Datum, nulls []bool, err error)

func (t Tuple) Natts() int
func (t Tuple) Hoff() int
func (t Tuple) IsNull(i int) bool
func (t Tuple) Xmin() XID / SetXmin(XID)
func (t Tuple) Xmax() XID / SetXmax(XID)
func (t Tuple) Ctid() TID / SetCtid(TID)
func (t Tuple) Infomask() uint16 / SetInfomask(uint16)
```

Semantics the tests depend on:

- `Form` writes a header with `xmin`, `xmax`, `ctid`, and every hint bit
  zero. `nulls` may be nil, meaning no nulls. It returns `ErrArity` if
  `len(values)` or a non-nil `nulls` disagrees with `d.Len()`, and `ErrType`
  if a non-null value has the wrong Go type. It does not enforce `NotNull`;
  that is the executor's job.
- `Deform` returns copies: mutating the returned string or the tuple
  afterwards does not affect the other. If the tuple has fewer attributes
  than the descriptor, the missing trailing attributes are NULL. That is how
  PostgreSQL handles columns added after rows were written. More attributes
  than the descriptor is `ErrCorrupt`, as is data that runs past the end of
  the tuple.
- `IsNull(i)` is true for `i >= Natts()`.

## Implementation notes

**Go you will need.** `encoding/binary.LittleEndian`: `PutUint16`/`Uint32`
and friends read and write at an offset, `AppendUint32`/`AppendUint64`
grow a byte slice. The two-value type assertion `x, ok := v.(int32)`
reports a mismatch instead of panicking, and fails for a nil `any`.
`append(t, make([]byte, pad)...)` emits zero padding. `string(b)` copies
the bytes out of `b`. `fmt.Errorf("%w: ...", ErrCorrupt)` wraps a sentinel
so `errors.Is` still matches it. `&^` is AND NOT, for rounding up to a
power of two.

**Alignment helper.** One private function rounds an offset up to a
power-of-two alignment: `(n + a - 1) &^ (a - 1)`. `Form` and `Deform` both
apply it to offsets measured from byte 0 of the tuple, so a wrong helper
cancels out in the round-trip tests and only the golden tests catch it.

**`Form`.**
1. Check arity, then scan `nulls` for any `true`. `HasNull`, the bitmap,
   and the extra header bytes exist only if there is one; an all-false
   slice is the same as nil.
2. Compute `hoff` as in "Tuple layout" and allocate the tuple with length
   `hoff`, so the zero header, bitmap, and padding come free. Write
   `natts` (masked with `nattsMask`) into infomask2, `HasNull` into
   infomask if needed, `hoff` into byte 18, then set the bitmap bit for
   every non-null attribute.
3. For each attribute in descriptor order: skip it if NULL; pad `len(t)`
   up to the type's alignment; type-assert the value and append its
   encoding. A failed assertion is `typeError`, and nil fails every
   assertion, which is how a nil value without a `nulls` slice becomes
   `ErrType`.

**`Natts`, `Hoff`, `IsNull`.** `Natts` masks infomask2 with `nattsMask`.
`IsNull` is true for `i >= Natts()`, false for everything when `HasNull`
is clear, and otherwise tests bit `i & 7` of bitmap byte `i >> 3`, where a
clear bit means NULL.

**`Deform`.**
1. Reject a tuple shorter than `FixedHeaderSize` before reading any header
   field; a nil tuple must not index out of range.
2. Read `natts` and `hoff`. `natts > d.Len()` and `hoff > len(t)` are
   `ErrCorrupt`.
3. Allocate `values` and `nulls` at `d.Len()`, not `natts`. Walk the
   descriptor with a position starting at `hoff`; an attribute with
   `i >= natts` or `IsNull(i)` is NULL and does not move the position.
4. Otherwise align the position, find the size (`Size()`, or for `text`
   check that 4 bytes remain and read the length), check that the value
   fits in `len(t)`, decode, and advance. Converting through
   `int32(...Uint32(...))` restores the sign; `string(b[4:])` copies.

**Header accessors.** Each is one `binary.LittleEndian` call at
`offXmin`, `offXmax`, `offCtid`, or `offInfomask`. `Ctid` is a `uint32`
block number at `offCtid` and a `uint16` item number at `offCtid+4`.
`SetInfomask` writes the whole mask; `HasNull` survives because callers
OR new bits into `Infomask()`.

## Suggested order

One step at a time with the line under it; `make test-ch02` at the end.
Every test not named in that step or an earlier one still panics.

1. Types. `String`, `Size`, `Align`. Green: `TestTypeIDs`.

   ```sh
   go test -race ./internal/tuple/... -run 'TestTypeIDs'
   ```
2. Form and header readers. `Form`, `Natts`, `Hoff`, `IsNull`, `Infomask`.
   Green: `TestFormGoldenNoNulls`, `TestFormGoldenWithNulls`,
   `TestNullBitmapOnlyWhenNeeded`, `TestHoffGrowsWithWideBitmap`,
   `TestFormErrors`. The golden tests pin every byte of two small tuples,
   so get those right first; the traps are in `Form` above.

   ```sh
   go test -race ./internal/tuple/... -run 'TestFormGoldenNoNulls|TestFormGoldenWithNulls|TestNullBitmapOnlyWhenNeeded|TestHoffGrowsWithWideBitmap|TestFormErrors'
   ```
3. Deform. `Deform`. Green: `TestRoundTrip`, `TestDeformDoesNotAlias`,
   `TestDeformMissingTrailingAttrsAreNull`, `TestDeformErrors`,
   `TestTupleFitsOnPage`, and `TestRandomRoundTrip`, which forms and
   deforms 500 random descriptors and is the real judge of steps 2 and 3.
   The `ErrCorrupt` checks are listed under `Deform` above.

   ```sh
   go test -race ./internal/tuple/... -run 'TestRoundTrip|TestDeformDoesNotAlias|TestDeformMissingTrailingAttrsAreNull|TestDeformErrors|TestTupleFitsOnPage|TestRandomRoundTrip'
   ```
4. Transaction fields. `Xmin`, `SetXmin`, `Xmax`, `SetXmax`, `Ctid`,
   `SetCtid`, `SetInfomask`. Green: `TestHeaderFields`, which compares the
   19 header bytes literally (little-endian, `ctid` as block then item
   number).

   ```sh
   go test -race ./internal/tuple/... -run 'TestHeaderFields'
   ```
5. The demo tool. Nothing to implement: `cmd/pgdb` is finished, and its
   tests run `sample` and `dump` over the code you just wrote. Green:
   `TestSampleThenDump`, which compares the dump below byte for byte,
   `TestDumpNewPage`, `TestDumpCorruptHeader`,
   `TestDumpItemPointerOutOfRange`, `TestDumpBlockOutOfRange`,
   `TestSampleNeedsAFreshDir`, and `TestDumpNotAPageFile`, which are the
   tool's own refusals on files it should not walk.

   ```sh
   go test -race ./cmd/pgdb/...
   ```

### When a test fails

- `TestFormGoldenNoNulls` — the bytes match up to offset 34 and then
  diverge. `c` is a `bool` at 34, so the position afterwards is 35, and
  `d` is an `int8` needing 8-alignment: five bytes of padding, then the
  value at 40. Aligning from the wrong base, such as from `hoff` rather
  than from byte 0 of the tuple, gives the same answer here and the wrong
  one elsewhere, so fix the helper rather than the offset.
- `TestFormGoldenNoNulls` — byte 18 is 19 instead of 24. `hoff` is the
  fixed header plus the bitmap rounded *up to a multiple of 8*, and the
  tuple is allocated at that length so the padding is already zero.
- `TestFormGoldenWithNulls` — the bitmap byte is `0x05` rather than
  `0x0A`. You set a bit per NULL. The convention is inverted: a set bit
  means the attribute is present.
- `TestNullBitmapOnlyWhenNeeded` — `HasNull` is set for a `nulls` slice
  that is all false. Scan the slice for a `true` before deciding; an
  all-false slice must produce exactly the same bytes as `nil`.
- `TestHoffGrowsWithWideBitmap` — 40 attributes give `hoff` 32 instead of
  24. The bitmap is `(natts + 7) / 8` bytes, so 40 attributes need 5, and
  `19 + 5 = 24` needs no rounding at all. Only the 41st attribute pushes
  it to 6 bytes and `hoff` to 32.
- `TestFormErrors` — the `nil` value with no `nulls` slice returns
  `ErrArity` or no error. Use the two-value type assertion and let `nil`
  fail it; that turns a missing value into `ErrType` without a special
  case.
- `TestDeformErrors` — `Deform(desc4, nil)` panics with an index out of
  range instead of returning `ErrCorrupt`. Check `len(t) < FixedHeaderSize`
  before reading any header field, including `natts`.
- `TestDeformErrors` — the truncated 30-byte tuple decodes without
  complaint and returns garbage. Every attribute must be checked against
  `len(t)` after alignment and, for `text`, twice: once for the four
  length bytes and once for the string they announce.
- `TestDeformDoesNotAlias` — the string turns into `0xFF` bytes.
  `string(b)` copies; slicing does not.
- `TestDeformMissingTrailingAttrsAreNull` — the result has two entries
  instead of four. Allocate `values` and `nulls` at `d.Len()` and treat
  any index at or past `natts` as NULL. This is how a column added by
  `ALTER TABLE` reads back on rows written before it existed.
- `TestRandomRoundTrip` — passes for hundreds of descriptors and then
  fails on one with a NULL in the middle. A NULL attribute must not move
  the write position, so `Form` and `Deform` have to skip it identically;
  aligning before the null check in one of them and after it in the other
  is the usual cause.

Once step 4 is green, the whole chapter is visible on a real file:

```sh
go run ./cmd/pgdb sample /tmp/pgdb-demo
go run ./cmd/pgdb dump /tmp/pgdb-demo/base/16384
```

```
16384: 1 block

block 0
  lsn 0  lower 36  upper 8072  special 8192  free 8032  items 3
    n   offset    len flags      xmin   xmax     ctid  natts  hoff  infomask
    1     8152     37 normal        1      0    (0,1)      2    24    0x0000
    2     8112     36 normal        1      0    (0,2)      2    24    0x0000
    3     8072     37 normal        1      0    (0,3)      2    24    0x0000
```

`sample` forms three rows of `(a int4, b text)` with your `Form` and adds
them to a page with your chapter 01 code; `dump` reads the header fields
back with your `Xmin`, `Ctid`, `Natts` and `Hoff`. Both are provided
complete in `cmd/pgdb` and are not yours to write, and `TestSampleThenDump`
compares that output byte for byte. Every later chapter can point `dump` at
any relation file, including the ones the REPL writes from chapter 11 and
the B-tree's pages from chapter 12, where the tuple columns are left out.

## Why not a type catalog

The four types are a `TypeID` with size and alignment in a switch, where
PostgreSQL looks up `typlen`, `typalign`, `typbyval` and input and output
functions in `pg_type`. A type catalog would be a chapter of plumbing that
changes nothing about how a tuple is laid out, which is what this chapter is
about, and four types already exercise fixed width, variable width,
alignment padding and the null bitmap (D5). The cost is that adding a type
later means editing Go, not inserting a row.

## Out of scope

TOAST and out-of-line storage, 1-byte varlena headers, `t_cid`, `xvac`,
`HEAP_HOT_UPDATED` and other HOT flags, multi-transaction xmax, more than
four types.

## Check your understanding

1. A table has 41 `bool` columns and one row with every value non-NULL
   except the last. How long is the tuple?
   <details><summary>Answer</summary>

   72 bytes. 41 attributes need `(41 + 7) / 8 = 6` bitmap bytes, so the
   header is `19 + 6 = 25`, rounded up to `hoff = 32`. `bool` has
   alignment 1 and size 1, and the 40 non-NULL values append one byte
   each with no padding, so the tuple is `32 + 40 = 72`. The NULL
   contributes nothing beyond its clear bitmap bit.
   </details>
2. A tuple's bytes 14 to 19 read `03 00 01 00 18 04`. What does that tell
   you about the row?
   <details><summary>Answer</summary>

   `infomask2 = 3`, so three attributes; `infomask = 1`, so `HasNull` is
   set and there is a bitmap; `hoff = 0x18 = 24`, so column data starts
   at 24; and the single bitmap byte is `0x04 = 0000 0100`, whose bits 0
   and 1 are clear and bit 2 is set. The first two columns are NULL and
   the third is present.
   </details>
3. Why does `Deform` allocate its result at the descriptor's length
   rather than at the tuple's `natts`, and treat the difference as NULL?
   <details><summary>Answer</summary>

   Because `ALTER TABLE ... ADD COLUMN` must not rewrite the table. Rows
   written before the column existed carry the old, smaller `natts`, and
   the only sensible value for a column that was not there is NULL.
   PostgreSQL does exactly this. It also means `natts` greater than the
   descriptor is genuine corruption, not an old row, which is why that
   direction is `ErrCorrupt`.
   </details>
4. `Form` refuses a value of the wrong Go type with `ErrType` but happily
   writes a NULL into a column declared `NOT NULL`. Why is the check
   split that way?
   <details><summary>Answer</summary>

   The type is a property of the encoding: writing an `int64` where the
   descriptor says `int4` produces bytes that can never be decoded back,
   so `Form` is the last place that can catch it. `NOT NULL` is a
   constraint on the table, checked once per row by the executor with a
   user-facing message and an error position. Putting it here would put
   SQL semantics in the storage layer and give the wrong error text.
   </details>
5. Our `text` carries a plain 4-byte length. PostgreSQL uses a varlena
   header that is 1 byte for strings under 127 bytes. What does it get
   for that complexity, and what does it cost?
   <details><summary>Answer</summary>

   Three bytes per short string, plus the alignment those three bytes
   would have forced: a 1-byte varlena needs no alignment at all, so a
   short string can start anywhere, where ours rounds up to 4. On a table
   of short strings that is a large fraction of the page, and a page holds
   proportionally more rows. The cost is that every reader must branch on
   the first byte to learn the header's size, that the same value has two
   encodings, and that a value can additionally be compressed or stored
   out of line in a TOAST table, which is a whole subsystem. We keep the
   fixed header for the same reason we mirror the design and not the code
   (D4).
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **A 2-byte type.** Add `int2` with 2-byte alignment and rerun the worked
   example: which padding gaps move, and which golden tests notice?
2. **Short varlena.** Implement PostgreSQL's 1-byte header for `text` values
   under 127 bytes (`VARATT_IS_1B`), where the low bit of the first byte
   says which form it is, and count the bytes it saves on a table of short
   strings.
3. **Read `heap_form_tuple` and `heap_deform_tuple` in `heaptuple.c`.** Find
   the fast path that skips the null bitmap entirely, and the reason
   `heap_deform_tuple` stops early rather than filling every attribute.
