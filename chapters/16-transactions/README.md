# Chapter 16: Transactions

**Goal.** Transaction IDs handed out from the control file, a commit
log on disk that says which of them committed, and a session that runs
every statement in a transaction: its own one by default, or the block
between `BEGIN` and `COMMIT` or `ROLLBACK`, with an error inside the
block refusing everything until the block ends.

**You edit.** `internal/txn/txn.go`: 7 functions with
`panic("not implemented")` bodies, `Open` through `Status`.
`internal/catalog/control.go`: 1, `UpdateControl`. And your own bodies
from earlier chapters: `catalog.NewOID` allocates through
`UpdateControl`; `session.Open`, `Close`, and `Exec` gain the
transaction state, `run` loses its `BEGIN`, `COMMIT`, and `ROLLBACK`
cases to it, and `wrap` learns `ErrInFailedTransaction`. The tests of
the three packages are the contract; the regression runner and the REPL
now print `Result.Warnings`.

**Needs from earlier chapters.** `internal/catalog` (`ReadControl`,
`WriteControl`, `Control`), `internal/tuple` (`XID` and the reserved
values), the session of chapters 11 to 15, and `os.File.ReadAt`,
`WriteAt`, and `Sync`. No earlier package was stubbed for this chapter.

**Done when.** `make test-ch16` passes.

**Effort.** Medium, about 4 hours. The commit log (step 3) is the hard
part: two bits per transaction, segment and offset arithmetic, and a file
that has to survive a restart.

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

Every tuple has carried `xmin` and `xmax` since chapter 2, and every
heap write has taken a transaction ID since chapter 5, but the session
has stamped `tuple.FrozenXID` on all of them. Nothing could tell a row
written by a statement that failed halfway from one that succeeded, and
`BEGIN` and `COMMIT` were parsed and answered with a tag and nothing
else. This chapter gives the stamps meaning. A transaction ID names a
unit of work; the commit log records, in two bits per ID, whether that
work committed or aborted; and from now on a tuple's `xmin` and `xmax`
say which transactions created and deleted it, so that chapter 17 can
look them up and decide whether a reader should see the row.

PostgreSQL does not write a tuple twice on commit. The heap page holds
the transaction ID, and the commit log holds the verdict; a reader
combines the two. That is why commit is cheap, a single bit pair
flipped and synced, no matter how many rows the transaction touched,
and why abort costs nothing at all: a transaction that never recorded a
commit is treated as aborted, which is also what happens to every
transaction that was in progress when the server died. The same
structure decides the order of work inside the session. A statement
outside a block gets a transaction of its own, committed when it
succeeds and aborted when it fails. An error inside a block aborts the
transaction at once but leaves the block open in a failed state, where
every statement but `COMMIT` and `ROLLBACK` is refused with the error
PostgreSQL's users know well, and `COMMIT` ends the block with a
`ROLLBACK` tag.

PostgreSQL source: `access/transam/varsup.c` (`GetNewTransactionId`,
`GetNewObjectId`), `access/transam/clog.c` (`TransactionIdSetTreeStatus`,
`TransactionIdGetStatus`, the `CLOG_*` constants), `access/transam/slru.c`
(segment files), `access/transam/transam.c` (`TransactionLogFetch`,
`TransactionIdDidCommit`), `access/transam/xact.c` (`StartTransaction`,
`CommitTransaction`, `AbortTransaction`, `BeginTransactionBlock`,
`EndTransactionBlock`, `UserAbortTransactionBlock`, the `TBLOCK_*`
states), `access/transam/README`, `tcop/postgres.c` (`exec_simple_query`,
`start_xact_command`, `IsTransactionExitStmt`), `include/access/clog.h`,
`include/access/transam.h`.

## Transaction IDs

`tuple.XID` is a 32-bit counter. Three values are reserved, as in
`transam.h`: `InvalidXID` (0) means "none", `BootstrapXID` (1) stamps
the catalog rows `Bootstrap` writes, and `FrozenXID` (2) marks a tuple
as older than every transaction; user transactions start at
`FirstNormalXID` (3). The next ID to hand out lives in the control file
of chapter 8, next to the next OID.

