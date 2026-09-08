# Chapter 08: System catalog

**Goal.** Tables that describe tables: `pg_class` and `pg_attribute`
stored as heap relations, a bootstrap that creates them on an empty data
directory, and a control file that hands out OIDs that never repeat.

**You edit.** `internal/catalog/control.go`: 2 functions with
`panic("not implemented")` bodies, `ReadControl` and `WriteControl`.
`internal/catalog/catalog.go`: 8 functions, `Bootstrap` through `Tables`.
Nothing else changes; `catalog_test.go` is the contract.

**Needs from earlier chapters.** `internal/tuple` (`Desc`, `Form`,
`Deform`, `BootstrapXID`, `FirstNormalXID`), `internal/heap` (`Create`,
`Open`, `Insert`, `Delete`, `Scan`), `internal/bufmgr` (`Pool.FlushAll`,
`Pool.Discard`, `Pool.Store`) and, through the pool's store,
`internal/smgr` (`Path`, `Create`, `Unlink`). No earlier package was
stubbed for this chapter.

**Done when.** `make test-ch08` passes.

**Effort.** Long, about 5 hours. `Bootstrap` (step 2) is the hard part:
the catalogs describe themselves, so you write `pg_class` rows with code
that cannot yet read `pg_class`.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser (07) ──► Analyzer (09) ──► Planner (14, 15)
                                     │                 │
                              Catalog [08]             ▼
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
                              Pages (01) and tuples (02) on disk
