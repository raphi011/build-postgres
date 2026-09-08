# Chapter 17: MVCC snapshots and visibility

**Goal.** Concurrent sessions each see a consistent snapshot: a
transaction reads what had committed when its snapshot was taken plus
its own writes, a rolled-back write is never seen by anyone, and two
transactions changing the same row are serialised, the second waiting
for the first and then following PostgreSQL's READ COMMITTED or
REPEATABLE READ rule.

**You edit.** `internal/mvcc/mvcc.go`: 7 functions with
`panic("not implemented")` bodies, `Take` through `Wait`.
`internal/txn/txn.go`: 3, `Snapshot`, `Wait`, and `Waiting`.
`internal/heap/heap.go`: 1, `Lock`. `internal/catalog/catalog.go`: 3,
`SetSnapshot`, `EndTransaction`, and `Invalidate`.
`internal/session/session.go`: 4, `OpenCluster`, `Connect`,
`Cluster.Close`, and `Blocked`. `internal/sql/ast/ast.go`: 1,
`Isolation.String`. And your own bodies from earlier chapters:
`heap.Scan`, `Next`, `Fetch`, `Delete`, and `Update` take a snapshot;
`index.Check`, `CheckUnique`, `Insert`, and `Build` too; the catalog's scans read
through `SetSnapshot`'s function and its drops defer their unlinks to
`EndTransaction`; the executor's `seqScan`, `indexScan`, and
`modifyTable` use `Env.Snapshot` and `Env.Isolation`; the lexer gains
six keywords and the parser `BEGIN ISOLATION LEVEL`; `session.Open`,
`Close`, and `Exec` move onto the cluster and take snapshots. The tests
of every package are the contract, with the regression file
`ch17_mvcc.sql` and the isolation suite under `testdata/isolation`,
whose runner `internal/isolation` is provided complete.

**Needs from earlier chapters.** `internal/txn` (`Manager`, `Status`,
`Begin`), `internal/tuple` (the hint bits `XminCommitted`,
`XminInvalid`, `XmaxCommitted`, `XmaxInvalid` of chapter 2, unused
until now), `internal/heap`, `internal/catalog`, `internal/index`,
`internal/executor`, and the session of chapter 16. No earlier package
was stubbed for this chapter.

**Done when.** `make test-ch17` passes.

**Effort.** Long, about 8 hours, best split over two sittings. The
visibility rule (step 2) is the hard part to get right and the session
layer (step 8), where one transaction waits for another, is the hard part
to debug.

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
                              Transactions and MVCC [16, 17]
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

Chapter 16 stamped every tuple with the transaction that wrote it and
recorded in `pg_xact` whether that transaction committed, and then left
the executor's rule alone: a tuple was live while its `xmax` was zero,
so a rolled-back `INSERT` stayed visible and a rolled-back `DELETE` hid
its row for good. This chapter combines the stamps and the log into the
rule PostgreSQL uses, and it opens the data directory to more than one
session at a time. The two go together. With one session the log
alone would do; with two, a reader must also decide what to make of a
transaction that is still running, or that committed a moment after
the reader's statement began, and that is what a snapshot is for.

PostgreSQL never locks a row against readers and never overwrites a
row in place. A writer leaves the old version where it is and stamps
it; a reader looks at the stamps and at its snapshot and decides for
itself which version, if any, it sees. Readers do not block writers
and writers do not block readers; only two writers of the same row
meet, and the second waits for the first. Everything in this chapter
is a consequence of that design: the snapshot, the visibility function,
the hint bits that cache its verdicts, the wait on a transaction, and
the recheck a READ COMMITTED update performs when the row it waited for
has changed under it.

PostgreSQL source: `utils/time/snapmgr.c` (`GetTransactionSnapshot`,
`GetCatalogSnapshot`, `XidInMVCCSnapshot`), `storage/ipc/procarray.c`
(`GetSnapshotData`), `access/heap/heapam_visibility.c`
(`HeapTupleSatisfiesMVCC`, `HeapTupleSatisfiesDirty`,
`HeapTupleSatisfiesUpdate`, `SetHintBits`), `access/heap/heapam.c`
(`heap_delete`, `heap_update`, `heap_lock_tuple`), `storage/lmgr/lmgr.c`
(`XactLockTableWait`), `executor/nodeModifyTable.c` (`ExecDelete`,
`ExecUpdate`), `executor/execMain.c` (`EvalPlanQual`),
`access/nbtree/nbtinsert.c` (`_bt_check_unique`),
`access/heap/heapam_handler.c` (`heapam_index_build_range_scan`),
`catalog/storage.c` (`smgrDoPendingDeletes`), `utils/cache/inval.c`
(`AcceptInvalidationMessages`), `include/utils/snapshot.h`,
`include/access/tableam.h` (`TM_Result`), `access/transam/README`
(the section on MVCC), and `src/test/isolation/isolationtester.c`.

## The snapshot

A snapshot is three numbers and a list, read from the transaction
manager under one lock (`Manager.Snapshot`, PostgreSQL's
`GetSnapshotData`):

| field | meaning |
|---|---|
| `Xmin` | the oldest running transaction, or `Xmax` if none: every ID below it has finished |
| `Xmax` | the next ID to be handed out: every ID at or above it had not started |
| `Xip` | the IDs between them that were running, ascending, the snapshot's own left out |
| `Xid` | the transaction the snapshot belongs to; `InvalidXID` outside one |

`Snapshot.InProgress(xid)` (`XidInMVCCSnapshot`) answers "was that
transaction still running, or not yet started, when I looked?": true
at or above `Xmax`, false below `Xmin`, a binary search in `Xip`
between. A transaction for which it is true is invisible whatever it
did afterwards, because the snapshot is the reader's fixed point in
time. A transaction for which it is false has finished, and the commit
log says how; that verdict cannot change, which is what makes it safe
to cache in the tuple.

`String` renders the snapshot as `pg_current_snapshot()` does,
`xmin:xmax:xip`, the running IDs joined by commas; `TestTake` reads
`10:14:10,13`.