Worked example. A freshly bootstrapped cluster has `NextXID` 3 in its
control file, because 0, 1 and 2 are reserved. Three `Begin` calls
return 3, 4 and 5, and the control file's counter is 6 afterwards, on
disk, before the third `Begin` returned. A `SELECT` consumes an ID like
anything else.

`Manager.Begin` takes the next ID, rewrites the control file with the
one after it, and only then returns the transaction. The rewrite is the
durability of the counter: after a crash the ID cannot be handed out a
second time. PostgreSQL writes the counter to WAL instead, one record
per 8192 IDs (`VAR_XID_PREFETCH`); without WAL one rename per
transaction is the honest equivalent. IDs are assigned when the
transaction starts, so a `SELECT` consumes one too. PostgreSQL assigns
lazily, at the first write, and a read-only transaction gets a virtual
ID only; that optimisation is out of scope.

The control file now has two writers, `catalog.NewOID` and
`Manager.Begin`, each interested in one field. `UpdateControl` is the
read-modify-write cycle both use: under a package-wide mutex, read the
file, let the caller change the struct, write it back. PostgreSQL's
`ControlFileLock` guards the same file for the same reason. `NewOID`
changes to call it; the catalog no longer keeps a copy of the file in
memory, so a chapter 8 implementation that did must drop the copy or
it will overwrite the manager's counter.

## The commit log

**Predict.** Transaction 3 commits and transaction 4 aborts, on a fresh
cluster. What are the bytes of `pg_xact/0000`, and how long is the file?

`pg_xact/` under the data directory holds two bits per transaction:

| bits | `Status` | meaning |
|---|---|---|
| `00` | `InProgress` | no verdict recorded |
| `01` | `Committed` | |
| `10` | `Aborted` | |

Bits are packed four transactions to a byte, lowest ID in the lowest
bits: transaction `x` is at byte `x / 4`, shifted left by `(x % 4) * 2`.
Pages are 8 KiB (32768 transactions), a segment file holds 32 pages
(1048576 transactions, 256 KiB), and segment `n` is the file named with
four uppercase hex digits, `0000`, `0001`, and so on, exactly as
PostgreSQL's SLRU names them. So transaction 3 sets bits 6 and 7 of the
first byte of `0000`, and transaction 1048576 sets bits 0 and 1 of the
first byte of `0001`. The constants `BitsPerXact`, `XactsPerByte`,
`PageSize`, `XactsPerPage`, `PagesPerSegment`, `XactsPerSegment`, and
`SegmentSize` are exported and the golden test reads the files.

A segment file is created when the first transaction in its range
finishes, and grows as needed: writing byte `n` of a shorter file
extends it with zero bytes, and reading past the end returns zero,
which is `InProgress`. PostgreSQL zeroes each new page explicitly
(`ExtendCLOG`) because its SLRU keeps pages in shared memory; the
sparse-file rule gives the same result here without a page cache.

Worked example. On a freshly bootstrapped cluster the first three
transactions are 3, 4 and 5. Commit 3, abort 4, leave 5 running:

| Transaction | Byte | Shift | Bits | Contribution |
|---|---|---|---|---|
| 3 | `3 / 4 = 0` | `(3 % 4) * 2 = 6` | `01` | `0x40` |
| 4 | `4 / 4 = 1` | `(4 % 4) * 2 = 0` | `10` | `0x02` |
| 5 | 1 | 2 | `00` | nothing written |

so `pg_xact/0000` is two bytes:

```
40 02
```

Byte 0 holds transactions 0 to 3 and byte 1 holds 4 to 7. Transaction 5
contributes nothing, because `InProgress` is what zero bits already mean
and nothing is written until there is a verdict; the file is two bytes
long only because transaction 4's abort touched byte 1. Reading
transaction 6 or 7 reads inside byte 1 and gets zero; reading
transaction 8 reads past the end of the file and also gets zero. Both
are `InProgress`.

Segment `0001` would begin at transaction 1048576, whose bits are the
lowest two of that file's first byte.

`Commit` writes the two bits and calls `Sync` on the segment file
before returning, so a transaction reported as committed is committed
after a crash. `Abort` writes without syncing: if the write is lost,
the transaction reads as in progress, which the next rule turns into
aborted anyway. PostgreSQL syncs the WAL rather than the log, and the
log is written back at checkpoints; without WAL the log itself is the
durable record.