```

Everything so far addresses a relation by a number the caller made up and a
descriptor the caller kept in a variable. A database has to remember those
itself: which tables exist, what they are called, which columns they have
and of what type. PostgreSQL keeps that information in ordinary heap tables,
the system catalogs, and reads them with the same scan code it uses for
user data. This chapter builds the two catalogs everything else needs and
the bootstrap that creates them on an empty data directory.

PostgreSQL source: `include/catalog/pg_class.h`,
`include/catalog/pg_attribute.h`, `catalog/heap.c`
(`heap_create_with_catalog`, `CheckAttributeNamesTypes`, `heap_drop_with_catalog`),
`catalog/catalog.c` (`GetNewOidWithIndex`), `access/transam/varsup.c`
(`GetNewObjectId`), `bootstrap/bootstrap.c`, `utils/cache/relcache.c`,
`include/catalog/pg_control.h`.

## Catalogs are tables

**Predict.** `pg_class` holds a row describing `pg_class`. Reading that
row needs `pg_class`'s column layout. Where does the layout come from
before any row has been read?

`pg_class` has one row per relation and `pg_attribute` one row per column.
Both are heap relations created with chapter 05's `heap.Create`, stored in
`base/<oid>` like any table, and read with `heap.Scan`. That is the whole
trick: once the two catalogs exist, `CREATE TABLE` is two heap inserts and a
file creation, and finding a table by name is a sequential scan of
`pg_class`.

The catalogs describe themselves. `pg_class` contains a row for `pg_class`
and a row for `pg_attribute`; `pg_attribute` contains the columns of both.
The chicken-and-egg problem is solved the way PostgreSQL's bootstrap mode
solves it: the descriptors of the two catalogs are compiled into the
program (`ClassDesc`, `AttributeDesc`) with fixed OIDs, so they can be
opened before any catalog row has been read. After bootstrap, the rows on
disk merely agree with the compiled-in descriptors.

The catalog OIDs are PostgreSQL's: `pg_class` is 1259 and `pg_attribute`
is 1249. Every OID below 16384 (`FirstUserOID`, PostgreSQL's
`FirstNormalObjectId`) is reserved for system objects; user tables start
there, which is why the first table you create in a fresh PostgreSQL
cluster gets OID 16384 too.

## Layout

`pg_class` (OID 1259), in `pg_class` column order:

| attnum | name | type | meaning |
|---|---|---|---|
| 1 | `oid` | `int4` | the relation's OID, which is also its file name |
| 2 | `relname` | `text` | table name as given |
| 3 | `relkind` | `text` | `r` for a table; chapter 13 adds `i` for an index |
| 4 | `relpages` | `int4` | page count as of the last `ANALYZE` (chapter 14) |
| 5 | `reltuples` | `int8` | tuple count as of the last `ANALYZE` |

`pg_attribute` (OID 1249):

| attnum | name | type | meaning |
|---|---|---|---|
| 1 | `attrelid` | `int4` | OID of the relation the column belongs to |
| 2 | `attname` | `text` | column name |
| 3 | `atttypid` | `int4` | the `tuple.TypeID` value |
| 4 | `attnum` | `int4` | 1-based column position |
| 5 | `attnotnull` | `bool` | `NOT NULL` constraint |

OIDs are stored as `int4`, so they range up to 2^31-1. PostgreSQL's `oid`
type is unsigned; the difference does not matter at any scale this book
reaches. There is no `pg_type` (D5), so `atttypid` holds the enum value
directly.

Bootstrap writes rows in this order: the `pg_class` row for `pg_class`,
then for `pg_attribute`, then the five `pg_attribute` rows of `pg_class`,
then the five of `pg_attribute`. All are stamped with `tuple.BootstrapXID`.
A user table's rows are the `pg_class` row followed by one `pg_attribute`
row per column in column order, stamped with the XID the caller passes.
Because chapter 05's scan returns tuples in insertion order, the tests can
check the catalog contents as a list.

Worked example. Bootstrap a fresh data directory, then
`CreateTable("users", {id int4 NOT NULL, name text}, 10)`. Scanning
`pg_class` gives four rows:

| TID | xmin | oid | relname | relkind | relpages | reltuples |
|---|---|---|---|---|---|---|
| `(0,1)` | 1 | 1259 | `pg_class` | `r` | 0 | 0 |
| `(0,2)` | 1 | 1249 | `pg_attribute` | `r` | 0 | 0 |
| `(0,3)` | 1 | 2610 | `pg_index` | `r` | 0 | 0 |
| `(0,4)` | 10 | 16384 | `users` | `r` | 0 | 0 |

and `pg_attribute` seventeen, of which these are the first two and the
last two:

| TID | xmin | attrelid | attname | atttypid | attnum | attnotnull |
|---|---|---|---|---|---|---|
| `(0,1)` | 1 | 1259 | `oid` | 1 | 1 | true |
| `(0,2)` | 1 | 1259 | `relname` | 4 | 2 | true |
| ... | | | | | | |
| `(0,16)` | 10 | 16384 | `id` | 1 | 1 | true |
| `(0,17)` | 10 | 16384 | `name` | 4 | 2 | false |

The bootstrap rows carry `xmin` 1, which is `tuple.BootstrapXID`; the
`users` rows carry the 10 the caller passed. `atttypid` is the
`tuple.TypeID` value: 1 is `int4`, 2 is `int8`, 3 is `bool`, 4 is `text`.
The fifteen bootstrap `pg_attribute` rows are five columns each of
`pg_class`, `pg_attribute` and `pg_index`, in that order, which is why
the first user column lands at item 16.

The data directory afterwards:

```
base/1249       8192 bytes    pg_attribute, one page of rows
base/1259       8192 bytes    pg_class, one page of rows
base/2610          0 bytes    pg_index, created and still empty
base/16384         0 bytes    users, created and still empty
global/control    20 bytes
```

Every one of those files was made by `heap.Create`, and every row above
was read back with `heap.Scan`. Nothing in this chapter has its own
storage format.

## The control file

Something has to hand out OIDs that never repeat, even across restarts.
PostgreSQL keeps the next OID and the next transaction ID in `pg_control`
under `global/`, together with the cluster's identity and checkpoint state.
Ours is `<datadir>/global/control`, 20 bytes, little-endian:

| offset | size | field |
|---|---|---|
| 0 | 4 | magic: the ASCII bytes `PGDB` |
| 4 | 4 | version, 1 |
| 8 | 4 | next OID |
| 12 | 4 | next XID |
| 16 | 4 | CRC-32 (IEEE) of bytes 0..16 |

The file is rewritten on every OID allocation, by writing `control.tmp`
and renaming it over `control`, so a crash leaves either the old or the
new file. PostgreSQL instead reserves 8192 OIDs at a time and logs the
reservation to WAL; we have no WAL and DDL is rare, so one rename per
allocation is fine. The next XID field is written as `tuple.FirstNormalXID`
and left alone until chapter 16, which reuses the file.

Worked example. The twenty bytes right after bootstrap:

```
50 47 44 42  01 00 00 00  00 40 00 00  03 00 00 00  a4 3d c7 3d
```

| Offset | Bytes | Field | Value |
|---|---|---|---|
| 0 | `50 47 44 42` | magic | `PGDB` |
| 4 | `01 00 00 00` | version | 1 |
| 8 | `00 40 00 00` | next OID | 16384 |
| 12 | `03 00 00 00` | next XID | 3 |
| 16 | `a4 3d c7 3d` | CRC-32 | of bytes 0 to 15 |

`0x4000` is 16384, `FirstUserOID`, so the first user table gets that OID
and the field becomes 16385. Next XID is 3, `tuple.FirstNormalXID`, and
stays there until chapter 16. The CRC covers the first sixteen bytes
only, and it is what turns a torn or hand-edited file into
`ErrControlCorrupt` rather than into an absurd OID.

Bootstrap is `initdb`: it refuses to run if the control file exists.
`Open` refuses to run if it does not. Nothing looks at the base directory
to decide which case it is in; the control file is the marker.

## The relation cache

Resolving a name means scanning `pg_class` for the name, then scanning
`pg_attribute` for the OID and sorting the rows by `attnum`. PostgreSQL
avoids doing that on every statement with the relation cache
(`relcache.c`), a per-backend hash keyed by OID, invalidated by messages
from any backend that changes the catalog. We have one process, so the
cache is a map from name to `*RelationInfo` inside `Catalog`, filled by
`Lookup` and cleared entirely by `CreateTable` and `DropTable`. Clearing
everything is cruder than PostgreSQL's per-relation invalidation and
completely adequate.

`Lookup` returns the cached pointer. Callers must treat the `RelationInfo`
and its `Desc` as read-only.

## Dropping a table

`DropTable` deletes the `pg_class` row and every `pg_attribute` row of the
relation with `heap.Delete`, which stamps `xmax` and leaves the bytes in
place (chapter 05), then unlinks the relation file and discards its pages
from the buffer pool. The rows stay in the catalog pages until VACUUM
exists (v2); scans skip them. Dropping a catalog is `ErrSystemTable`, as
PostgreSQL refuses with `permission denied: "pg_class" is a system catalog`.

Worked example. `DropTable("users", 20)` on the catalog above. A scan of
`pg_class` now returns three rows, the three system ones; row `(0,4)` is
still on the page with `xmax` 20 and is skipped. The same happens to
`pg_attribute` rows `(0,16)` and `(0,17)`. `base/16384` is gone once the
commit hook runs, and `Tables()` returns

```
1249 pg_attribute r
1259 pg_class     r
2610 pg_index     r
```

sorted by name, not by OID. `CreateTable("users", ...)` immediately
afterwards succeeds and gets OID 16385: the name is free at once, but
the OID counter never goes back.

## API

`internal/catalog`

```go
const (
	ClassOID     tuple.OID = 1259
	AttributeOID tuple.OID = 1249
	FirstUserOID tuple.OID = 16384
)

