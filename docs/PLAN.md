# Chapter plan (v1)

Order is dependency-driven and bottom-up, the way the PostgreSQL source tree is
layered: storage, then access methods, then the SQL front end, then the
executor, then the planner, then transactions. Two deliberate deviations keep
the reader motivated:

- A REPL that runs real SQL arrives at chapter 11, before indexes, planning,
  or transactions exist. Everything after that improves a working database.
- Data structures are laid out for their final form from day one. Page headers
  carry an LSN from chapter 1 and tuple headers carry `xmin`/`xmax` from
  chapter 2, even though WAL and MVCC come much later. This avoids rewriting
  the storage layer and matches how PostgreSQL's on-disk format looks.

Each chapter has: goal, the PostgreSQL design it mirrors (with source paths in
the PostgreSQL repo under `src/backend/`), what to build, what the tests check,
and what is explicitly out of scope.

## Conventions used throughout

- Language: Go, standard library only. Module path `github.com/raphi011/build-postgres`.
- Page size 8192 bytes, little-endian, fixed forever.
- Supported types in v1: `int4`, `int8`, `bool`, `text`. Enough for every
  concept without a type-system chapter.
- Each chapter maps to one or two packages under `internal/`. Tests live next
  to the code. Chapter 11 onward also uses SQL regression tests in
  `testdata/regress/` (see DECISIONS.md, D6).
- Error behaviour is tested as strictly as success behaviour.

---

## Chapter 00: Introduction

Goal: orient the reader. Repo layout, how the skeleton and tests work, a map
of PostgreSQL's subsystems annotated with the chapter that builds each one,
and the conventions above. Contains the `cmd/pgdb` stub that later chapters
grow into the REPL.

Tests: `go build ./...` succeeds.

---

## Part 1: Storage

### Chapter 01: Pages

Goal: an 8 KiB slotted page that stores variable-length items.

PostgreSQL: `storage/page/bufpage.c`, `include/storage/bufpage.h`,
`include/storage/itemid.h`.

Build: `internal/page`. `PageHeader` (`lsn`, `checksum`, `flags`, `lower`,
`upper`, `special`), an array of 4-byte line pointers growing down from the
header, item data growing up from the end. `Init`, `AddItem`, `GetItem`,
`DeleteItem` (mark unused), `FreeSpace`, `Compact`. Line pointer numbers are
stable across compaction so that tuple IDs stay valid.

Tests: header round-trips through bytes; items retrievable by number after
adds and deletes; `AddItem` fails when full; `FreeSpace` accounting is exact;
`Compact` reclaims deleted space and keeps live item numbers unchanged;
property test against a map for random add/delete sequences.

Out of scope: checksums (field exists, always zero), special space users.

### Chapter 02: Heap tuples

Goal: the on-disk tuple format and value encoding.

PostgreSQL: `access/common/heaptuple.c`, `include/access/htup_details.h`.

Build: `internal/tuple`. `TupleHeader` with `xmin`, `xmax`, `infomask`,
`natts`, null bitmap, and `hoff`. `Datum` encoding for the four types, with
4-byte alignment for fixed types and a length prefix for `text`. `Form(desc,
values, nulls)` and `Deform(desc, bytes)`. A `TupleDesc` type describing
column names and types, shared with every later layer.

Tests: round-trip every type including NULL; null bitmap only present when
needed; alignment padding matches the documented layout byte-for-byte
(golden byte slices); deforming a tuple with a trailing added column returns
NULL for it.

Out of scope: TOAST, varlena 1-byte headers, `xvac`, `t_ctid` chains.

Tools: `cmd/pgdb` gains two subcommands, provided complete rather than
stubbed. `pgdb sample DIR` writes a data directory holding one relation of
three rows; `pgdb dump FILE [BLOCK]` prints each page's header, its line
pointers, and, on a heap page, the tuple headers behind them. They are the
first runnable thing in the book and the debugging tool for every later
chapter; `cmd/pgdb` joins the chapter 02 test target.

### Chapter 03: Relation files and the storage manager

Goal: read and write pages of a relation file on disk.