Which is why the session flushes before it commits. A committed bit
that vouches for rows still sitting in the buffer pool is worse than no
bit at all: after a crash the log says the transaction happened and the
table it wrote is empty, or missing, because the catalog page that
named it never reached the disk either. A private flush —
`pool.FlushAll` then `store.SyncAll` — runs on both commit paths just
before `tx.Commit`. It flushes the whole pool, not this session's pages,
because the pool does not know whose they are; that is why `FlushAll`
takes each frame's content lock (chapter 4) while another session is
still writing to it. This is the expensive way round —
PostgreSQL flushes a few WAL records instead and leaves the pages for a
checkpoint — and it is the whole of v1's durability story (D11).

The name of a file needs the same care as its contents. `fsync` on
`base/16384` says nothing about whether the directory entry `16384`
survives; `smgr.Create` and `Unlink` therefore call `smgr.SyncDir` on
`base/`, `WriteControl` calls it on `global/` after the rename that
installs the new control file, and `segment` calls it on `pg_xact/`
after creating a segment. Skip the last one and the first commit on a
fresh cluster can lose the whole log file, turning every committed
transaction into an aborted one. Skip the `global/` one and `NextXID`
can go backwards across a crash, handing a new transaction an ID whose
commit bit is already set — its uncommitted rows visible to everyone
from the moment it writes them.

## Transaction status

`Manager.Status` answers for any ID, in this order:

1. `InvalidXID` is `Aborted`; `BootstrapXID` and `FrozenXID` are
   `Committed`. The log is not consulted (`TransactionLogFetch`).
2. An ID the manager has not handed out yet, that is, one at or above
   `NextXID`, is `ErrFutureXID`.
3. An ID between `Begin` and `Commit` or `Abort` in this manager is
   `InProgress`. This is PostgreSQL's `TransactionIdIsInProgress`, which
   asks the shared process array rather than the log, because the log
   cannot tell "running" from "died without a verdict".
4. Otherwise the log's bits, except that `00` is `Aborted`: the
   transaction was in progress in a process that no longer exists.

Worked example, on the manager above where 3 committed, 4 aborted and 5
is still running:

| ID | Status | Why |
|---|---|---|
| 0 | `Aborted` | `InvalidXID`, rule 1 |
| 1 | `Committed` | `BootstrapXID`, rule 1 |
| 2 | `Committed` | `FrozenXID`, rule 1 |
| 3 | `Committed` | the log's `01` |
| 4 | `Aborted` | the log's `10` |
| 5 | `InProgress` | rule 3, this manager is running it |
| 99 | `ErrFutureXID` | rule 2, at or above `NextXID` (6) |

Close the manager and open a new one on the same directory, which is
what a crash and restart look like:

| ID | Status |
|---|---|
| 3 | `Committed` |
| 4 | `Aborted` |
| 5 | `Aborted` |

Transaction 5 changed answer. Its bits are still `00`, but no manager is
running it any more, so rule 3 no longer applies and rule 4 turns `00`
into `Aborted`. That is the whole reason `Abort` need not sync: a lost
abort record is indistinguishable from a process that died, and both
mean aborted.

`Close` closes the segment files and forgets the running transactions,
so a handle from before `Close` returns `ErrFinished` and its ID reads
as `Aborted` from a new `Manager`. That is the crash case the plan asks
for, and the tests simulate it by opening a second `Manager` on the
directory.

## Transactions in the session

The session keeps three fields: the current transaction (`nil` between
statements outside a block), whether an explicit block is open, and
whether that block has failed. They follow PostgreSQL's `TBLOCK_*`
states, reduced to the ones a single session without subtransactions
reaches:

| state | `tx` | `explicit` | `failed` |
|---|---|---|---|
| idle (`TBLOCK_DEFAULT`) | nil | false | false |
| in a block (`TBLOCK_INPROGRESS`) | set | true | false |
| failed block (`TBLOCK_ABORT`) | set, aborted | true | true |

`Exec` parses the whole string first, then handles each statement:

- `BEGIN`, `COMMIT`, and `ROLLBACK` never start a transaction of their
  own. `BEGIN` in the idle state begins one and opens the block; inside
  a block it succeeds with the warning `there is already a transaction
  in progress`. `COMMIT` and `ROLLBACK` outside a block succeed with the
  warning `there is no transaction in progress`, tags `COMMIT` and
  `ROLLBACK`. `COMMIT` of a block commits it; `ROLLBACK` aborts it; both
  return to idle.
- Any other statement in the idle state begins a transaction, runs, and
  commits it on success or aborts it on error; inside a block it runs
  in the block's transaction. Both analysis and execution count as
  running: an unknown table inside a block fails the block.
- An error inside a block aborts the transaction immediately
  (`AbortTransaction` records the abort at once) and marks the block
  failed. From then on every statement but `COMMIT` and `ROLLBACK`,
  `BEGIN` included, fails with `ErrInFailedTransaction`, message
  `current transaction is aborted, commands ignored until end of
  transaction block`, no position, before it is analysed. A syntax error
  is still reported as one, because parsing comes first. `COMMIT` and
  `ROLLBACK` of a failed block both answer `ROLLBACK` and return to
  idle; nothing is written to the log, the abort already is.
- Warnings ride on `Result.Warnings`; the statement still succeeded and
  has its tag. The regression runner prints them as `WARNING:  ...`
  before the result, as psql does, and the REPL prints them to stderr.

Worked example, from `ch16_txn.sql`:

```
BEGIN;
INSERT INTO t VALUES (1, 'one');
INSERT INTO t VALUES (2, 'two');
COMMIT;
SELECT * FROM t ORDER BY a;
 a |  b  
---+-----
 1 | one
 2 | two
(2 rows)

COMMIT;
WARNING:  there is no transaction in progress
ROLLBACK;
WARNING:  there is no transaction in progress
BEGIN;
BEGIN;
WARNING:  there is already a transaction in progress
```

Both spurious statements succeed: the tags are still `COMMIT`,
`ROLLBACK` and `BEGIN`, and only a `WARNING:` line marks them. That is
PostgreSQL's behaviour, and it matters because a client that wraps every
statement in `BEGIN`/`COMMIT` must not break against a server that is
already in a block. The regression runner prints the warning before the
result, as psql does.

Each statement of a multi-statement `Exec` string is its own
transaction, as if psql had sent them one by one. PostgreSQL runs such
a string as one implicit block since version 10; that is out of scope.

`Close` aborts a transaction left open, then flushes and closes as
before. Every heap and catalog write of the session is stamped with the
current transaction's ID: `executor.Env.XID`, `CreateTable`,
`DropTable`, `CreateIndex`, `DropIndex`, and `UpdateStats` all get it
from the session's `xid()`.

## What waits for chapter 17

Nothing in this chapter changes what a scan returns. The executor's
scans and `index.CheckUnique` still take a tuple with `xmax == 0` as
live, so a rolled-back `INSERT` stays visible and a rolled-back
`DELETE` hides its row. The tests of this chapter check the stamps on
the tuples and the verdicts in the log, not the rows a `SELECT` shows,
and `ch16_txn.sql` never selects from a table after a rolled-back
write. Chapter 17 adds the snapshot and the visibility function that
combine the two, and its regression file shows the rows vanishing.

## API

`internal/txn`

```go
type Status byte
const ( InProgress Status = 0; Committed Status = 1; Aborted Status = 2 )
func (s Status) String() string

const (
    BitsPerXact = 2; XactsPerByte = 4; PageSize = 8192
    XactsPerPage = 32768; PagesPerSegment = 32
    XactsPerSegment = 1048576; SegmentSize = 262144
)
const Dir = "pg_xact"

var ErrFinished, ErrFutureXID error

type Manager struct { /* private */ }
func Open(dir string) (*Manager, error)
func (m *Manager) Close() error
func (m *Manager) NextXID() tuple.XID
func (m *Manager) Begin() (*Transaction, error)
func (m *Manager) Status(xid tuple.XID) (Status, error)

type Transaction struct { XID tuple.XID /* private */ }
func (t *Transaction) Commit() error
func (t *Transaction) Abort() error
```

`internal/catalog`

```go
func UpdateControl(dir string, fn func(*Control)) error
```

`internal/session`