var ClassDesc, AttributeDesc *tuple.Desc

type RelKind byte
const RelKindTable RelKind = 'r'

type RelationInfo struct {
	OID    tuple.OID
	Name   string
	Kind   RelKind
	Pages  int32
	Tuples int64
	Desc   *tuple.Desc
}

type Control struct { NextOID tuple.OID; NextXID tuple.XID }
func ReadControl(dir string) (Control, error)
func WriteControl(dir string, c Control) error

type Catalog struct{ ... }
func Bootstrap(pool *bufmgr.Pool) (*Catalog, error)
func Open(pool *bufmgr.Pool) (*Catalog, error)
func (c *Catalog) Pool() *bufmgr.Pool
func (c *Catalog) NewOID() (tuple.OID, error)
func (c *Catalog) CreateTable(name string, desc *tuple.Desc, xid tuple.XID) (tuple.OID, error)
func (c *Catalog) DropTable(name string, xid tuple.XID) error
func (c *Catalog) Lookup(name string) (*RelationInfo, error)
func (c *Catalog) LookupOID(oid tuple.OID) (*RelationInfo, error)
func (c *Catalog) Tables() ([]*RelationInfo, error)
```

Errors: `ErrBootstrapped`, `ErrNotBootstrapped`, `ErrControlCorrupt`,
`ErrExists`, `ErrNotFound`, `ErrDuplicateColumn`, `ErrTooManyColumns`,
`ErrSystemTable`. And `const MaxColumns = 1600`.

Semantics the tests depend on:

- `Bootstrap` creates `global/control` with next OID `FirstUserOID` and
  next XID `FirstNormalXID`, creates the two catalog relations, inserts
  the rows listed above in that order, and calls `FlushAll`. A data
  directory with a control file is `ErrBootstrapped`.
- `Open` reads the control file (`ErrNotBootstrapped` if missing,
  `ErrControlCorrupt` on a bad magic, version, or CRC) and does no other
  I/O.
- `ReadControl` and `WriteControl` implement the layout above; the golden
  bytes in `catalog_test.go` are the reference.
- `NewOID` returns the next OID and persists the counter before returning.
  OIDs strictly increase, across restarts too. `NewOID` is safe to call
  from several goroutines.
- `CreateTable` allocates an OID, creates the relation file through the
  pool's store, and inserts the catalog rows. More than `MaxColumns`
  columns is `ErrTooManyColumns`; a name already in `pg_class` (catalogs
  included) is `ErrExists`; two columns with the same name is
  `ErrDuplicateColumn`; all three are checked before anything is
  allocated or written. A table with no columns is allowed, and one with
  exactly `MaxColumns` is too.
- `DropTable` deletes the rows, unlinks the file, and discards the pool's
  pages for it. Unknown name is `ErrNotFound`; a catalog is
  `ErrSystemTable`. The name is free for reuse immediately; the new table
  gets a new OID.
- `Lookup` and `LookupOID` return `ErrNotFound` for unknown or dropped
  tables and a `RelationInfo` whose `Desc` equals the one passed to
  `CreateTable` (names, types, `NotNull`). `Lookup` of a catalog returns
  `ClassDesc` or `AttributeDesc` contents. A second `Lookup` of the same
  name pins no buffers.
- `Tables` returns every relation in `pg_class`, catalogs included, sorted
  by name.
- Neither `CreateTable` nor `DropTable` flushes; callers `FlushAll` before
  relying on the catalog after a reopen, as the heap tests do. The control
  file is always current.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** `encoding/binary.LittleEndian` (`Uint32`,
`PutUint32`), `hash/crc32.ChecksumIEEE`, `os.ReadFile`, `os.MkdirAll`,
`os.OpenFile` with `os.O_WRONLY|os.O_CREATE|os.O_TRUNC`, `os.File.Sync`,
`os.Rename`, `errors.Is` against `os.ErrNotExist`, `sort.Slice`,
`sync.Mutex` (not reentrant), a `map[string]bool` as a set, and type
assertions on `tuple.Datum`: `v[0].(int32)`, `v[1].(string)`. `defer`
pairs `Lock` with `Unlock` and `Scan` with `Close`.

<details><summary><b>Private state.</b></summary>

Beside `pool`, `Catalog` holds two `*heap.Relation` for `pg_class` and
`pg_attribute`, opened with `heap.Open` against `ClassDesc` and
`AttributeDesc`; the in-memory `Control`; one `sync.Mutex`; and the
`map[string]*RelationInfo` cache. A `newCatalog(pool, ctl)` constructor
builds all of that and is shared by `Bootstrap` and `Open`. Every exported
method takes the mutex and holds it to the end.
</details>

<details><summary><b>ReadControl.</b></summary>

`os.ReadFile` on `<dir>/global/control`; `os.ErrNotExist` becomes
`ErrNotBootstrapped`, any other read error is returned as is. Then four
checks, each `ErrControlCorrupt`: length is exactly 20, bytes 0..4 are
`PGDB`, `Uint32(b[4:8])` is 1, and `Uint32(b[16:20])` equals
`ChecksumIEEE(b[:16])`. Only then decode the OID from 8..12 and the XID
from 12..16. The length check is what catches the truncated, trailing-byte,
and empty cases in `TestControlCorrupt`.
</details>

<details><summary><b>WriteControl.</b></summary>

Build the 20 bytes in a `make([]byte, 20)`: `copy` the magic, `PutUint32`
the version, OID, and XID, then the CRC of the first 16 bytes into the last
four. `os.MkdirAll` on `global/`, open `control.tmp` with
`O_WRONLY|O_CREATE|O_TRUNC` and mode `0o644`, `Write`, `Sync`, `Close`,
`os.Rename` over `control`. Close the file on every error path. PostgreSQL:
`update_controlfile` in `controldata_utils.c`.
</details>

<details><summary><b>Bootstrap.</b></summary>

`dir` is `pool.Store().Path()`.

1. `ReadControl(dir)`: `nil` error is `ErrBootstrapped`; anything but
   `ErrNotBootstrapped` (a corrupt file, an I/O error) is returned as is.
2. `WriteControl` with `FirstUserOID` and `tuple.FirstNormalXID`; the
   golden bytes encode exactly that pair.
3. `heap.Create` for `ClassOID` and `AttributeOID`, then `newCatalog`.
4. `insertClass` for `pg_class`, then `pg_attribute`; `insertAttributes`
   for `ClassDesc`, then `AttributeDesc`. All with `tuple.BootstrapXID`.
5. `pool.FlushAll()`. `TestBootstrapIsDurable` reopens without flushing.
</details>

<details><summary><b>insertClass and insertAttributes.</b></summary>

`insertClass(oid, name, xid)` forms one `ClassDesc` tuple, `int32(oid),
name, "r", int32(0), int64(0)`, and inserts it into `pg_class`.
`insertAttributes(oid, desc, xid)` forms one `AttributeDesc` tuple per
`desc.Attrs[i]`: `int32(oid), a.Name, int32(a.Type), int32(i+1),
a.NotNull`. Datum types must match the column types exactly or `tuple.Form`
refuses; `RelKind` goes in as a one-byte `string`, not a `byte`.
</details>

<details><summary><b>Open.</b></summary>

`ReadControl`, return its error unchanged (`ErrNotBootstrapped` and
`ErrControlCorrupt` both come from there), `newCatalog`. No scan, no buffer
touched.
</details>

<details><summary><b>scanClass and findClass.</b></summary>

`scanClass(fn)` opens a `pg_class` scan, deforms each tuple with
`ClassDesc`, builds a `RelationInfo` without `Desc`
(`RelKind(v[2].(string)[0])` for the kind), and calls `fn(tid, info)`; `fn`
returns `false` to stop early. `findClassTID(pred)` runs it until `pred`
matches and returns the TID and the info, or `ErrNotFound`; `findClass` is
the same without the TID. `Lookup`, `LookupOID`, `CreateTable`, and
`DropTable` all go through these two.
</details>

<details><summary><b>scanAttributes and loadAttributes.</b></summary>

`scanAttributes(oid)` scans all of `pg_attribute`, skips rows whose
`attrelid` differs, keeps `(tid, attnum, tuple.Attr)` per row, and sorts by
`attnum` with `sort.Slice`. `loadAttributes(info)` appends those attrs to a
`tuple.NewDesc()` and stores it in `info.Desc`. The tests compare that
`Desc` with `reflect.DeepEqual` against the one passed to `CreateTable`, so
`Name`, `Type`, and `NotNull` must round-trip exactly.
</details>

<details><summary><b>Lookup.</b></summary>

Under the lock: return the cached pointer if the name is in the map, before
any scan (`TestLookupCache` compares `Pool().Stats()` around the second
call). Otherwise `findClass` by name, `loadAttributes`, put the pointer in
the map, return it. A failed lookup caches nothing. Names are compared with
`==`; the parser folds case, not the catalog.
</details>

<details><summary><b>LookupOID.</b></summary>

Same shape, keyed by OID: walk the cache values first, then `findClass`
with an OID predicate, `loadAttributes`, and cache under `info.Name` so a
later `Lookup` by name hits.
</details>

<details><summary><b>Tables.</b></summary>

`scanClass` over every row: use the cached pointer when the name is in the
map, else `loadAttributes` and cache it. Then `sort.Slice` by `Name`.
Entries are the same pointers `Lookup` hands out. Because scans see only
tuples without `xmax`, dropped tables do not appear.
</details>

<details><summary><b>NewOID.</b></summary>

Lock, then the private `newOID`, which `CreateTable` also calls while
already holding the lock: copy `control`, increment `NextOID` on the copy,
`WriteControl` the copy, and only on success assign it back and return the
old value. A failed write leaves the counter untouched.
`TestNewOIDConcurrent` wants 200 dense OIDs from 8 goroutines; the mutex
around read, write, and store is the whole answer.
</details>

<details><summary><b>CreateTable.</b></summary>

Order matters; `TestCreateTableErrors` expects the next OID to be
`FirstUserOID+1` after every failed call.

1. `len(desc.Attrs) > MaxColumns` is `ErrTooManyColumns`. This is the one
   place the limit is enforced, and it has to exist somewhere: the tuple
   header holds the attribute count in 11 bits and the offset of the data
   in a single byte, so a 2048-column table stores a count of zero and
   reads back as all NULLs, and a 1900-column row with one NULL in it
   truncates the header offset and returns the header bytes as data. No
   error at any step, which is the worst kind. PostgreSQL's limit is the
   same 1600 and its message is `tables can have at most 1600 columns`.
   Then walk `desc.Attrs` with a `map[string]bool`; a repeat is
   `ErrDuplicateColumn`. No lock needed yet.
2. Lock. `findClass` by name: `nil` error is `ErrExists`; any error other
   than `ErrNotFound` is returned.
3. `newOID`, `heap.Create(pool, oid, desc)`, `insertClass`,
   `insertAttributes` with the caller's `xid`.
4. `invalidate` (replace the cache map with an empty one). No flush.
</details>

<details><summary><b>DropTable.</b></summary>

Lock, `findClassTID` by name (`ErrNotFound` propagates), then `info.OID <
FirstUserOID` is `ErrSystemTable`, checked before any write.
`class.Delete(tid, xid)`, then `scanAttributes(info.OID)` and `attr.Delete`
each TID, then `invalidate`, `pool.Discard(info.OID)`, and
`Store().Unlink(info.OID)`. Discard before unlink: a dirty page of the
dropped relation flushed later would hit `smgr.ErrNotFound`. `heap.Delete`
leaves the row in place; `TestDropTable` fetches it at `{0, 3}` and expects
`xmax` 20.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch08` at the end.
Every test not named in that step or an earlier one still panics.

