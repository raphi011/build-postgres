# Chapter 03: Relation files and the storage manager

**Goal.** A data directory that stores each relation as `base/<oid>` and
reads, writes, and appends its 8 KiB pages by block number, safely from
several goroutines.

**You edit.** `internal/smgr/smgr.go`: 12 functions with
`panic("not implemented")` bodies, `Open` through `SyncDir`. Nothing else
changes; `smgr_test.go` is the contract.

**Needs from earlier chapters.** `tuple.OID` and `tuple.BlockNumber` from
chapter 02 (types only), and `page.PageSize` from chapter 01.

**Done when.** `make test-ch03` passes.

**Effort.** Short, about 2 hours. Only `Extend` (step 3) needs thought,
and only because two goroutines may call it at once.

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
                                   Storage manager [03]
                                             │
                                             ▼
                              Pages (01) and tuples (02) on disk
```

Pages have to live somewhere. In PostgreSQL every table and every index is a
relation, every relation is a file (or a set of files) in the data
directory, and the storage manager is the only code that touches those
files. Everything above it thinks in `(relation, block number)`.

PostgreSQL source: `storage/smgr/smgr.c`, `storage/smgr/md.c`,
`include/postgres_ext.h` (the `Oid` type), and `storage/file/fd.c`.

## The data directory

```
<datadir>/
  base/
    16384        relation with OID 16384: block 0, block 1, ... concatenated
    16385
```

A relation file is nothing but its pages back to back: block *n* starts at
byte `n * 8192`. The file's length divided by the page size is its block
count; a length that is not a multiple of the page size means the file is
corrupt. PostgreSQL splits files into 1 GiB segments (`16384`, `16384.1`,
...) and keeps side files called forks for free-space and visibility maps.
We do neither.

Relations are named by an OID, a 32-bit object identifier that PostgreSQL
uses for every catalog object. The catalog (chapter 08) will hand these out.
For now callers pick them.

Worked example. `Open(dir)` on an empty directory, then `Create(16384)`,
two `Extend` calls, and `Create(16385)`:

| Call | Returns | `base/16384` | `base/16385` |
|---|---|---|---|
| `Open(dir)` | – | – | – |
| `Create(16384)` | `nil` | 0 bytes | – |
| `Extend(16384, p)` | block 0 | 8192 bytes | – |
| `Extend(16384, p)` | block 1 | 16384 bytes | – |
| `Create(16385)` | `nil` | 16384 bytes | 0 bytes |

`Open` creates `base/` and nothing else. `Create` makes a zero-length
file, which is a relation with zero blocks, not a corrupt one; `NBlocks`
returns 0 for it. Each `Extend` appends exactly one page, so the file
length is the block count times 8192 and `NBlocks(16384)` is now 2. Block
1 occupies bytes 8192 to 16383, and `Read(16384, 1, buf)` is
`ReadAt(buf, 8192)`.

The file length is the only place the block count is recorded, which is
why `NBlocks` stats the file on every call rather than caching. Truncate
`base/16384` to 12288 bytes behind the storage manager's back and the
next `NBlocks` reports it:

```
smgr: relation file size is not a multiple of the page size: <path> is 12288 bytes
```

## Storage manager API

**Predict.** `Write(rel, 5, buf)` on a relation that has three blocks.
What should happen, and what would the underlying `WriteAt` do if you
let it through?

`internal/smgr`

```go
type DataDir struct{ ... }

func Open(path string) (*DataDir, error)
func (d *DataDir) Path() string
func (d *DataDir) Close() error

func (d *DataDir) Create(rel tuple.OID) error
func (d *DataDir) Exists(rel tuple.OID) (bool, error)
func (d *DataDir) Unlink(rel tuple.OID) error