```go
var ErrInFailedTransaction error
const WarnNoTransaction = "there is no transaction in progress"
const WarnAlreadyTransaction = "there is already a transaction in progress"

type Session struct {
    store; pool; cat            // as before
    txn      *txn.Manager
    tx       *txn.Transaction   // nil when idle
    explicit bool               // inside BEGIN ... COMMIT/ROLLBACK
    failed   bool               // the block hit an error
}
type Result struct { ...; Warnings []string }
```

Semantics the tests depend on:

- `Open` returns `catalog.ErrNotBootstrapped` without a control file and
  `catalog.ErrControlCorrupt` for a bad one, and creates `pg_xact/`.
  `NextXID` is the control file's value.
- `Begin` hands out consecutive IDs from `NextXID` and has rewritten the
  control file with `XID + 1` before it returns. It never fails on its
  own; a control file that cannot be written is the error.
- `Commit` and `Abort` return `ErrFinished` on a transaction that has
  finished either way, or whose manager was closed, and change nothing
  then. `Commit` syncs the segment file, and creating a segment syncs
  `pg_xact/`.
- `Status` follows the four rules above. In particular an ID handed
  out before a `Close` or a crash is `Aborted` in a new manager, and
  `NextXID` of the new manager continues where the old one stopped.
- The log bytes are exactly as in "The commit log": `TestLogGolden`
  reads `pg_xact/0000` after seven transactions, and `TestSegments`
  checks the byte and file of IDs at page and segment boundaries. A
  segment file is exactly as long as its highest written byte needs.
- `Begin`, `Commit`, `Abort`, and `Status` are safe for concurrent use:
  eight goroutines get distinct IDs and their own verdicts.
- `UpdateControl` returns `ReadControl`'s errors, notably
  `ErrNotBootstrapped`; concurrent callers changing different fields
  lose nothing; no `control.tmp` is left behind. `WriteControl` syncs
  `global/` after the rename.
- A commit flushes the cluster before it records its verdict:
  `TestCommitIsDurableWithoutClose` abandons a cluster without `Close`,
  and the committed rows are still there through a fresh `Open`.
- Session: every statement outside a block, DDL, `SELECT`, and a failed
  one included, takes exactly one ID; `BEGIN` takes one for the whole
  block; `COMMIT` and `ROLLBACK` outside a block and every refused
  statement take none. Tuples carry the ID of the statement or block
  that wrote them in `xmin` (`INSERT`, the new version of `UPDATE`) and
  `xmax` (`DELETE`, the old version of `UPDATE`); catalog rows carry the
  DDL's ID.
- After `COMMIT` the block's ID is `Committed`; before, `InProgress`;
  after `ROLLBACK`, `Aborted`; after an error inside the block,
  `Aborted` already, while `tx` is still set and `explicit` and `failed`
  are true. `COMMIT` and `ROLLBACK` of a failed block answer
  `ROLLBACK` with no warning and leave the idle state behind.
- Refused statements fail with `ErrInFailedTransaction` wrapped in
  `*Error` with the message above and `Pos.Line == 0`; a syntax error in
  a failed block is `parser.ErrSyntax`.
- A statement that fails outside a block leaves the idle state; the
  next statement runs. A multi-statement string whose second statement
  fails has committed the first.
- `Close` aborts an open transaction, so after a reopen it is `Aborted`
  and the next `BEGIN` gets the ID after it.
- `ch16_txn.sql` shows the warnings and the failed-block errors.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** What the commit log keeps in memory: one file handle
or many, a cached segment or a read per lookup, and where the lock sits.
The tests pin the bits on disk, that a commit survives a reopen, and that
concurrent `Begin` and `Commit` are race-free.

**Go you will need.** `os.OpenFile` with `os.O_RDWR|os.O_CREATE`;
`(*os.File).ReadAt` returns `io.EOF` for an offset past the end, which
here means "zero"; `WriteAt` past the end extends the file; `Sync` is
`fsync`. `fmt.Sprintf("%04X", n)` for segment names. `clear` on a map.
A `sync.Mutex` at package level for the control file.

<details><summary><b>Layout.</b></summary>