1. Control file. `ReadControl`, `WriteControl`. Green:
   `TestControlRoundTrip`. The test checks that no `control.tmp` is left
   behind; see `WriteControl` above.

   ```sh
   go test -race ./internal/catalog/... -run 'TestControlRoundTrip'
   ```
2. Bootstrap. `Bootstrap`. Green: `TestControlGolden`,
   `TestBootstrapTwice`. The golden test compares the 20 control bytes
   after bootstrap, CRC included.

   ```sh
   go test -race ./internal/catalog/... -run 'TestControlGolden|TestBootstrapTwice'
   ```
3. Open. `Open`. Green: `TestControlCorrupt`, `TestBootstrapIsDurable`,
   `TestOpenNotBootstrapped`. The durability test is covered under
   `Bootstrap` above.

   ```sh
   go test -race ./internal/catalog/... -run 'TestControlCorrupt|TestBootstrapIsDurable|TestOpenNotBootstrapped'
   ```
4. Reading the catalogs. `Lookup`, `LookupOID`, `Tables`. Green:
   `TestBootstrapDescribesItself`, `TestLookupNotFound`. Build the
   name cache here; it is judged in steps 6 and 7.

   ```sh
   go test -race ./internal/catalog/... -run 'TestBootstrapDescribesItself|TestLookupNotFound'
   ```