PostgreSQL: `storage/smgr/md.c`, `storage/smgr/smgr.c`.

Build: `internal/smgr`. Relations identified by an OID, stored as
`<datadir>/base/<oid>`. `Open`, `Read(blockNo) -> page`, `Write(blockNo,
page)`, `Extend() -> blockNo`, `NBlocks`, `Close`. A `DataDir` type that owns
the directory and hands out relation handles.

Tests: extend then read back across close/reopen; reading past the end is an
error; block numbers are dense; two relations do not interfere.

Out of scope: segment files at 1 GiB, forks (FSM, VM), tablespaces, fsync
ordering (that arrives with WAL in v2).

### Chapter 04: Buffer manager

Goal: a fixed-size pool of in-memory pages between callers and the storage
manager.

PostgreSQL: `storage/buffer/bufmgr.c`, `storage/buffer/freelist.c`.

Build: `internal/bufmgr`. `Pool` with N frames. `Pin(rel, blockNo) -> *Buffer`
that loads on miss, `Unpin`, `MarkDirty`, `Flush(buffer)`, `FlushAll`. Buffer
lookup by `(rel, blockNo)` in a hash table. Clock-sweep replacement with a
usage counter; pinned frames are never evicted. Hit/miss counters exposed for
tests.

Tests: repeated pins hit; more distinct pages than frames cause eviction of
the unpinned page with lowest usage; evicting a dirty page writes it; pinned
pages are never evicted and the pool returns an error when everything is
pinned; `FlushAll` makes the on-disk state match; concurrent pins from
goroutines are safe (`-race`).

Out of scope: per-buffer content locks beyond a mutex, background writer,
ring buffers.

### Chapter 05: Heap access method

Goal: insert, scan, delete, and update tuples in a relation through the buffer
manager.

PostgreSQL: `access/heap/heapam.c`, `access/heap/hio.c`, `storage/freespace/`.

Build: `internal/heap`. `Relation` bound to a `TupleDesc`, an OID, and the
pool. `Insert(tuple) -> TID` (block number + line pointer) using a simple
in-memory free space map with a fallback linear search and extend. `Scan()`
iterator yielding `(TID, tuple)`. `Fetch(TID)`. `Delete(TID)` sets `xmax`.
`Update(TID, tuple)` is delete plus insert and returns the new TID. Visibility
in this chapter: a tuple is visible iff `xmax == 0`.

Tests: inserted tuples come back from a scan in insertion order; scan spans
many pages; deleted tuples disappear from scans; updated tuples appear once;
data survives `FlushAll`, pool discard, and reopen; tuple larger than a page
is rejected.

Out of scope: HOT, real free space map fork, MVCC visibility (chapter 17).

---

## Part 2: SQL front end

### Chapter 06: Lexer

Goal: turn SQL text into tokens.

PostgreSQL: `parser/scan.l`.

Build: `internal/sql/lexer`. Keywords (case-insensitive), identifiers
(including double-quoted), integer literals, single-quoted strings with `''`
escaping, operators (`= <> < <= > >= + - * / ( ) , ; .`), comments (`--` and
`/* */`), positions for error messages.

Tests: table-driven token streams; error position for an unterminated string;
identifier case folding matches PostgreSQL (unquoted lower-cased, quoted
preserved).

### Chapter 07: Parser and AST

Goal: recursive-descent parser producing an AST.

PostgreSQL: `parser/gram.y`, `include/nodes/parsenodes.h`.

Build: `internal/sql/ast`, `internal/sql/parser`. Statements: `CREATE TABLE`
(columns, types, `NOT NULL`, `PRIMARY KEY`), `DROP TABLE`, `INSERT ... VALUES`
(multi-row), `SELECT` with select list including `*`, `FROM` with one or more
tables and aliases, `JOIN ... ON`, `WHERE`, `ORDER BY ... ASC|DESC`, `LIMIT`,
`UPDATE ... SET ... WHERE`, `DELETE FROM ... WHERE`, `BEGIN`, `COMMIT`,
`ROLLBACK`, `EXPLAIN`. Expressions via Pratt parsing with PostgreSQL's
precedence table: literals, column refs (qualified or not), unary minus,
arithmetic, comparison, `IS [NOT] NULL`, `AND`, `OR`, `NOT`, parentheses.