Two private helpers do the arithmetic once: `offset(xid)` returns the byte
within the segment and the shift within the byte, `segment(xid)` returns
the segment file, opening and caching it in a map keyed by segment number.
`get(xid)` and `set(xid, status, sync)` read and write through them with a
one-byte buffer, masking the other three transactions' bits on write. All
of them run with the manager's mutex held.
</details>

<details><summary><b>Begin.</b></summary>

Under the mutex: take `next`, `UpdateControl` setting `NextXID` to the one
after, and only on success advance `next`, add the ID to the running set,
and return the handle. The running set is a `map[tuple.XID]bool`; the
handle holds the manager and the ID, nothing else, so "finished" means "not
in the set".
</details>

<details><summary><b>Commit and Abort.</b></summary>

One private `finish(status, sync)`: lock, check the running set
(`ErrFinished`), `set`, delete from the set. Delete only after the write
succeeded, so a failed write can be retried.
</details>

<details><summary><b>Status.</b></summary>

The special IDs first, without the lock. Then under the lock: `next` check,
running set, `get`, and the `00` to `Aborted` rule.
</details>

<details><summary><b>UpdateControl.</b></summary>

Lock, `ReadControl`, call `fn` on the value, and `WriteControl`. `NewOID`
becomes a call to it whose closure reads the current OID into a local and
increments the field; `CreateTable` and `CreateIndex` keep calling the
private `newOID` under the catalog's own lock, which is fine because the
two locks are always taken in the same order.
</details>

<details><summary><b>Session.</b></summary>