5. OIDs. `NewOID`. Green: `TestNewOIDConcurrent`.

   ```sh
   go test -race ./internal/catalog/... -run 'TestNewOIDConcurrent'
   ```
6. Create. `CreateTable`. Green: `TestCreateTable`,
   `TestCreateTableNoColumns`, `TestCreateTableErrors`, `TestTablesSorted`,
   `TestPersistsAcrossReopen`, `TestCreatedTableIsUsable`,
   `TestOIDsNeverRepeat`, `TestLookupConcurrent`. The order of checks
   against allocation is under `CreateTable` above.

   ```sh
   go test -race ./internal/catalog/... -run 'TestCreateTable$|TestCreateTableNoColumns|TestCreateTableErrors|TestTablesSorted|TestPersistsAcrossReopen|TestCreatedTableIsUsable|TestOIDsNeverRepeat|TestLookupConcurrent'
   ```
7. Drop. `DropTable`. Green: `TestDropTable`, `TestDropTableErrors`,
   `TestLookupCache`. The cache test is covered under `Lookup` above.

   ```sh
   go test -race ./internal/catalog/... -run 'TestDropTable$|TestDropTableErrors|TestLookupCache'
   ```

The package also holds `TestCreateIndex`, `TestIndexesOrder`,
`TestCreateIndexErrors`, `TestDropIndex`, `TestDropTableDropsIndexes` and
`TestIndexCache` for chapter 13, `TestUpdateStats` for chapter 14, and
`TestUpdateControl`, `TestCatalogSnapshot`, `TestEndTransactionAbort` and
`TestInvalidate` for chapters 16 and 17. None of them is part of
`make test-ch08`.