Worked example. On a fresh cluster the first transactions are 3, 4, 5.
Transaction 3 commits, 4 aborts, 5 keeps running, and transaction 6,
ours, takes a snapshot:

| Field | Value | Meaning |
|---|---|---|
| `Xmin` | 5 | 3 and 4 have finished; nothing below 5 can still be running |
| `Xmax` | 7 | 7 had not been handed out |
| `Xip` | `[5]` | 5 was running; our own 6 is left out |
| `Xid` | 6 | our transaction |

which prints as `5:7:5`. `InProgress` then answers 3 with false (below
`Xmin`), 5 with true (in `Xip`), and 9 with true (at or above `Xmax`).
The last is the one worth pausing on: a transaction that had not started
when we looked counts as "in progress" forever, from this snapshot's
point of view, which is exactly what makes the snapshot a fixed point in
time.

The same snapshot on a line of transaction IDs:

```
     ┌──── finished ────┐  ┌─ running ─┐   ┌── not handed out yet ──┐
 xid   1    2    3    4      5      6         7      8      9  ...
 ──────────────────────────────────────────────────────────────────► time
                 ✓    ✗      ▲      ▲         ▲
              commit abort   │      ours      Xmax = 7
                             Xmin = 5, and Xip = [5]
```

`InProgress` is false in the left band, a binary search of `Xip` in the
middle one, and true in the right: a transaction that had not started
when we looked never becomes visible to this snapshot, however soon
afterwards it commits.

## The visibility rule

**Predict.** Your snapshot is `5:7:5`. A tuple has xmin 3, which
committed, and xmax 5, which is still running. Visible or not? And what
about a tuple with xmin 5?

`Snapshot.Visible(t)` is `HeapTupleSatisfiesMVCC` without command IDs.
In order:

1. xmin. If it is the snapshot's own, the tuple is the transaction's
   own insert and step 2 applies. Otherwise, if `XminCommitted` is not
   set and `InProgress(xmin)` is true, invisible, and nothing is
   recorded: the verdict is not final. Else look xmin up: `XminInvalid`
   set, or the log says aborted, is invisible; committed is on to step
   2, unless `InProgress(xmin)` is true after all, which happens when a
   later snapshot already set `XminCommitted` on a transaction ours saw
   running.
2. xmax. Zero is visible. The snapshot's own is invisible: the
   transaction deleted the row itself, and without command IDs every
   own delete is in the past (see "The Halloween problem" below). If
   `XmaxCommitted` is not set and `InProgress(xmax)` is true, visible:
   the delete had not happened for us. Else look xmax up: aborted (or
   `XmaxInvalid`) is visible; committed is visible only if
   `InProgress(xmax)`, that is, the delete committed after the
   snapshot.

Worked example. The snapshot `5:7:5` above, with 3 committed, 4 aborted,
5 running and 6 our own, against seven tuples:

| xmin | xmax | Visible | Why |
|---|---|---|---|
| 3 | 0 | yes | xmin committed and below `Xmin`; no delete |
| 4 | 0 | no | xmin aborted, step 1 |
| 3 | 5 | yes | xmin committed; xmax is `InProgress`, so the delete has not happened for us |
| 5 | 0 | no | xmin is `InProgress`, step 1 |
| 3 | 3 | no | xmin committed, xmax committed and not `InProgress`: deleted before our snapshot |
| 6 | 0 | yes | our own insert |
| 6 | 6 | no | our own delete, which is always in the past |

Rows three and five are the pair to read together: the same tuple shape,
`xmax` set in both, and the answer turns entirely on whether that
transaction had finished when the snapshot was taken. Row four is the
mirror image on the insert side. Rows six and seven need no log lookup at
all, and row seven is only correct because a statement never sees a
tuple its own transaction stamped during that statement, which is what
the materialisation in `modifyTable` guarantees.

The reserved IDs need no lookup: `Manager.Status` already reports
`BootstrapXID` and `FrozenXID` committed, and both are below every
snapshot's `Xmin`.

`Dirty` (`HeapTupleSatisfiesDirty`) is the rule for the unique check,
which must see what running transactions are doing: it reports a
tuple whose xmin or xmax is running with `wait` set to that ID and the
caller waits and asks again; otherwise the verdicts decide, the
snapshot's bounds playing no part, so a row committed after the
snapshot conflicts too. `Modify` (`HeapTupleSatisfiesUpdate`) is the
rule for a writer, returning a `heap.TM` verdict: `TMInvisible` if xmin
aborted or is running elsewhere, `TMOk` if xmax is zero or aborted,
`TMSelfModified` if xmax is the snapshot's own, `TMBeingModified` if
xmax is running, and `TMUpdated` or `TMDeleted` if it committed,
according to whether the tuple's ctid points elsewhere.

## Hint bits

Looking a transaction up in the commit log costs a lock and a read.
The tuple header has had four bits for the answer since chapter 2:
`XminCommitted`, `XminInvalid`, `XmaxCommitted`, `XmaxInvalid`. The
visibility functions read them first and set them whenever the log is
consulted, and the heap writes them back to the page. A verdict is
final, so a hint can never lie, with one exception handled by the
heap: `XmaxInvalid` describes the xmax that was there when it was set,
and a later `Delete` that overwrites an aborted xmax clears both xmax
bits with it.

The heap does not write under the shared lock it reads with. `Scan`
and `Fetch` copy the tuple, call `Visible` on the copy, and note which
copies changed their infomask; then a private `setHints` takes the
exclusive lock and ORs the new bits into each tuple whose xmin and xmax
are still the ones the verdict was reached on, and marks the buffer
dirty. PostgreSQL sets hints under the shared lock, relying on a single
byte write being atomic; the race detector has no such tolerance.

Worked example. The same seven tuples, each with infomask 0 before the
check:

| xmin | xmax | Infomask after | Bits set |
|---|---|---|---|
| 3 | 0 | `0x0100` | `XminCommitted` |
| 4 | 0 | `0x0200` | `XminInvalid` |
| 3 | 5 | `0x0100` | `XminCommitted` only; 5 has no verdict yet |
| 5 | 0 | `0x0000` | nothing; the log was never consulted |
| 3 | 3 | `0x0500` | `XminCommitted` and `XmaxCommitted` |
| 6 | 0 | `0x0000` | our own transaction needs no lookup |
| 6 | 6 | `0x0000` | likewise |