Replace the `xid` field by the three state fields and a private `xid()`
that returns `tx.XID`. `Exec` switches on the AST type before analysis:
`ast.Begin`, `ast.Commit`, and `ast.Rollback` go to a private `control`,
everything else to a private `statement`. `statement` checks `failed`,
begins a transaction if `tx` is nil, analyses, runs, and finishes: on error
`Abort`, then either mark the block failed or clear `tx`; on success
`Commit` and clear `tx` unless the block is explicit. `control` is the
table in "Transactions in the session" written out; the `Warnings` slice is
set on the result, never returned as an error. Both commit paths flush
first — `pool.FlushAll` then `store.SyncAll`, one private helper — and
then call `tx.Commit`. `Close` aborts `tx` when it is set and the block
has not failed (a failed block's transaction is already aborted), then
closes the manager after the flush and before the store.
</details>

<details><summary><b>Regression runner and REPL.</b></summary>

Print each warning as `WARNING: ` plus the message and a newline before the
result, the REPL on stderr.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch16` at the end.
Every test not named in that step or an earlier one still panics.

1. Control file lock. `UpdateControl`, then `NewOID` through it. Green:
   catalog `TestUpdateControl` and the chapter 8 tests unchanged.

   ```sh
   go test -race ./internal/catalog/... -run 'TestUpdateControl'
   ```
2. Manager. `Open`, `Close`, `NextXID`, `Begin`. Green: `TestOpen`,
   `TestBegin`.

   ```sh
   go test -race ./internal/txn/... -run 'TestOpen|TestBegin'
   ```
3. Log. `offset`, `segment`, `get`, `set`; `Commit`, `Abort`, `Status`.
   Green: `TestCommitAbort`, `TestLogGolden`, `TestSegments`,
   `TestRestart`, `TestConcurrent`.

   ```sh
   go test -race ./internal/txn/... -run 'TestCommitAbort|TestLogGolden|TestSegments|TestRestart|TestConcurrent'
   ```
4. Session. The state fields, `Open`, `Close`, the commit flush, `Exec`
   with `statement` and `control`, `wrap`. Green: session
   `TestImplicitTransactions`, `TestTransactionBlock`,
   `TestTransactionWarnings`, `TestFailedTransaction`,
   `TestTransactionPersistence`, `TestCommitIsDurableWithoutClose`, and
   the chapter 11 to 15 session tests unchanged.

   ```sh
   go test -race ./internal/session/... -run 'TestImplicitTransactions|TestTransactionBlock|TestTransactionWarnings|TestFailedTransaction|TestTransactionPersistence|TestCommitIsDurableWithoutClose'
   ```
5. Output. Warnings in the regression runner and the REPL. Green:
   `TestRegress` with `ch16_txn.sql`.

   ```sh
   REGRESS_CHAPTER=16 go test -race ./internal/regress/... -run 'TestRegress'
   ```

`TestSnapshot` and `TestWait` in `internal/txn` belong to chapter 17 and
are not part of `make test-ch16`.

### When a test fails

- catalog `TestUpdateControl` — the OID counter and the XID counter
  overwrite each other. Both writers go through `UpdateControl`, which
  reads the file, applies the caller's change and writes it back under
  one lock. A catalog that kept its own copy of the control file in
  memory from chapter 08 must drop it.
- `TestBegin` — the same ID is handed out twice after a restart. The
  control file is rewritten *before* `Begin` returns, so a crash between
  the two loses an ID rather than reusing one.
- `TestLogGolden` — the bytes are right and in the wrong order within
  the byte. Transaction `x` is at byte `x / 4`, shifted left by
  `(x % 4) * 2`, so the lowest ID sits in the lowest bits and
  transaction 3 lands at `0x40`, not `0x01`.
- `TestLogGolden` — the file is longer than expected, or writing a
  status zeroes a neighbour. Write only the two bits: read the byte,
  mask the pair, or in the new value. Extending the file to reach byte
  `n` is fine and is what makes reading an untouched transaction return
  `InProgress`.
- `TestSegments` — the second segment is named `1` or `0001.0`. Four
  uppercase hex digits, so `0000`, `0001`, exactly as PostgreSQL's SLRU
  names them; segment `n` starts at transaction `n * XactsPerSegment`.
- `TestRestart` — a transaction that was running before the restart
  still reads as `InProgress`. Rule 3 asks the *manager's* set of
  running transactions, not the log, and a new manager knows none; rule
  4 then turns the log's `00` into `Aborted`. That ordering is the whole
  point of the four rules.
- `TestRestart` — `Status` of a committed transaction returns
  `ErrFutureXID` after a restart. `NextXID` comes from the control file,
  so it must be at least as high as every ID ever handed out.
- `TestCommitAbort` — a committed transaction is lost after a crash.
  `Commit` syncs the segment file before returning; `Abort` deliberately
  does not, because a lost abort reads as in progress and rule 4 turns
  that into aborted anyway.
- `TestConcurrent` — the race detector fires on the segment map, or two
  goroutines' bits clobber each other. The read-modify-write of a byte
  is a critical section, and so is opening a segment file the first
  time.
- session `TestFailedTransaction` — a statement after an error inside a
  block reports a normal error, or a `BEGIN` succeeds. Once the block is
  failed, every statement but `COMMIT` and `ROLLBACK` fails with
  `ErrInFailedTransaction` *before* it is analysed; a syntax error is
  still a syntax error, because parsing comes first.
- session `TestFailedTransaction` — `COMMIT` of a failed block reports
  `COMMIT`. Both `COMMIT` and `ROLLBACK` answer `ROLLBACK`, and neither
  writes anything to the log: the abort was recorded the moment the
  error happened.
- session `TestTransactionWarnings` — a spurious `COMMIT` returns an
  error. It succeeds with a warning and the tag `COMMIT`; warnings ride
  on `Result.Warnings` and the statement is still a success.
- session `TestImplicitTransactions` — a failing statement outside a
  block leaves a transaction running. Every statement in the idle state
  begins one and either commits or aborts it, and analysis errors count
  as failures too.
- session `TestTransactionPersistence` — rows written in a rolled-back
  block are gone. They are not: nothing in this chapter changes what a
  scan returns, and a rolled-back `INSERT` is still visible until
  chapter 17 adds the visibility rule. The test checks the stamps and
  the log, not the rows.

## Why visibility waits for chapter 17

This chapter hands out real transaction IDs and records real verdicts, and
then leaves the executor's rule alone: a tuple is dead when `xmax` is non-
zero, whatever the log says. Checking the log here without a snapshot would
be most of `HeapTupleSatisfiesMVCC` written twice, once wrong, because the
half of the rule that needs a snapshot is the half that makes the other half
correct (D23). So this chapter's tests read tuple headers and log bits
directly, its regression file never selects from a table after a rolled-back
write, and the rows only start vanishing in chapter 17. The other
consequence of having no WAL is here too: `Commit` syncs the log segment
itself and the session flushes the whole buffer pool before it, where
PostgreSQL syncs a few WAL records and writes both the log and the pages
back at a checkpoint.

## Out of scope

Lazy transaction ID assignment and virtual transaction IDs, so every
statement takes an ID; subtransactions and `SAVEPOINT`, so the log
never holds `SUB_COMMITTED` (`11`); two-phase commit; transaction ID
wraparound, freezing, and `VACUUM`, so the counter is a plain 32-bit
integer that runs out after four billion transactions (v2); the SLRU
page cache and its truncation (`pg_xact` only grows); WAL, so the
control file is rewritten per transaction and the log synced per
commit (v2); PostgreSQL's implicit transaction block around a
multi-statement query string; `COMMIT AND CHAIN`, `START TRANSACTION`,
`END`, `ABORT`, and isolation levels, which chapter 17 introduces where
it needs them; the visibility rules that make the log matter
(chapter 17).

## Check your understanding

1. A fresh cluster. Transaction 3 commits, 4 aborts, 5 is still running.
   What is in `pg_xact/0000`, and how long is the file?
   <details><summary>Answer</summary>

   Two bytes, `40 02`. Transaction 3 is byte 0 shifted 6, so `01 << 6 =
   0x40`; transaction 4 is byte 1 shifted 0, so `10 << 0 = 0x02`.
   Transaction 5 writes nothing, because `00` already means in progress.
   The file is two bytes only because byte 1 was touched.
   </details>
2. The same manager is closed and a new one opened on the directory.
   What does `Status(5)` return now, and why did it change?
   <details><summary>Answer</summary>

   `Aborted`. The bits are still `00`, but rule 3, "this manager is
   running it", no longer applies, and rule 4 reads `00` as aborted. A
   transaction whose process is gone without a verdict did not commit,
   and that is the only safe reading.
   </details>
3. Why does `Commit` sync the segment file while `Abort` does not?
   <details><summary>Answer</summary>

   Because losing the two records has opposite consequences. A lost
   commit would let a transaction that a client was told had committed
   read back as aborted, which breaks durability. A lost abort reads
   back as `00`, which after a restart means aborted anyway, so nothing
   is lost. PostgreSQL does not sync the log at all: it syncs the WAL,
   and the log follows at checkpoints. Without WAL, the log is our only
   durable record, so the commit path has to pay for the sync.
   </details>
4. Why does `Status` ask the manager whether a transaction is running
   before it looks at the log, rather than trusting `00` to mean
   "running"?
   <details><summary>Answer</summary>

   Because the log cannot distinguish a transaction that is running from
   one that died without writing a verdict. Both are `00`. Only a
   live record of who is running can tell them apart, and that record
   cannot survive a crash, which is exactly the property that makes the
   `00`-is-aborted fallback correct. PostgreSQL asks the shared process
   array for the same reason in `TransactionIdIsInProgress`.
   </details>
5. We assign an ID in `Begin`, so a read-only `SELECT` consumes one, and
   we rewrite the control file per transaction. What does PostgreSQL do
   instead?
   <details><summary>Answer</summary>

   It assigns lazily, at the first write, and gives a read-only
   transaction a virtual ID that never reaches the log or a tuple
   header; and it logs the counter to WAL once per 8192 IDs rather than
   per transaction. The first saves the 32-bit ID space, which matters
   because it wraps and wraparound must be prevented by freezing old
   tuples; a workload of read-only transactions would otherwise burn IDs
   for nothing. The second turns one `fsync` per transaction into one
   per 8192. Both are performance and space, not correctness, and both
   need machinery this book does not have.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Lazy transaction IDs.** Assign an ID at the first write instead of at
   `Begin`, with a virtual ID for read-only transactions. The counter stops
   moving for a workload of `SELECT`s, and every place that assumed a
   statement has an ID has to be found.
2. **Subtransactions.** Add `SAVEPOINT` and `ROLLBACK TO`, which is where
   the log's fourth status `SUB_COMMITTED` (`11`) comes from and where a
   commit has to walk a parent chain.
3. **Read `slru.c`.** `pg_xact` is an SLRU: a small page cache in shared
   memory with its own replacement and truncation. Ours reads and writes the
   segment file directly. Work out what the cache buys at four bytes per
   1024 transactions, and when truncation is safe.