### When a test fails

- `TestControlGolden` — the first sixteen bytes match and the CRC does
  not. The checksum covers bytes 0 to 15 only, computed over the buffer
  you are about to write, not over the file after the write.
- `TestControlRoundTrip` — a `control.tmp` is left in `global/`. Write
  the temporary file, sync it, then rename it over `control`; the rename
  is what makes the update atomic, and a failure path that returns before
  the rename has to remove the temporary itself.
- `TestControlCorrupt` — a file with a good CRC but a bad magic is
  accepted. Check magic, version and CRC, and report all three as
  `ErrControlCorrupt`; there is no version negotiation.
- `TestOpenNotBootstrapped` — `Open` succeeds on a directory that has
  `base/` but no control file, or `Bootstrap` succeeds on one that has
  both. The control file alone decides, and nothing looks at `base/`.
- `TestBootstrapDescribesItself` — `Lookup("pg_class")` fails or returns
  a descriptor built from disk that does not match `ClassDesc`. The two
  catalog descriptors are compiled in and are used to *read* the catalog;
  the rows on disk only have to agree with them.
- `TestBootstrapDescribesItself` — the `pg_attribute` rows come back in
  the wrong order. Insert all three `pg_class` rows first, then the five
  columns of each catalog in catalog order, and rely on the heap scan
  returning insertion order.