Tests: golden tests comparing a pretty-printed AST per statement; precedence
cases (`a OR b AND c`, `NOT a = b`, `-a * b`); syntax errors report position
and expected token.

Out of scope: subqueries, aggregates, `GROUP BY`, `CREATE INDEX` (added in
chapter 13), everything else.

### Chapter 08: System catalog

Goal: tables that describe tables, stored as heap tables themselves.

PostgreSQL: `catalog/`, `include/catalog/pg_class.h`,
`include/catalog/pg_attribute.h`, `utils/cache/relcache.c`.

Build: `internal/catalog`. `pg_class` (oid, relname, relkind, relpages,
reltuples), `pg_attribute` (attrelid, attname, atttypid, attnum, attnotnull).
Bootstrap: on an empty data dir, create the catalog relations with hard-coded
OIDs and insert their own rows. `CreateTable(name, desc) -> oid`, `DropTable`,
`Lookup(name) -> RelationInfo` with a small in-memory cache invalidated on
DDL. OID allocation via a global counter persisted in a control file.

Tests: bootstrap creates a data dir whose catalog describes itself; created
tables are found after reopen; duplicate name is an error; drop removes both
catalog rows and the relation file; OIDs never repeat across restarts.

Out of scope: `pg_type` (types are a fixed enum), schemas, `pg_index` until
chapter 13.

### Chapter 09: Analyzer

Goal: turn an AST into a bound `Query` with resolved columns and checked types.

PostgreSQL: `parser/analyze.c`, `parser/parse_expr.c`, `parser/parse_relation.c`,
`include/nodes/primnodes.h`.

Build: `internal/sql/analyzer`. Range table of relations with aliases;
column references become `(rangeIndex, attnum)`; every expression gets a
result type; `*` expands; `ORDER BY` targets resolve to output columns or
expressions; `INSERT` value lists are checked for arity and type; implicit
`int4` to `int8` widening only.

Tests: unknown table, unknown column, ambiguous column, type mismatch
(`text > int`), wrong `INSERT` arity, `NOT NULL` violation at analysis time
for literal NULLs, and successful binding checked against golden
`Query` dumps.

---

## Part 3: Execution

### Chapter 10: Expression evaluation

Goal: evaluate bound expressions against a row.

PostgreSQL: `executor/execExpr.c`, `executor/execExprInterp.c`,
`utils/adt/int.c`, `utils/adt/bool.c`.

Build: `internal/executor/expr`. Compile a bound expression into a closure
tree `func(row) (Datum, isNull)`. Operators for the four types, three-valued
logic for `AND`/`OR`/`NOT`, `IS NULL`, comparison of `text` bytewise (`C`
collation), integer overflow is an error like PostgreSQL.

Tests: exhaustive truth tables for three-valued logic; `NULL = NULL` is NULL;
`WHERE` treats NULL as false; overflow detection; text comparison order.

### Chapter 11: Executor and REPL

Goal: run statements end to end with the iterator (Volcano) model.

PostgreSQL: `executor/execMain.c`, `executor/execProcnode.c`,
`executor/nodeSeqscan.c`, `executor/nodeSort.c`, `executor/nodeLimit.c`,
`executor/nodeModifyTable.c`.

Build: `internal/executor`. A `Plan` node tree (for now produced by a trivial
planner that emits SeqScan, Filter, Project, Sort, Limit, and ModifyTable),
each node implementing `Open`, `Next() (row, ok)`, `Close`. A `Session` type
wiring lexer, parser, analyzer, planner stub, and executor:
`Session.Exec(sql) -> Result`. `cmd/pgdb` becomes a REPL over a data
directory with PostgreSQL-style aligned output and `\d`, `\dt`, `\q`.

Tests: package tests per node; the SQL regression suite starts here.
`testdata/regress/sql/ch11_*.sql` with `expected/*.out` covering DDL, inserts,
projection, filters, ordering (including NULLs last for ASC, matching
PostgreSQL), limits, update, delete, and error messages.