Only the rows whose verdict came from the log carry a hint, and only for
the transaction whose verdict was final. Row three keeps its `xmax`
unhinted because transaction 5 may still commit or abort, and a hint
that could change would be a lie. Rows four, six and seven never touched
the log, so a second check costs the same as the first, which is why
`TestScanSnapshot` counts lookups rather than comparing bits alone: the
first scan over eight tuples consults the table of verdicts for every
finished transaction, the second only for the two still running.

## Writers

`heap.Delete` and `Update` take the snapshot too. Under the exclusive
lock they ask `Modify`; on `TMBeingModified` they release the lock,
`Wait` for the transaction in xmax, and look again, since it may have
committed (the tuple is gone or replaced) or aborted (the tuple is
free). No lock is held while waiting: the transaction waited for may
need the same page to finish its own update. `TMOk` stamps xmax and
clears the xmax hint bits; `TMUpdated`, `TMDeleted`, and
`TMSelfModified` are `ErrAlreadyDeleted`; `TMInvisible` is the new
`ErrInvisible`, which a scan never hands a writer. With a nil snapshot
the rule of chapter 5 stands.

`Update` keeps chapter 5's order: it asks whether it may replace the old
version, inserts the new one, and only then stamps xmax and the ctid.
The check no longer settles the matter, though, because another
transaction can take the row while the insert runs; the stamp asks
again, and an update that loses there stamps the version it has just
inserted with its own xmax, so no snapshot ever sees it (D25). That is
not an undo of a delete — nothing had been deleted, and nobody had yet
been told about the new tuple. `Lock` is the wait and the check without the
stamp, `heap_lock_tuple`'s role: the executor calls it before the unique
check of an `UPDATE`, so that the check does not count a committed new
version the update is about to replace.

The wait itself is `Manager.Wait(xid)`: each running transaction has a
channel that `Commit`, `Abort`, and `Close` close; `Wait` on an ID that
is not running returns at once. PostgreSQL takes the transaction's lock
in the lock manager and waits on it (`XactLockTableWait`); a channel is
the same thing without the lock table. `Waiting` counts the goroutines
blocked there, and `Session.Blocked` reports one session's state, which
is what the isolation runner reads to tell a step that blocks from one
that is slow, as `pg_isolation_test_session_is_blocked` does.

## What the executor does after the wait

`modifyTable.write` applies the operation and, on `ErrAlreadyDeleted`
with a snapshot in `Env`, decides by `Env.Isolation`:

- REPEATABLE READ (`ast.RepeatableRead` and above): the row the
  statement chose has been changed by a transaction the snapshot does
  not see; the statement cannot proceed without reading past its
  snapshot. `*Error` wrapping `ErrSerializationFailure`, message
  `could not serialize access due to concurrent update` (SQLSTATE
  40001).
- READ COMMITTED: follow the chain. Fetch the old version; a ctid
  pointing at itself means deleted, and the row is skipped. Otherwise
  fetch the version the ctid names, evaluate the statement's quals
  against it (the `Filter` quals above the scan and an `IndexScan`'s
  own, collected at `Build` time and compiled against the table's
  layout), skip it if they fail, and apply the operation to it, `SET`
  expressions and all, with the wait loop again if it too has moved
  on. PostgreSQL's `EvalPlanQual` re-executes the plan with the new
  version substituted; for a single-table `UPDATE` or `DELETE` that is
  exactly this.

`count` grows only for rows written, so a skipped row is not in the
command tag.

Reads are simpler: `seqScan` passes `Env.Snapshot` to `heap.Scan`, and
`indexScan` to `heap.Fetch`, skipping entries whose tuple is not
visible. With a nil `Env.Snapshot` everything behaves as in chapter 11,
which is how that chapter's tests still pass.

## The Halloween problem

Chapter 11's `modifyTable` reads every input row before writing the
first, so that an `UPDATE` never meets the versions it creates. MVCC
alone would not fix that: a new version has xmin equal to the
statement's own transaction, and "own insert is visible" would let the
statement update it again, and again. PostgreSQL adds a command ID to
every tuple and compares it with the statement's; here the
materialisation stays, and with it the invariant that a statement
never sees a tuple its own transaction stamped during that statement.
That is why `Visible` can treat every own xmax as "in the past" and
why `TMSelfModified` never reaches the executor's loop.

## The unique check between transactions

`CheckUnique` and `Build` take the snapshot too. For each entry of a
unique index with the key, `CheckUnique` fetches the tuple raw and asks
`Dirty`: a live tuple other than `except` is the violation; a tuple a
running transaction is inserting or deleting makes the check close the
scan, `Wait` for that transaction, and start the index over, so that
two transactions inserting the same key are serialised and the second
sees the first's verdict. `Build` indexes every version some snapshot
might still see, which is all but those whose insert aborted, and
checks uniqueness among the tuples `Dirty` reports live, waiting for
running transactions as `CheckUnique` does; PostgreSQL spools dead
tuples separately for the same reason. `Build` reads the relation
through `heap.Any`, the snapshot that sees everything.

## Snapshots in the session

`Cluster` is what sessions share: the store, the pool, and the
transaction manager. `OpenCluster` opens (or bootstraps) the data
directory, `Connect` attaches a session, `Cluster.Close` flushes and
closes. `session.Open` keeps its chapter 11 shape by opening a cluster
of its own and closing it with the session. Each session owns a
`Catalog` over the shared pool: the relation cache is per session, as
PostgreSQL's relcache is per backend, and `Invalidate` drops it at the
start of every statement, in place of the invalidation messages that
other backends' commits would deliver.

`statement` takes the snapshot after starting the transaction:
`mvcc.Take(manager, xid)` for every statement under READ COMMITTED, and
under REPEATABLE READ once, at the first statement of the block, kept
in the session until the block ends. `BEGIN ISOLATION LEVEL READ
COMMITTED`, `READ UNCOMMITTED` (the same level, as in PostgreSQL), and
`REPEATABLE READ` set the level for the block; plain `BEGIN` is READ
COMMITTED. The executor's `Env` carries the snapshot and the level.