- `TestBootstrapIsDurable` — the catalog is empty after a reopen.
  `Bootstrap` ends with `FlushAll`; the control file is written through
  its own `Sync`, but the catalog pages are still only in the pool.
- `TestNewOIDConcurrent` — two goroutines get the same OID. The
  increment and the control-file write are one critical section, and the
  file must be written before the OID is returned, or a crash hands the
  same OID out twice.
- `TestOIDsNeverRepeat` — OIDs restart after a reopen. `Open` has to take
  its counter from the control file rather than from the highest OID in
  `pg_class`; a dropped table's OID must never come back.
- `TestCreateTableErrors` — a duplicate name or a duplicate column leaves
  an OID consumed or a file behind. Both checks come before the
  allocation, so a rejected `CreateTable` changes nothing at all.
- `TestTablesSorted` — the order is by OID. Sort by name.
- `TestLookupCache` — a second `Lookup` still scans, or a `Lookup` after
  a `CreateTable` returns the stale entry. Fill the cache in `Lookup` and
  clear it entirely in `CreateTable` and `DropTable`; the test counts
  pinned buffers to tell the two apart.
- `TestLookupConcurrent` — the race detector fires on the cache map. A
  Go map is not safe for concurrent write; guard it, and remember the
  cached `*RelationInfo` is handed out to callers, so it must never be
  mutated after it is published.
- `TestDropTableErrors` — dropping `pg_class` succeeds. Any relation with
  an OID below `FirstUserOID` is `ErrSystemTable`.
- `TestDropTable` — the rows are gone but the file is still there, or the
  file is gone too early. `DropTable` records the file for removal and
  the commit hook does the unlink and the `Discard`; the test calls
  `EndTransaction(true)` right afterwards.

## Why not a hard-coded schema