Out of scope: joins (chapter 15), aggregates (v2).

---

## Part 4: Indexing

### Chapter 12: B-tree

Goal: an on-disk B+tree keyed by tuples, storing TIDs.

PostgreSQL: `access/nbtree/README`, `nbtinsert.c`, `nbtsearch.c`, `nbtpage.c`.

Build: `internal/btree`. Metapage (root block, height). Internal and leaf
pages built on chapter 1 pages, using the special space for `btpo_prev`,
`btpo_next`, level, and flags. Leaf items are `(key, TID)`; internal items are
`(separator key, child block)`. High key on every non-rightmost page as in
PostgreSQL. `Search(key) -> leaf position`, `Insert(key, TID)` with leaf and
internal splits propagating to a new root, `Scan(lowKey, highKey)` forward
range scan following `btpo_next`. Duplicate keys allowed, ordered by TID
(PostgreSQL 12+ behaviour). Single writer, no concurrency.

Tests: property tests inserting random keys and comparing every range scan to
a sorted slice; forced splits with large keys; tree height grows and root
changes; `Scan` across page boundaries; persistence across reopen; a
structural checker that walks every page and validates ordering, high keys,
and sibling links (like `amcheck`).

Out of scope: deletion, page reuse, Lehman-Yao concurrency, suffix truncation,
deduplication.

### Chapter 13: Index integration

Goal: `CREATE INDEX`, `PRIMARY KEY`, and an index scan node.

PostgreSQL: `catalog/index.c`, `executor/nodeIndexscan.c`,
`access/index/indexam.c`, `include/catalog/pg_index.h`.

Build: `pg_index` catalog table; `CREATE [UNIQUE] INDEX name ON table
(column)` and `DROP INDEX` through parser, analyzer, and catalog; heap
`Insert`/`Update`/`Delete` maintain every index on the relation (deletes leave
dead index entries, like PostgreSQL); `PRIMARY KEY` creates a unique index;
unique violation is an error with PostgreSQL's message; `IndexScan` executor
node over a range.

Tests: regression files for index DDL and unique violations; index contents
match heap after mixed DML (checked with the structural checker plus a heap
cross-check); index scan returns the same rows as a filtered seq scan for
random predicates.

---

## Part 5: Planning

### Chapter 14: Planner and EXPLAIN

Goal: choose between sequential and index scans with a cost model.

PostgreSQL: `optimizer/README`, `optimizer/path/costsize.c`,
`optimizer/path/indxpath.c`, `optimizer/plan/createplan.c`,
`commands/explain.c`.

Build: `internal/planner` replaces the stub. Path generation for a single
relation: SeqScan always; IndexScan for each index whose column appears in an
equality or range predicate in `WHERE` (extracted from an AND tree). Cost
model with `seq_page_cost`, `random_page_cost`, `cpu_tuple_cost`,
`cpu_index_tuple_cost`, `cpu_operator_cost` at PostgreSQL's defaults;
selectivity estimates fixed at PostgreSQL's defaults (`0.005` for equality,
`0.333` for inequality) with `relpages` and `reltuples` maintained by
`ANALYZE`. Sort pushdown: an index scan whose order matches `ORDER BY`
removes the Sort node. `EXPLAIN` prints the plan tree with costs and row
estimates in PostgreSQL's text format.

Tests: `EXPLAIN` golden tests across small and large tables showing the switch
from index to seq scan as selectivity changes; sort elimination; `ANALYZE`
updates statistics; results are identical whichever plan is chosen (property
test).

Out of scope: histograms and MCVs, multi-column indexes, bitmap scans.

### Chapter 15: Joins

Goal: nested loop and hash joins, with a join order chosen by cost.

PostgreSQL: `executor/nodeNestloop.c`, `executor/nodeHashjoin.c`,
`optimizer/path/joinpath.c`, `optimizer/path/joinrels.c`.