Catalog reads do not use the transaction's snapshot. `SetSnapshot`
gives the catalog a function it calls at the start of every scan, and
the session's returns a fresh snapshot for the current transaction:
DDL another session committed a moment ago is seen by the next
statement even inside a REPEATABLE READ block, and an index it created
is maintained by the next `INSERT`, as PostgreSQL's catalog snapshot
arranges. A session's own uncommitted DDL is visible to itself and to
nobody else, and a rolled-back `CREATE TABLE` leaves a `pg_class` row
nobody will ever see.

That row's file is another matter. `CreateTable` and `CreateIndex`
create files and `DropTable` and `DropIndex` remove them, and neither
can be undone by a commit log. So the catalog keeps a list of pending
files: a created file is removed if the transaction aborts, a dropped
one when it commits, and `EndTransaction(commit)`, which the session
calls whenever a transaction ends, does the removing (discarding the
pool's buffers first, as `DropTable` did directly until now). A
rolled-back `DROP TABLE` thus keeps its table, rows and indexes
included, and a rolled-back `CREATE TABLE` leaves no file behind. This
is PostgreSQL's pending-deletes list in `storage.c`.

## The isolation test suite

D6 promised a third test layer from this chapter: specs in the format
of `src/test/isolation`, run by `internal/isolation`, which is
provided. A spec declares `setup` and `teardown` SQL, sessions with
named steps, and permutations:

```
session s1
step s1b { BEGIN; }
step s1u { UPDATE t SET b = b + 1 WHERE a = 1; }
step s1c { COMMIT; }

session s2
step s2u { UPDATE t SET b = b + 1 WHERE a = 1; }

permutation s1b s1u s2u s1c
```

Each permutation runs on a fresh cluster, one session per declared
session, the steps in the given order; a step that blocks is reported
as `<waiting ...>` and the runner moves on, reporting `<... completed>`
with the step's output once a later step has unblocked it. Query
results print as `isolationtester` prints them, columns joined by `|`,
numbers right-aligned, then the row count. `testdata/isolation/specs`
holds four specs, `expected` their output, and `-update` regenerates
it like the regression suite's.

Worked example, the first permutation of `ch17_isolation.spec`. The
setup creates `t(a int4)` holding one row, and the permutation is
`rc s i2 s d1 s c s`, where `s1` reads and `s2` writes:

```
starting permutation: rc s i2 s d1 s c s
step rc: BEGIN ISOLATION LEVEL READ COMMITTED;
step s: SELECT a FROM t ORDER BY a;
a
-
1
(1 row)

step i2: INSERT INTO t VALUES (2);
step s: SELECT a FROM t ORDER BY a;
a
-
1
2
(2 rows)

step d1: DELETE FROM t WHERE a = 1;
step s: SELECT a FROM t ORDER BY a;
a
-
2
(1 row)
```

Session 1 is inside a `READ COMMITTED` block the whole time and sees
each of session 2's commits as it happens, because a new snapshot is
taken for every statement. Running the same permutation with
`BEGIN ISOLATION LEVEL REPEATABLE READ` gives `1` three times: one
snapshot is taken at the first statement and kept. The third permutation
in the file makes the "at the first statement, not at `BEGIN`" part
explicit by inserting a row between the two.

## API

`internal/mvcc`

```go
type Log interface {
    Status(xid tuple.XID) (txn.Status, error)
    Wait(xid tuple.XID)
}
type Snapshot struct { Xmin, Xmax tuple.XID; Xip []tuple.XID; Xid tuple.XID; Log Log }
func Take(m *txn.Manager, xid tuple.XID) *Snapshot
func (s *Snapshot) String() string
func (s *Snapshot) InProgress(xid tuple.XID) bool
func (s *Snapshot) Visible(t tuple.Tuple) (bool, error)
func (s *Snapshot) Dirty(t tuple.Tuple) (visible bool, wait tuple.XID, err error)
func (s *Snapshot) Modify(t tuple.Tuple, tid tuple.TID) (heap.TM, error)
func (s *Snapshot) Wait(xid tuple.XID)
```

`internal/txn`

```go
func (m *Manager) Snapshot() (xmin, xmax tuple.XID, xip []tuple.XID)
func (m *Manager) Wait(xid tuple.XID)
func (m *Manager) Waiting() int
```

`internal/heap`

```go
var ErrInvisible error
type Snapshot interface {
    Visible(t tuple.Tuple) (bool, error)
    Dirty(t tuple.Tuple) (visible bool, wait tuple.XID, err error)
    Modify(t tuple.Tuple, tid tuple.TID) (TM, error)
    Wait(xid tuple.XID)
}
var Any Snapshot
type TM int
const ( TMOk TM = iota; TMInvisible; TMSelfModified; TMUpdated; TMDeleted; TMBeingModified )
func (r TM) String() string

func (r *Relation) Fetch(tid tuple.TID, snap Snapshot) (t tuple.Tuple, visible bool, err error)
func (r *Relation) Delete(tid tuple.TID, xid tuple.XID, snap Snapshot) error
func (r *Relation) Update(tid tuple.TID, t tuple.Tuple, xid tuple.XID, snap Snapshot) (tuple.TID, error)
func (r *Relation) Lock(tid tuple.TID, xid tuple.XID, snap Snapshot) error
func (r *Relation) Scan(snap Snapshot) *Scan
```

`internal/catalog`

```go
func (c *Catalog) SetSnapshot(fn func() heap.Snapshot)
func (c *Catalog) EndTransaction(commit bool) error
func (c *Catalog) Invalidate()
```

`internal/index`

```go
func Check(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID, snap heap.Snapshot) error
func CheckUnique(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID, snap heap.Snapshot) error
func Build(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo, snap heap.Snapshot) error
```

`internal/executor`

```go
var ErrSerializationFailure error // 40001
type Env struct { Pool *bufmgr.Pool; XID tuple.XID; Snapshot heap.Snapshot; Isolation ast.Isolation }
```

`internal/sql/ast`

```go
type Isolation int
const ( DefaultIsolation Isolation = iota; ReadCommitted; RepeatableRead )
func (i Isolation) String() string
type Begin struct { Isolation Isolation }
```

`internal/session`

```go
type Cluster struct { /* private */ }
func OpenCluster(dir string) (*Cluster, error)
func (c *Cluster) Connect() (*Session, error)
func (c *Cluster) Close() error
func (s *Session) Blocked() bool
```

Semantics the tests depend on:

- `Manager.Snapshot` reports `next, next, nil` with nothing running and
  otherwise the running IDs sorted, xmin the smallest of them. `Wait`
  returns at once for a finished, reserved, or unassigned ID, blocks
  for a running one until `Commit`, `Abort`, or `Close`, and `Waiting`
  counts the blocked callers.
- `Take` excludes `xid` from `Xip`; the result is fixed: a commit after
  it stays invisible, and `String` is `xmin:xmax:xip`.
- `Visible`, `Dirty`, and `Modify` follow the rules above exactly, hint
  bits included: `TestVisible` checks the verdict, the bits set, and
  the number of log lookups per case, and that a second call looks
  nothing up. An ID inside the window the log does not know is an
  error; one at or above `Xmax` is invisible without a lookup.
- `heap.Scan(snap)` yields what `Visible` allows; `Fetch(tid, snap)`
  returns the tuple and the verdict; both write hint bits to the page,
  which the raw tuples show and the lookup count proves. With a nil
  snapshot `Scan` skips non-zero xmax and `Fetch`'s flag is `xmax ==
  0`.
- `Delete`, `Update`, and `Lock` with a snapshot: `ErrInvisible` for
  an aborted or running xmin, `ErrAlreadyDeleted` for a committed or
  own xmax, a wait for a running one and then the verdict, an
  overwritten xmax (hint bits cleared) for an aborted one. `Lock`
  writes nothing. `Update` checks `ErrTupleTooLarge` before stamping
  anything, and an update whose stamp finds the row taken leaves the
  version it inserted dead rather than orphaned.
- Catalog: with `SetSnapshot`, rows of a transaction the snapshot calls
  running or aborted are invisible to `Lookup`, `Tables`, and the
  duplicate-name check; own rows are visible. `EndTransaction(true)`
  removes the files of dropped relations, `EndTransaction(false)` those
  of created ones, a file created and dropped in one transaction goes
  either way, and the list is empty afterwards. `Invalidate` and
  `SetSnapshot` drop the cache.
- `CheckUnique` waits for a running insert or delete of the key and
  then reports the violation (commit) or nothing (abort); `Build` with
  a snapshot indexes deleted tuples and skips aborted inserts, and a
  unique build waits for a running insert of a duplicate.
- Executor: `Env.Snapshot` decides what `SeqScan` and `IndexScan`
  return; a second `UPDATE` or `DELETE` of a row waits, and after the
  first commits it applies to the new version under READ COMMITTED
  (`TestConcurrentUpdate` checks the version chain), skips a deleted
  row or one that no longer passes the quals (`UPDATE 0`), proceeds on
  the original after an abort, and fails with `ErrSerializationFailure`
  and the message above under REPEATABLE READ.
- Parser: `BEGIN ISOLATION LEVEL` with `READ COMMITTED`, `READ
  UNCOMMITTED` (printed as `READ COMMITTED`), and `REPEATABLE READ`;
  `SERIALIZABLE` is a syntax error expecting `READ or REPEATABLE`, and
  the other partial forms fail as `TestPositions` lists. `Begin{}`
  prints `BEGIN`.
- Session: an uncommitted insert, delete, or update is visible to its
  own session only, through sequential and index scans alike; a
  rollback restores the other session's view and the tuples stay on
  the pages. `BEGIN ISOLATION LEVEL REPEATABLE READ` sets
  `s.isolation`, keeps `s.snap` from the first statement, and both are
  cleared when the block ends, a failed block included; a nested
  `BEGIN` cannot change the level. A waiting statement reports
  `Blocked`; a serialization failure fails the block, and its `COMMIT`
  answers `ROLLBACK`. The second insert of a key waits and then fails
  or succeeds with the first's verdict. Another session's uncommitted
  table "does not exist"; committed DDL is seen at once, an index
  included; a rolled-back `DROP TABLE` keeps the rows and a
  rolled-back `CREATE TABLE` frees the name. `Close` of one session
  leaves the cluster open for the others; `Cluster.Close` flushes.
- `heap.ErrAlreadyDeleted` reaching `Exec` (two sessions updating one
  catalog row) is reported as `tuple concurrently updated`.
- `ch17_mvcc.sql` shows rolled-back inserts, deletes, updates, and DDL
  vanishing; the four specs under `testdata/isolation/specs` cover the
  permutations listed in each.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** How a waiter is woken when the transaction it waits
for ends. The tests pin that `Wait` returns once the other transaction
commits or aborts, that it returns at once for a transaction that has
already finished, and that `Waiting` counts the waiters; the folded note
uses a channel per transaction, and a condition variable is as good.

**Go you will need.** `slices.Sort` and `slices.BinarySearch` for
`Xip`; a `chan struct{}` closed with `close` as a one-shot broadcast,
received with `<-done`; `sync/atomic.Bool` for `Blocked`; a goroutine
per step in the isolation runner, already written.

<details><summary><b>txn.Snapshot, Wait, Waiting.</b></summary>

Change the running set to a `map[tuple.XID]chan struct{}`; `Begin` makes
the channel, `finish` and `Close` close it after deleting the entry.
`Snapshot` collects the keys under the lock, sorts them, and takes the
smallest as xmin, `next` as xmax. `Wait`: under the lock look the channel
up, return if absent, increment `waiting`, unlock, receive, lock,
decrement.
</details>

<details><summary><b>mvcc.Take</b></summary>

`mvcc.Take` calls `Manager.Snapshot`, removes `xid` from the slice, and stores the
manager as `Log`. **InProgress** is three comparisons and a binary search.
**Visible, Dirty, Modify** share two private helpers: `xminStatus(t)`
returns committed for a hinted or own xmin, aborted for `XminInvalid`, else
asks `Log.Status` and sets the bit; `xmaxStatus(t)` the same for a non-
zero, non-own xmax. Write the three rules over them in the order of "The
visibility rule"; the tests' table of verdicts (`newLog`) has one ID per
case.
</details>

<details><summary><b>heap.</b></summary>

`Fetch` and `readBlock` (the part of `loadBlock` under the shared lock)
build a `hint{off, xmin, xmax, before, after}` per tuple whose infomask
`Visible` changed; `setHints(buf, hints)` applies them under the exclusive
lock with the xmin/xmax recheck. `markDeleted(tid, xid, snap)` is the loop
of "Writers"; with `xid == InvalidXID` it checks without stamping, which is
`Lock`. Pass `Modify` a copy of the tuple: the page is locked, but a hint
set there would need `MarkDirty`, and the stamp that follows makes the xmax
hint moot anyway. `Update`: size check, `markDeleted` with `InvalidXID`,
`Insert`, then `markDeleted` again with `xid` and the new TID; if that
second call fails, `markDeleted` the new version with `xid` and a nil
snapshot before returning its error. `heap.Any` is an empty struct whose
methods answer visible, live, and `TMOk`.
</details>

<details><summary><b>catalog.</b></summary>

A `snapshot func() heap.Snapshot` field, nil until `SetSnapshot`, and a
private `snap()` that calls it or returns nil; every `Scan`, `Delete`, and
`Update` on the three catalog heaps goes through it. A `pending
[]pendingFile{oid, atCommit}` list appended by `CreateTable` and
`CreateIndex` (false) and by `DropTable` and `dropIndex` (true) in place of
their `Discard` and `Unlink`; `EndTransaction` applies the matching
entries, clears the list, and invalidates.
</details>

<details><summary><b>index.</b></summary>

`liveEntry` returns `(conflict, wait, err)`: `Fetch(tid, nil)`, then
`Dirty` when there is a snapshot; `CheckUnique` loops per index while
`wait` is set, closing the scan before waiting. `Build` scans through
`heap.Any` when it has a snapshot and decides per tuple with a private
`buildTuple`: `Dirty` first (wait means index but not live, or wait and
refetch for a unique index; live means index and live), else `Modify`,
whose `TMInvisible` means skip.
</details>

<details><summary><b>executor.</b></summary>

`Env` gains the two fields; `seqScan.Open` and `indexScan.Next` pass
`env.Snapshot`. `modifyTable` gains `quals` (collected by a private
`scanQuals` over `Filter` and `IndexScan` nodes) compiled into one ANDed
`qual` at `Open`, and `write` becomes the loop of "What the executor does
after the wait" around a private `apply`, with a private `newest(h, tid)`
for the chain. `apply` for an `UPDATE` calls `h.Lock` before the unique
check.
</details>

<details><summary><b>ast, lexer, parser.</b></summary>

`Isolation` with `DefaultIsolation` zero so that `Begin{}` still prints
`BEGIN`; the keywords `committed`, `isolation`, `level`, `read`,
`repeatable`, `uncommitted`; a private `parseBegin` that accepts the
optional clause and reports the expected words the error tests list.
</details>

<details><summary><b>session.</b></summary>

`Cluster{store, pool, txn}` with `OpenCluster` doing what `Open` did, minus
the catalog; `Connect` opens a `Catalog` on the pool and calls
`SetSnapshot` with a method that takes a fresh snapshot for the current
transaction (or `InvalidXID`). `Session` gains `cluster`, `owner`,
`isolation`, `snap`, and an `atomic.Bool` for `Blocked`, set by a private
`sessionLog` wrapper around the manager whose `Wait` brackets the
manager's; `snapshot(xid)` takes a snapshot and swaps that wrapper in as
its `Log`. `statement` begins the transaction, takes the snapshot unless
one is kept, calls `cat.Invalidate`, and on every end of a transaction a
private `endTransaction(commit)` clears the state and calls
`cat.EndTransaction`. `control` reads `Begin.Isolation`. `wrap` adds
`heap.ErrAlreadyDeleted`.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch17` at the end.
Every test not named in that step or an earlier one still panics.

1. Transaction manager. `Snapshot`, `Wait`, `Waiting`. Green:
   `TestSnapshot`, `TestWait`, and the chapter 16 tests unchanged.

   ```sh
   go test -race ./internal/txn/... -run 'TestSnapshot|TestWait'
   ```
2. Snapshots. `Take`, `String`, `InProgress`, `Visible`, `Dirty`,
   `Modify`, `Wait` in `mvcc`. Green: `TestInProgress`, `TestVisible`,
   `TestVisibleFutureXID`, `TestDirty`, `TestModify`, `TestTake`.

   ```sh
   go test -race ./internal/mvcc/... -run 'TestInProgress|TestVisible|TestVisibleFutureXID|TestDirty|TestModify|TestTake'
   ```
3. Heap. `Scan` and `Next` with the snapshot, `Fetch`, `setHints`,
   `Delete`, `Update`, `Lock`. Green: `TestScanSnapshot`,
   `TestFetchSnapshot`, `TestDeleteSnapshot`, `TestUpdateSnapshot`,
   `TestUpdateLostRaceHidesNewVersion`, `TestLockSnapshot`, and the
   chapter 5 tests, which now pass nil.

   ```sh
   go test -race ./internal/heap/... -run 'TestScanSnapshot|TestFetchSnapshot|TestDeleteSnapshot|TestUpdateSnapshot|TestUpdateLostRaceHidesNewVersion|TestLockSnapshot'
   ```
4. Catalog. `SetSnapshot`, `Invalidate`, the pending list and
   `EndTransaction`. Green: `TestCatalogSnapshot`, `TestInvalidate`,
   `TestEndTransactionAbort`, and the chapter 8 and 13 tests, which now
   call `EndTransaction` after a drop.

   ```sh
   go test -race ./internal/catalog/... -run 'TestCatalogSnapshot|TestInvalidate|TestEndTransactionAbort'
   ```
5. Indexes. `CheckUnique`, `Check`, `Build` with the snapshot. Green:
   `TestCheckUniqueWaits`, `TestBuildSnapshot`, and the chapter 13
   tests with nil.

   ```sh
   go test -race ./internal/index/... -run 'TestCheckUniqueWaits|TestBuildSnapshot'
   ```
6. Executor. `Env`, the scans, `modifyTable`'s loop. Green:
   `TestScanSnapshot` (executor), `TestConcurrentUpdate`,
   `TestConcurrentUpdateAborted`, `TestConcurrentUpdateRecheck`,
   `TestConcurrentUpdateRepeatableRead`.

   ```sh
   go test -race ./internal/executor/... -run 'TestScanSnapshot|TestConcurrentUpdate$|TestConcurrentUpdateAborted|TestConcurrentUpdateRecheck|TestConcurrentUpdateRepeatableRead'
   ```
7. Syntax. `Isolation`, the keywords, `parseBegin`. Green: `TestString`
   (ast), `TestAllKeywords`, `TestGolden`, `TestRoundTrip`,
   `TestPositions` (parser). The line also re-runs the lexer's own
   `TestPositions`, which is unrelated and still passes.

   ```sh
   go test -race ./internal/sql/ast/... ./internal/sql/lexer/... ./internal/sql/parser/... -run 'TestString|TestAllKeywords|TestGolden|TestRoundTrip|TestPositions'
   ```
8. Session. `Cluster`, `Connect`, `Blocked`, the snapshot per
   statement, the isolation level, `endTransaction`. Green:
   `TestClusterVisibility`, `TestIsolationLevels`,
   `TestConcurrentUpdates`, `TestUniqueAcrossSessions`,
   `TestUniqueUnderConcurrentInsert`, `TestCatalogAcrossSessions`,
   `TestClusterClose`, the chapter 11 to 16 session tests, `TestRegress`
   with `ch17_mvcc.sql`, and `TestIsolation`. The last unique test is
   the one the isolation harness cannot express: it runs one step at a
   time, so it never has two sessions inside the window between the
   unique check and the index write, which is the window chapter 13's
   lock closes.

   ```sh
   REGRESS_CHAPTER=17 go test -race ./internal/session/... ./internal/regress/... ./internal/isolation/... -run 'TestClusterVisibility|TestIsolationLevels|TestConcurrentUpdates|TestUnique|TestCatalogAcrossSessions|TestClusterClose|TestRegress|TestIsolation'
   ```

```sh
go test ./internal/regress/... -update    # after changing a .sql file
go test ./internal/isolation/... -update  # after changing a .spec file
```

### When a test fails

- `TestTake` — the snapshot's own ID appears in `Xip`. It is removed
  after the manager hands the list over, which is why `Take` takes the
  ID as an argument.
- `TestInProgress` — an ID at or above `Xmax` reports false. It reports
  true: a transaction that had not started when the snapshot was taken
  is as invisible as one that was running, and treating it as finished
  lets a reader see a commit from its own future.
- `TestVisible` — a tuple whose xmax is a running transaction is
  invisible. A delete that has not committed has not happened for us, so
  the tuple is still visible; the mirror of the xmin rule.
- `TestVisible` — a tuple whose xmax committed after the snapshot is
  invisible. Check `InProgress(xmax)` *after* the log says committed: a
  delete that committed later than our snapshot is one we must not see.
- `TestVisible` — a tuple our own transaction deleted is visible.
  The snapshot's own xmax is always in the past, which is sound only
  because `modifyTable` materialises its input; see "The Halloween
  problem".
- `TestVisibleFutureXID` — an error instead of an answer, or a panic.
  `Manager.Status` returns `ErrFutureXID` for an ID at or above
  `NextXID`, but `InProgress` has already answered true for those, so
  the log is never asked about one.
- `TestVisible` — passes for the ordinary cases and fails once a hint
  bit is set. `XminCommitted` short-circuits the log lookup but not the
  `InProgress` test: a later snapshot can have set the bit on a
  transaction ours saw running.
- `TestDirty` — the unique check misses a conflicting row that committed
  after the snapshot. `Dirty` ignores the snapshot's bounds entirely; it
  reports what is true now, and reports a running transaction through
  `wait` so the caller can block on it.
- `TestModify` — a tuple whose xmax is running is reported as deleted.
  It is `TMBeingModified`, and the writer must release the page lock,
  `Wait`, and look again; holding the lock while waiting deadlocks
  against a transaction that needs the same page to finish.
- heap `TestScanSnapshot` — the race detector fires on the infomask.
  Hints are computed on a copy under the shared lock and written back
  under the exclusive one, and only onto tuples whose xmin and xmax are
  still the ones the verdict was reached on.
- heap `TestScanSnapshot` — the second scan consults the log as often as
  the first. The bits have to be written back to the page and the buffer
  marked dirty, not just set on the copy the scan hands out.
- heap `TestUpdateLostRaceHidesNewVersion` — the relation ends up with a
  row nothing points at. The second `markDeleted` is where the update
  can still lose the row; on its error the new version has to be stamped
  with the updater's own xid, so that its xmin and xmax are the same
  transaction and no snapshot sees it.
- executor `TestConcurrentUpdateRecheck` — a row is updated although the
  new version no longer matches the `WHERE`. Under READ COMMITTED the
  chain is followed and the statement's quals are re-evaluated against
  the new version; a row that fails them is skipped and not counted.
- executor `TestConcurrentUpdateRepeatableRead` — the update follows the
  chain instead of failing. At REPEATABLE READ and above it is
  `could not serialize access due to concurrent update`, SQLSTATE
  40001, because proceeding would mean reading past the snapshot.
- index `TestCheckUniqueWaits` — two sessions insert the same key and
  both succeed. `Dirty` reports the other session's in-progress tuple
  with a `wait` ID; the check must close its scan, wait, and start the
  index over, so the second sees the first's verdict.
- session `TestCatalogAcrossSessions` — one session does not see
  another's `CREATE TABLE`. Each session has its own relation cache, and
  `Invalidate` drops it at the start of every statement, standing in for
  the invalidation messages a real backend would receive.
- session `TestClusterVisibility` — a session sees a row another session
  has written but not committed. Every statement outside a block takes
  its own snapshot, and the tuple's xmin is in that snapshot's `Xip`.
- `TestIsolation` — a step reported as `<waiting ...>` that should have
  completed, or the reverse. `Session.Blocked` is what the runner reads
  to tell a blocked step from a slow one, and it must reflect the
  goroutine parked in `Manager.Wait`.

## Why versions in the heap and not an undo log

The other way to give every reader a consistent view is Oracle's: keep one
current row in place, write the old values to an undo log, and let a reader
reconstruct the version it is entitled to. Rollback is then real work and a
long-running reader can fail when the undo it needs has been recycled, but
the table holds no dead rows. PostgreSQL keeps every version in the heap
with `xmin` and `xmax`, so a rollback is one bit in the commit log and a
read is two integer comparisons against a snapshot; the price is that dead
versions accumulate and something has to remove them. That something is
VACUUM, which v1 does not have (D11), so a table that is updated in a loop
grows forever here and its indexes keep entries pointing at tuples nobody
can see (D24).

## Out of scope

Command IDs (`cmin`, `cmax`), so the Halloween problem is solved by
materialisation and a statement cannot see its own transaction's
changes made by itself; SERIALIZABLE and its predicate locks, so the
parser refuses the level; `SET TRANSACTION ISOLATION LEVEL` and
`default_transaction_isolation`; lazy transaction IDs; row locks
(`SELECT FOR UPDATE`) and the lock manager, so `Lock` waits and checks
but holds nothing and an update that loses the row after its check has
to bury the version it inserted (D25); table locks, so DDL is not
serialised against DML or other DDL: two sessions creating the same
table name at once both succeed, and an index created while another
session's statement runs is missed by that statement; deadlock
detection, so two sessions each waiting for the other's key wait
forever; shared invalidation messages, replaced by dropping the cache
per statement; `VACUUM`, so
dead versions and the index entries pointing at them stay (v2), and
`Build` cannot tell a dead tuple from a recently dead one; the
`indcheckxmin` rule for indexes built while older snapshots run;
`EvalPlanQual` for joins and `UPDATE ... FROM`, which the parser does
not have; multiple databases and the `postmaster`, so a cluster is one
process's object and sessions are goroutines.

## Check your understanding

1. Transactions 3 and 4 have finished, 5 is running, and your
   transaction is 6. What are `Xmin`, `Xmax` and `Xip`, and how does the
   snapshot print?
   <details><summary>Answer</summary>

   `Xmin` 5, `Xmax` 7, `Xip` `[5]`, printing as `5:7:5`. `Xmin` is the
   oldest running transaction, so everything below it has a final
   verdict; `Xmax` is the next ID to be handed out; and the snapshot's
   own 6 is removed from the running list, because a transaction always
   sees its own work.
   </details>
2. With that snapshot, is a tuple with xmin 3 (committed) and xmax 5
   visible? What about xmin 5, xmax 0?
   <details><summary>Answer</summary>

   The first is visible and the second is not. Transaction 5 is in
   `Xip`, so its delete had not happened when the snapshot was taken and
   its insert had not either. The two cases are the same test applied to
   the two ends of the tuple's life, which is why the rule is two steps
   and not two rules.
   </details>
3. Why is `XminCommitted` safe to cache in the tuple, while "this
   transaction is running" is not?
   <details><summary>Answer</summary>

   Because a verdict is final and a state is not. Once a transaction has
   committed it has committed for every future reader, so the bit can
   never become wrong. "Running" changes by itself, and worse, it is
   relative to a snapshot: the same transaction is running for one
   reader and finished for another. That is why the visibility rule
   still tests `InProgress` even after the hint says committed.
   </details>
4. `Update` checks the old version, inserts the new one, and only then
   stamps xmax. What stops two concurrent updates from both succeeding,
   and what does the loser have to clean up?
   <details><summary>Answer</summary>

   Nothing stops them from both passing the check — the window is the
   whole insert. The stamp is where they are serialised: it asks
   `Modify` again under the old version's page lock, so the second one
   finds a running or committed xmax, waits, and comes back with
   `ErrAlreadyDeleted`. By then it has already inserted a tuple, and
   PostgreSQL would not be in this position, because it holds a
   lock-only xmax on the old version across the insert (D25). Without
   row locks the loser cleans up instead: it stamps its own new version
   with its own xid, so xmin and xmax are the same transaction and the
   tuple is dead to every snapshot, its own included.
   </details>
5. PostgreSQL adds a command ID to every tuple, which we leave out. What
   does it buy, and how do we get by without it?
   <details><summary>Answer</summary>

   It makes visibility precise *within* a transaction: a row inserted by
   an earlier statement of the same transaction is visible, and one
   inserted by the current statement is not. That is what stops an
   `UPDATE` from meeting the rows it is writing, and it is what lets
   PostgreSQL stream rows through `ModifyTable` instead of buffering
   them. We get the same guarantee from chapter 11's rule that
   `modifyTable` reads its whole input before writing anything, which
   costs memory proportional to the rows updated and is why `Visible`
   can treat every own xmax as being in the past.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Row locks.** `SELECT ... FOR UPDATE`: mark the tuple, make a second
   session wait for it, and release at commit. `Modify` already returns
   `TMBeingModified`; what is missing is somewhere to hold the lock and a
   rule for what happens at the end of the transaction.
2. **Deadlock detection.** Two sessions each waiting for the other's key
   wait forever today. Build the wait-for graph the manager could keep,
   detect a cycle after a timeout, and pick a victim to abort with
   PostgreSQL's message.
3. **A single-pass VACUUM.** Remove the tuples dead to every running
   snapshot and the index entries pointing at them. Deciding what "dead to
   everyone" means is the chapter's content one more time, from the other
   side; actually freeing B-tree pages is where v2's difficulty lives.