func (d *DataDir) NBlocks(rel tuple.OID) (tuple.BlockNumber, error)
func (d *DataDir) Read(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error
func (d *DataDir) Write(rel tuple.OID, blk tuple.BlockNumber, buf []byte) error
func (d *DataDir) Extend(rel tuple.OID, buf []byte) (tuple.BlockNumber, error)
func (d *DataDir) Sync(rel tuple.OID) error
func (d *DataDir) SyncAll() error

func SyncDir(path string) error
```

Semantics the tests depend on:

- `Open` creates `path` and `path/base` if they do not exist. It fails if
  `path` exists and is not a directory.
- `Create` makes an empty relation file and fails with `ErrExists` if there
  already is one. `Unlink` removes it. `Exists` reports whether it is there.
  Both sync `base/` afterwards: a file name only becomes durable when the
  directory holding it does.
- `SyncDir` flushes a directory, and is what `Create` and `Unlink` use.
  It is exported because the catalog's control file (chapter 08) and the
  commit log (chapter 16) live outside `base/` and need the same thing.
- `SyncAll` syncs every relation file the `DataDir` has open. A commit
  calls it through the buffer pool (chapter 16).
- `Read`, `Write`, `Extend`, `NBlocks`, and `Sync` fail with `ErrNotFound`
  for a relation that has not been created.
- `buf` must be exactly `page.PageSize` bytes for `Read`, `Write`, and
  `Extend`. Anything else is a programming error and panics.
- `Read` and `Write` fail with `ErrBlockOutOfRange` for a block at or past
  `NBlocks`. Growing a relation is `Extend` only, which appends one page and
  returns its block number. Block numbers are dense: the first `Extend`
  returns 0.
- `NBlocks` fails with `ErrCorrupt` if the file length is not a multiple of
  the page size.
- Every method is safe to call from multiple goroutines. Two concurrent
  `Extend` calls on the same relation return different block numbers and
  both pages are readable afterwards.
- `Close` closes any open file descriptors. Using the `DataDir` after
  `Close` is a programming error.

The `DataDir` keeps one open file descriptor per relation it has touched,
opened lazily, so that the buffer manager in the next chapter can call
`Read` and `Write` freely. Use `ReadAt` and `WriteAt`; they take an offset,
so no seek state is shared between goroutines.

Worked example. On the two-block relation above, one round trip and the
two calls that must fail:

| Call | Offset used | Result |
|---|---|---|
| `Extend(16384, p)` | 0 | block 0; file grows to 8192 |
| `Read(16384, 0, buf)` | `ReadAt(buf, 0)` | `nil`; `buf` holds page 0 |
| `Write(16384, 1, buf)` | `WriteAt(buf, 8192)` | `nil`; file stays 16384 |
| `Read(16384, 2, buf)` | none | `ErrBlockOutOfRange` |
| `Write(16384, 2, buf)` | none | `ErrBlockOutOfRange` |
| `NBlocks(99999)` | none | `ErrNotFound` |

The two failures print

```
smgr: block number out of range: block 2 of 2
smgr: relation does not exist
```

Block 2 is rejected on the *write* path as well as the read path, and
that is the whole point of the range check. `WriteAt(buf, 16384)` would
have succeeded, grown the file to 24576, and left block 2 as a page the
storage manager never initialised. `Extend` is the only way a relation
grows.

`Sync` calls `fsync` on the relation file, `SyncAll` on every open one.
Nothing calls either yet; chapter 16 does, at commit. Ordering the
*writes* — page before commit log, and the log itself before anything
else — is the write-ahead log's job in v2. Without one, the crude version
of the same guarantee is to flush everything before recording the commit.

`fsync` on a file does not make its *name* durable: a crash after
`Create` can leave a file whose directory entry never reached the disk,
and a crash after `Unlink` a name that is still there. That is what
`SyncDir` is for, and why `Create` and `Unlink` call it.

## Implementation notes

**Go you will need.** `os.MkdirAll`, `os.OpenFile` with
`os.O_CREATE|os.O_EXCL`, `errors.Is` against `os.ErrExist` and
`os.ErrNotExist`, `fmt.Errorf` with `%w` (so `errors.Is` still matches a
wrapped sentinel), `os.File.Stat`, `ReadAt`, `WriteAt`, and `Sync`,
`strconv.FormatUint`, `filepath.Join`, `sync.Mutex`. A `sync.Mutex` is not
reentrant: locking it twice on the same goroutine deadlocks.

**Private state.** Beside `path`, `DataDir` holds a `sync.Mutex` and a
`map[tuple.OID]*os.File` of lazily opened relation files (the paragraph
after the API listing explains why). Four private helpers carry the
whole chapter:

1. `relPath(rel)`: `filepath.Join(path, "base", strconv.FormatUint(...))`.
   Used by `Create`, `Exists`, `Unlink`, and `file`.
2. `file(rel)`: under the lock, return the cached descriptor or open
   `relPath(rel)` with `os.O_RDWR` and cache it. `os.ErrNotExist` becomes
   `ErrNotFound`. `NBlocks`, `Read`, `Write`, `Extend`, and `Sync` all
   start here, so this one function is the `ErrNotFound` contract.
   PostgreSQL: `_mdfd_getseg` in `md.c`, backed by the `fd.c` cache.
3. `nblocks(f)`: `f.Stat()`, then `Size() % page.PageSize`; a remainder is
   `ErrCorrupt`, wrapped with `%w` and the file name and size. Never cache
   the result.
4. `checkBuf(buf)`: `panic` unless `len(buf) == page.PageSize`. First
   line of `Read`, `Write`, and `Extend`.

**Open.** One `os.MkdirAll(filepath.Join(path, "base"), 0o755)` creates
both directories and fails on its own when `path` is a regular file;
that is the whole of `TestOpenRejectsFile`. Allocate the map. Nothing is
opened yet.

**Close.** Lock, close every cached file, delete each entry, return the
first close error.

**Create.** `os.OpenFile` with `os.O_RDWR|os.O_CREATE|os.O_EXCL`, mode
`0o644`; `os.ErrExist` becomes `ErrExists`. Take the lock only after the
open, then put the new descriptor in the map, closing any stale one that
was there. Caching it here saves the reopen in the `Extend` that follows.

**Exists.** `os.Stat(relPath(rel))`; `os.ErrNotExist` is `(false, nil)`,
any other error is returned as is.

**Unlink.** Lock, close and delete the cached descriptor, unlock, then
`os.Remove`; `os.ErrNotExist` becomes `ErrNotFound`. Dropping the
descriptor first matters: an open file survives its unlink on Unix, so a
stale entry would make the next `Create` plus `Extend` write into a file
nobody can see. `TestCreateExistsUnlink` re-creates the relation for
exactly this reason.

**NBlocks.** `file`, then `nblocks`. `TestCorruptFileSize` truncates the
file with `os.Truncate` after an `Extend`, which is why the stat must be
fresh on every call.

**Read and Write.** `checkBuf`, `file`, `nblocks`, then `blk >= n` is
`ErrBlockOutOfRange` (wrap it with `%w`, naming the block and the count).
Only then `f.ReadAt(buf, int64(blk)*page.PageSize)` or `WriteAt` at the
same offset. The range check is what stops `Write(rel, 1000, ...)` from
growing the file: `WriteAt` past the end would succeed and leave a hole.
No lock is held around the I/O; the offset travels with the call.

**Extend.** `checkBuf`, `file`, and only then take the lock and hold it
for the rest: `nblocks`, `WriteAt` at `n * page.PageSize`, return `n`.
Two goroutines that stat the length outside the lock see the same `n`
and write the same block; `TestConcurrentExtend` reports that as a
duplicate block number. Do not call `file` while holding the lock, it
locks too. The solution uses the one `DataDir` mutex rather than a
per-relation lock; serialising extends across relations is fine here.

**Sync.** `file`, then `f.Sync()`. `SyncAll` walks the map of open files
under the mutex and returns the first error. `SyncDir` is `os.Open` on the
directory, `Sync`, `Close`; a directory opens read-only and `fsync` on the
handle is what flushes the entries.

## Suggested order

One step at a time with the line under it; `make test-ch03` at the end.
Every test not named in that step or an earlier one still panics.

1. Data directory. `Open`, `Close`. Green: `TestOpenCreatesLayout`,
   `TestOpenRejectsFile`. Every other test opens through these two, so
   create `path` and `path/base` before anything else works.

   ```sh
   go test -race ./internal/smgr/... -run 'TestOpenCreatesLayout|TestOpenRejectsFile'
   ```
2. Relation files. `Create`, `Exists`, `Unlink`, `NBlocks`, and
   `SyncDir`, which the first two call. Green: `TestCreateExistsUnlink`,
   `TestSyncDir`. The first checks `base/16384` with `os.Stat` and
   creates the relation again after unlinking it; see `Unlink` above.

   ```sh
   go test -race ./internal/smgr/... -run 'TestCreateExistsUnlink|TestSyncDir'
   ```
3. Pages. `Extend`, `Read`, `Write`. Green: `TestBlockOutOfRange`,
   `TestWrongBufferSizePanics` (a buffer that is not `page.PageSize` bytes
   must panic, not return an error), `TestPersistsAcrossReopen`,
   `TestRelationsAreIndependent`, `TestCorruptFileSize`, and
   `TestConcurrentExtend`. The corrupt-size and concurrent tests are
   covered under `NBlocks` and `Extend` above.

   ```sh
   go test -race ./internal/smgr/... -run 'TestBlockOutOfRange|TestWrongBufferSizePanics|TestPersistsAcrossReopen|TestRelationsAreIndependent|TestCorruptFileSize|TestConcurrentExtend'
   ```
4. Sync. `Sync`, `SyncAll`. Green: `TestNotFound` (every method returns
   `ErrNotFound` for a relation that was never created), `TestSyncAll`,
   and `TestExtendReadWrite`, the end-to-end check of steps 2 to 4.

   ```sh
   go test -race ./internal/smgr/... -run 'TestNotFound|TestSyncAll|TestExtendReadWrite'
   ```

### When a test fails

- `TestOpenRejectsFile` — `Open` succeeds on a path that is a regular
  file. Let `os.MkdirAll` decide: it returns an error for that on its own,
  so no explicit `Stat` is needed. Adding one usually gets the case where
  the parent exists but `base` does not wrong.
- `TestCreateExistsUnlink` — everything passes until the relation is
  created a second time, and then reads come back as zeroes or the file
  on disk stays empty. An unlinked file on Unix survives as long as a
  descriptor is open, so `Unlink` must close and drop the cached
  descriptor before `os.Remove`. Otherwise the next `Extend` writes into
  the deleted inode.
- `TestNotFound` — one method returns a raw `os.ErrNotExist` or a nil
  error. Every method that needs a descriptor should go through the one
  private `file` helper, which is where `os.ErrNotExist` becomes
  `ErrNotFound`; that keeps the contract in one place.
- `TestNotFound` — `errors.Is` fails even though the message looks right.
  Wrap with `%w`, not `%v`.
- `TestWrongBufferSizePanics` — an error comes back instead of a panic.
  A wrong buffer length is a programming error, not a corrupt database,
  and the check must be the first line of `Read`, `Write` and `Extend`,
  before any file is opened.
- `TestBlockOutOfRange` — the read is rejected but the write is not, and
  the file has silently grown. `WriteAt` past the end of a file succeeds
  and leaves a hole. Range-check `Write` against `NBlocks` exactly as you
  do `Read`.
- `TestCorruptFileSize` — passes on a fresh relation and fails after the
  test truncates the file. The block count must come from a fresh
  `f.Stat()` on every call. Nothing may cache it, because the file can
  change under you.
- `TestConcurrentExtend` — two goroutines get the same block number, or
  a page written by one is missing afterwards. The stat and the write in
  `Extend` are one critical section: take the lock before `nblocks` and
  hold it through `WriteAt`.
- `TestConcurrentExtend` — deadlocks instead of failing. `sync.Mutex` is
  not reentrant, and the `file` helper takes the same lock. Resolve the
  descriptor before locking.
- `TestPersistsAcrossReopen` — data is missing after `Close` and a fresh
  `Open`. `Close` has to close every cached descriptor; the test relies on
  the writes having reached the file, not on `Sync`.

## Why not segment files

PostgreSQL splits a relation at 1 GiB into `16384`, `16384.1` and so on, and
much of `md.c` is the bookkeeping for it: the split exists because the
systems PostgreSQL was born on could not hold a larger file, not because a
storage manager wants it. We map one relation to one file and get the same
API, which is the rule for scale and history features we do not need (D4).
The consequence is that `NBlocks` is one `stat` rather than a walk over
segments, and a relation is as large as the filesystem allows.

## Out of scope

Segments, forks, tablespaces, databases (`base/<dboid>/<reloid>`),
`smgrtruncate`, `smgrprefetch`, the virtual file descriptor cache.

## Check your understanding

1. A relation file is 40960 bytes. How many blocks does it hold, at what
   byte offset does the last one start, and what does `Read(rel, 5, buf)`
   return?
   <details><summary>Answer</summary>

   `40960 / 8192 = 5` blocks, numbered 0 to 4. Block 4 starts at
   `4 * 8192 = 32768`. Block 5 is at the block count, not below it, so
   `Read` returns `ErrBlockOutOfRange` with "block 5 of 5" and never
   touches the file.
   </details>
2. Two goroutines call `Extend` on the same fresh relation at the same
   time. What must the two return values be, and how many bytes long is
   the file afterwards?
   <details><summary>Answer</summary>

   Blocks 0 and 1 in some order, never 0 twice, and the file is 16384
   bytes with both pages readable. That holds only if the length check
   and the write happen under one lock: two goroutines that stat first
   and lock later both see zero blocks, both write at offset 0, and one
   page is lost.
   </details>
3. Why does `NBlocks` stat the file every time instead of caching the
   count in the `DataDir`?
   <details><summary>Answer</summary>

   Because the file length *is* the block count; there is no other record
   of it. A cache would have to be invalidated by `Extend`, by `Unlink`,
   by a second `DataDir` on the same directory, and by anything outside
   the process that touches the file, and a stale count reads a block
   that is not there or refuses one that is. A `stat` is one system call
   against a structure the kernel already has in memory.
   </details>
4. `Read` and `Write` do their I/O with no lock held, while `Extend`
   holds one for its whole body. Why is that safe?
   <details><summary>Answer</summary>

   `ReadAt` and `WriteAt` carry the offset in the call, so they share no
   seek position and two goroutines cannot interleave into each other's
   position the way `Read` and `Seek` on a shared `*os.File` would. The
   lock exists for one thing only: making "what is the length" and
   "append at that length" atomic. Nothing else in the chapter is a
   read-modify-write on shared state.
   </details>
5. PostgreSQL splits a relation into 1 GiB segments (`16384`, `16384.1`,
   ...) and keeps a free-space map and a visibility map as separate
   forks. What does that buy, and what does it cost us to skip?
   <details><summary>Answer</summary>

   Segments exist because some filesystems could not hold a file larger
   than 2 GiB, and they let a `DROP` or a truncation release space in
   pieces; the cost is that every block number has to be split into a
   segment and an offset, and that a descriptor cache has to hold many
   files per relation. The free-space map is what lets an insert find a
   page with room without scanning the table, which is why our heap
   (chapter 05) has to scan instead; the visibility map is what lets a
   scan skip pages where every tuple is visible to everyone, which is an
   MVCC optimisation we do not have in chapter 17. Skipping all three
   costs performance and no correctness, which is why they are out of
   scope rather than deferred.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Segments.** Implement them behind the existing API with a small limit,
   say four blocks per segment, so the tests stay cheap. `Extend` across a
   boundary and `NBlocks` over a hole are the interesting cases.
2. **A descriptor cache.** Keep at most N files open, closing the least
   recently used and reopening on demand, which is what `fd.c` does for a
   backend that touches thousands of relations.
3. **Read `mdextend` and `mdnblocks` in `md.c`.** Work out what PostgreSQL
   does when a crash leaves a torn or partial block at the end of a file,
   and what our `Read` would do with one.