Build: `NestLoop` and `HashJoin` executor nodes for inner joins; `NestLoop`
with a parameterised inner `IndexScan` when an index matches the join key;
planner enumerates join orders for up to four relations by dynamic
programming over relation sets (the same shape as `make_rel_from_joinlist`),
picks the cheapest, and applies `WHERE` predicates at the lowest level where
all their columns are available.

Tests: regression files for two- and three-table joins with expected output
and expected `EXPLAIN` plans; join results checked against a naive
cross-product-and-filter oracle for random data; the planner picks the
parameterised index nested loop when the inner table is large and indexed.

Out of scope: outer joins, merge join, join order beyond four relations.

---

## Part 6: Transactions

### Chapter 16: Transaction manager and commit log

Goal: transaction IDs and a durable record of which ones committed.

PostgreSQL: `access/transam/xact.c`, `access/transam/varsup.c`,
`access/transam/clog.c`, `access/transam/README`.

Build: `internal/txn`. 32-bit `XID` allocation from a counter in the control
file, with `FrozenXID` and `BootstrapXID` reserved like PostgreSQL. `pg_xact`
as a bitmap file with two bits per transaction (in progress, committed,
aborted). `Begin() -> *Transaction`, `Commit`, `Abort`, `Status(xid)`.
`Session` gains explicit `BEGIN`/`COMMIT`/`ROLLBACK` and implicit
single-statement transactions. Heap `Insert` now stamps `xmin`, `Delete`
stamps `xmax`, `Update` does both.

Tests: status persists across restart; a transaction left open at shutdown
reads as aborted after restart; XIDs increase monotonically across restarts;
`COMMIT` outside a transaction is a warning not an error, matching
PostgreSQL.

Out of scope: subtransactions, two-phase commit, wraparound and freezing.

### Chapter 17: MVCC snapshots and visibility

Goal: concurrent sessions each see a consistent snapshot.

PostgreSQL: `utils/time/snapmgr.c`, `access/heap/heapam_visibility.c`
(`HeapTupleSatisfiesMVCC`), `storage/ipc/procarray.c`, `access/transam/README`
section on MVCC.

Build: `internal/mvcc`. `Snapshot{xmin, xmax, xip}` taken from a shared
transaction list at statement start (READ COMMITTED) or transaction start
(REPEATABLE READ). The visibility function implementing PostgreSQL's rules
for `xmin`/`xmax` against the snapshot and the commit log, including the
"my own transaction" cases. Hint bits `XMIN_COMMITTED`, `XMIN_INVALID`,
`XMAX_COMMITTED`, `XMAX_INVALID` set lazily in the tuple header to avoid
repeat commit-log lookups. Heap scans and index fetches take a snapshot.
`Update` and `Delete` on a tuple already modified by a concurrent
uncommitted transaction block until it finishes, then re-check visibility
(the READ COMMITTED update rule). Row-level waiting uses a per-transaction
lock the way PostgreSQL waits on the transaction ID lock.

Tests: an isolation-test runner in the style of `src/test/isolation` with
specs listing sessions and permutations of steps. Specs cover: uncommitted
inserts invisible to other sessions; ROLLBACK makes inserts vanish; a
session sees its own uncommitted writes; REPEATABLE READ does not see
commits after snapshot; READ COMMITTED does; concurrent updates to the same
row serialise and the second sees the first's committed value; deleted rows
stay visible to older snapshots; hint bits get set and the commit log is not
consulted again (counter check). All run under `-race`.

Out of scope: SERIALIZABLE, VACUUM (v2), lock manager beyond transaction
locks, deadlock detection.

---

## What v1 leaves out, and where it goes

Version 2, in intended order:

1. VACUUM: reclaim dead tuples and index entries, `relfrozenxid`.
2. Write-ahead log: records for heap and B-tree changes, page LSNs enforced
   in the buffer manager, fsync on commit.
3. Crash recovery and checkpoints: redo from the last checkpoint, tested by
   discarding the buffer pool mid-transaction.
4. Aggregates and `GROUP BY` (hash and sorted aggregation).
5. PostgreSQL wire protocol v3 so real `psql` and drivers connect.

Not planned: TOAST, tablespaces, partitioning, parallel query, replication,
JIT, the extension system, more than four data types.