The catalogs are tables that describe tables, including themselves, which is
the whole reason `Bootstrap` has to write `pg_class` rows with code that
cannot yet read `pg_class`. A Go map from table name to descriptor would
skip that problem entirely and read more simply, and every later chapter
would then pay for it: `pg_index` (chapter 13), the statistics `ANALYZE`
writes (chapter 14) and the transactional DDL of chapter 17 are rows and
tuple headers here, and would each be a special case there. We mirror
PostgreSQL's design where the design is the lesson (D4).

## Out of scope

`pg_type`, `pg_namespace` and schemas, `pg_database`, `pg_index` (chapter
13), `relpages`/`reltuples` maintenance (chapter 14), OID prefetch and
wraparound, system columns (`ctid`, `xmin` as selectable columns),
`ALTER TABLE`, temporary tables, and any transactional behaviour of DDL:
a `CreateTable` whose caller later rolls back stays created until chapter
16 gives XIDs meaning.

## Check your understanding

1. A fresh cluster is bootstrapped and two tables are created and then
   dropped. What is `NextOID` in the control file, and what does
   `Tables()` return?
   <details><summary>Answer</summary>

   16386, and the three system relations sorted by name:
   `pg_attribute`, `pg_class`, `pg_index`. The counter moved twice and
   never moves back, because an OID that came back could name a file that
   an old buffer or an old catalog row still refers to. The dropped
   names are free for immediate reuse; the OIDs are not.
   </details>
2. `pg_attribute` holds fifteen rows immediately after bootstrap. Which
   relations do they describe, and why is a user table's first column at
   item 16?
   <details><summary>Answer</summary>

   Five columns each of `pg_class`, `pg_attribute` and `pg_index`, in
   that order. `pg_index` is bootstrapped with its descriptor even though
   chapter 13 is what fills it, so the row layout does not shift when
   indexes arrive. The heap scan returns tuples in insertion order, so
   the first user column is item 16 on the first page.
   </details>
3. How does `Lookup("pg_class")` read a row out of `pg_class` before any
   row of `pg_class` has been read?
   <details><summary>Answer</summary>

   It does not need one. `ClassDesc` is compiled into the program along
   with the fixed OID 1259, so `heap.Open(pool, 1259, ClassDesc)` works
   with no catalog access at all, and the scan can then decode the rows.
   The rows on disk are the authority for what exists, never for how
   `pg_class` itself is laid out. PostgreSQL does exactly this in
   bootstrap mode, generating the descriptors from the `.h` files.
   </details>
4. Why does `CreateTable` clear the whole name cache instead of adding
   the new entry to it?
   <details><summary>Answer</summary>

   Because it is correct without any reasoning about what else changed,
   and DDL is rare enough that the cost is invisible. Adding the entry
   would be an optimisation that has to be right about every other entry
   the statement could have invalidated. PostgreSQL cannot afford the
   blunt version: it has many backends and would have to broadcast a
   flush to all of them, so it sends per-relation invalidation messages
   instead.
   </details>
5. PostgreSQL reserves 8192 OIDs at a time and writes the reservation to
   the write-ahead log, where we rewrite the control file on every single
   allocation. What does the reservation buy, and what does it cost?
   <details><summary>Answer</summary>

   One durable write per 8192 OIDs instead of per OID, which matters
   because PostgreSQL assigns OIDs to far more than tables: types,
   operators, constraints, large objects, every row of many catalogs.
   The cost is that a crash discards the rest of the reservation, so OIDs
   have gaps, and that the counter wraps at 2^32 and has to be checked
   for collisions against the catalog. We have no WAL to log a
   reservation to, DDL is rare, and a rename per allocation is one
   `fsync` in a code path nobody runs in a loop.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **System columns.** Make `ctid`, `xmin` and `xmax` selectable, as
   PostgreSQL does, by giving them negative attribute numbers the analyzer
   (chapter 09) resolves against the tuple header rather than the
   descriptor.
2. **Schemas.** Add `pg_namespace` and a two-part name, and watch how much
   of `Lookup`, the name cache and every error message has to learn about
   search paths.
3. **Read `initdb`'s BKI input** (`src/include/catalog/pg_class.dat` and
   `genbki.pl`). PostgreSQL generates its bootstrap data from the same
   declarations that generate the C structs. Which of our hand-written
   bootstrap rows would that have caught?
