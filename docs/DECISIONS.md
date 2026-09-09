# Design decisions

Numbered so they can be referenced from chapters and issues. Each records the
choice, the alternatives considered, and why.

## D1: Go, standard library only

Chosen over Rust and Python. Go gives a single static binary, a built-in test
runner and race detector, goroutines for the concurrency chapters, and low
ceremony so chapter code stays about databases rather than about the
language. No third-party dependencies: the reader should be able to read every
line that runs. Go 1.26 or newer.

## D2: Bottom-up chapter order with an early REPL

PostgreSQL's own layering (storage, access methods, parser, executor, planner,
transactions) determines dependencies. Pure bottom-up means eleven chapters
before running SQL; pure top-down means an in-memory toy that gets rewritten.
Compromise: storage first, then just enough front end to reach a REPL at
chapter 11, then every remaining chapter improves a working system.

## D3: On-disk formats are final from the chapter that introduces them

Page headers include an LSN in chapter 1 and tuple headers include
`xmin`/`xmax`/`infomask` in chapter 2, unused until later chapters fill them
in. The alternative, adding fields when needed, forces rewrites of the
storage layer and its tests and hides why PostgreSQL's formats look the way
they do.

## D4: Mirror PostgreSQL's design, not its code

Names, file layout, and algorithms follow PostgreSQL closely enough that the
reader can open the corresponding source file and recognise it. Where
PostgreSQL's design exists to handle scale or history we do not need (segment
files, TOAST, wraparound, Lehman-Yao concurrency), we drop it and say so in the
chapter's "out of scope" section rather than substituting a different design.

## D5: Four data types

`int4`, `int8`, `bool`, `text`. Fixed-width, variable-width, and a boolean are
enough to exercise alignment, null bitmaps, comparison, and sorting. A type
system and `pg_type` are not a v1 concern.

## D6: Two test layers

1. Go unit and property tests next to each package. These are the primary
   gate for every chapter and the only gate before chapter 11.
2. From chapter 11, SQL regression tests modelled on `pg_regress`:
   `testdata/regress/sql/<name>.sql` executed through `Session`, output
   compared to `testdata/regress/expected/<name>.out`. Output format matches
   `psql` aligned mode so expected files can be cross-checked against real
   PostgreSQL. Files are prefixed with the chapter that introduces them.
3. From chapter 17, isolation specs modelled on `src/test/isolation`: a spec
   declares setup SQL, named sessions with named steps, and permutations;
   the runner executes each permutation and compares output.

## D7: Skeleton on `main`, reference implementation on `solution`

`main` holds chapter text, package skeletons with exported signatures and
`panic("not implemented")` bodies, and all tests. `solution` holds the
finished implementation as one commit per chapter, tagged `ch01` through
`ch17`, so `git diff ch12 ch13` is the minimal change for a chapter. CI runs
the full test suite on `solution` and `go vet` plus `go build` on `main`.
Readers work on their own branch off `main`.

Rejected: a `solutions/` directory inside the module. It either compiles as
dead code the tests never exercise or requires build tags that complicate
every command in the book.

## D8: Chapter gating by package, not build tags

Each chapter lists the `go test` command for its packages. A `Makefile`
target per chapter runs the packages of that chapter and all earlier ones.
Tests for chapter N never import a package from chapter N+1. Build tags were
rejected because they hide which tests run and break editor tooling.

## D9: Concurrency arrives with the buffer manager, not with MVCC

The buffer pool (chapter 4) is safe for concurrent pins from the start and
tested under `-race`, so chapter 17 can add concurrent sessions without
revisiting storage. Everything between chapters 5 and 16 assumes a single
writer and says so.

## D10: Single-column B-tree without deletion in v1

Insert-only trees with a structural checker cover search, splits, root
growth, high keys, and sibling links, which is the conceptual content.
Deletion and page reuse belong to VACUUM in v2. Multi-column keys add
comparison plumbing but no new ideas.

## D11: WAL and recovery are v2

They are the most requested "storage system" topic after MVCC, but they
touch every chapter (page LSNs, buffer flush ordering, heap and B-tree record
types, commit). Putting them last in v1 would make the book's final third a
cross-cutting refactor. v1 ends with a database that is correct under
concurrent transactions and durable via explicit flushes at commit. v2 opens
with VACUUM, then WAL, then recovery, all on a stable base.

## D12: Base identifier types live in `internal/tuple`

`OID`, `XID`, `BlockNumber`, and `TID` are defined in the tuple package
because the tuple header stores them. A separate leaf package would name
things more precisely but would contain no behaviour.

## D13: Heap methods take an explicit XID from chapter 05

`Insert`, `Delete`, and `Update` accept the transaction ID and store it
without interpreting it. Chapter 16 supplies real values without changing
signatures. Chapter 17 adds a snapshot argument to `Scan` and `Fetch`.

## D14: The solution branch merges main

Each chapter on `solution` is a merge of `main` plus one implementation
commit, tagged `chNN`. A linear rebase would need conflict resolution
whenever a later chapter edits an earlier skeleton. `git diff ch04 ch05 --
internal/` is the supported way to view a chapter's delta.

## D15: No CI and no license file

The repository is for personal use. Correctness is checked by running the
test suite on `solution` before tagging.

## D16: One token kind for all keywords, one kind per operator

The lexer returns every keyword as `Kind == Keyword` with the lower-cased
word in `Text`, and gives each operator and punctuation character its own
`Kind`. The operator set is closed by the expression grammar and the parser
switches on it structurally; the keyword list grows with later chapters
(`INDEX`, `UNIQUE`, `ANALYZE`) and is easier to extend as a table than as
an enum plus `String()` method. PostgreSQL generates a token per keyword
from `kwlist.h`; the parser-facing effect is the same.

## D17: Every keyword is reserved

PostgreSQL has four keyword classes so that `key`, `level`, or `name` can
still be a column name. We have one class and keep the list short instead.
Type names (`int4`, `text`) are identifiers that the parser recognises,
which is also how PostgreSQL treats the ones that are not SQL-standard
words.

## D18: The AST prints as canonical SQL, and that is the golden format

Every node has a `String()` that renders SQL with upper-case keywords,
every operator application in parentheses, and identifiers quoted only
when needed. The parser tests compare against this text instead of
against struct literals, so a precedence case reads as `(a OR (b AND c))`
rather than three nested composite literals, and every golden case is
re-parsed from its rendering to check that the printer and the parser
agree. Chapter 14's `EXPLAIN` reuses the expression printer.

## D19: The parser maps type names, and returns a statement list

There is no `pg_type`, so `int`, `integer`, `int4`, `bigint`, `int8`,
`bool`, `boolean`, and `text` are resolved to `tuple.TypeID` in the
parser and an unknown type name is a parse error (`ErrUnknownType`)
rather than an analysis error. `Parse` returns every statement in the
input, separated by semicolons, so the regression runner (chapter 11) can
hand it a whole file; the REPL passes one statement at a time.

## D20: The executor maintains indexes; the unique check precedes the write

`CREATE INDEX` is split between two layers: `catalog.CreateIndex` records
the index and creates an empty tree, and `index.Build` fills it, so a
failed build (duplicates under a unique index) is undone by dropping the
index again. DML goes through `internal/index` from `ModifyTable`
(`ExecInsertIndexTuples` in PostgreSQL): every insert and update adds an
entry to every index of the table, a delete leaves its entry behind, and
an index scan checks the heap tuple's visibility. The unique check runs
before the heap tuple is written, unlike PostgreSQL, which inserts the
heap tuple first and relies on the transaction abort to remove it: there
is no rollback until chapter 16, and a violation must leave nothing
behind. For the same reason the size limit of an index tuple is checked
there too (`index.Check`), which is the only part of the check that
cannot change under the checker's feet.

The unique part can, so `index.Insert` repeats it. A check that reads the
index and returns, with the write following separately, decides nothing:
two transactions both find the key absent, both write it, and the index
holds a duplicate that a later `REINDEX` refuses to build. PostgreSQL has
no separate window because `_bt_check_unique` runs inside `_bt_doinsert`
with the leaf page's write lock held. There is no such page lock here —
the check scans through a `btree.Scan`, which unpins every page it leaves
— so `internal/index` keeps one mutex per relation OID and holds it
across the repeated check and the index writes. It is never held across a
wait for another transaction: the wait releases it and the check starts
again, since the transaction being waited for may want it too. It is also
never held across the heap write, which is why the heap tuple goes in
first and the repeat can find a conflict after it: under MVCC the
transaction aborts and buries it; before chapter 16 that leaves a row
behind, which is the price of a race those chapters have no way to
resolve anyway. Index scans exist as an executor node from this chapter; the
planner keeps choosing sequential scans until chapter 14 has statistics
to choose with.

## D21: A cost model with default selectivities and no column statistics

The planner uses PostgreSQL's cost parameters and formulas (`cost_seqscan`,
`cost_index` with Mackert-Lohman page fetches, `cost_sort`, the LIMIT
fraction), fed by `relpages` and `reltuples` that `ANALYZE` records, but
no per-column statistics: every predicate takes the default selectivity
PostgreSQL uses without a histogram (`0.005` for `=`, `1/3` for one
inequality, `0.005` for a range). Histograms and most-common-value lists
would add a sampling pass and a `pg_statistic` catalog for one more
input to the same formulas; the defaults already make the planner switch
between index and sequential scans as a table grows, which is the effect
the chapter is about. Two consequences are visible in `EXPLAIN`: row
estimates are the same for every value, and small analysed tables scan
sequentially even for an equality on an indexed column, since a fetch of
0.5 percent of the rows costs more than reading one page.

## D22: Join selectivity from a default distinct count

Chapter 14 gave every predicate a constant selectivity. A join needs one
more number, because the rows an equality join keeps depend on how many
distinct values the key has, and a constant would make every join look
alike. The planner uses PostgreSQL's fallback in `eqjoinsel`: a column
has as many distinct values as its table has rows when a unique index
says so or the table is smaller than `DefaultNumDistinct` (200), and
200 otherwise; the join keeps one row in the larger of the two counts.
The same count sizes hash buckets (`estimate_hash_bucket_stats`). It is
the last piece of `pg_statistic` the planner does without: with it, a
primary-key join estimates one match per outer row and a join between
two large unanalysed tables one in 200, which is enough for the planner
to prefer hashing a small table and probing an index of a large one.
Real distinct counts would come from the sampling pass that histograms
need, which stays out of scope.

## D23: The commit log is durable per commit; visibility waits a chapter

Chapter 16 gives every statement a real transaction ID and records its
verdict in `pg_xact`, but leaves the executor's `xmax == 0` rule alone.
The alternative, a first visibility check against the log without a
snapshot, would be most of chapter 17's `HeapTupleSatisfiesMVCC` written
twice, once wrong. So this chapter's tests read tuple headers and log
bits, and its regression file never selects from a table after a
rolled-back write; chapter 17 adds the snapshot, the visibility
function, and the regression output that shows rows vanish.

Two smaller choices follow from having no WAL. The control file is
rewritten on every `Begin`, as it already was on every `NewOID`, under
one package-wide lock (`UpdateControl`, PostgreSQL's `ControlFileLock`)
because both counters share the file. And `Commit` syncs the commit log
segment itself, because the log is the only durable record of a commit;
PostgreSQL syncs the WAL and writes the log back at checkpoints. That
last point is also why the session flushes the buffer pool and syncs the
relation files before `Commit` writes the bit: a log that vouches for
pages still in memory is worse than one that says nothing (D11). IDs
are assigned at `Begin` rather than at the first write, so a `SELECT`
consumes one; lazy assignment is an optimisation the tests do not need.

## D24: One snapshot interface, waits in the heap, files removed at commit

Chapter 17's visibility rules need the transaction manager, and the
transaction manager (through the control file) needs the catalog, which
needs the heap; so the heap cannot import `mvcc`. The heap instead
declares the `Snapshot` interface it asks its questions of, mirroring the
four `HeapTupleSatisfies*` entry points it needs (`Visible`, `Dirty`,
`Modify`, and `Wait`), and `mvcc.Snapshot` implements it over
`txn.Manager`. A nil `Snapshot` is the rule of chapters 5 to 16, so
those chapters' tests pass nil and change nothing else. The wait for a
transaction that holds a tuple lives in `heap.Delete` and `Update`, as
in `heap_delete`; the executor handles what comes after the wait
(EvalPlanQual under READ COMMITTED, the serialization failure under
REPEATABLE READ). Command IDs are not needed because `ModifyTable`
materialises its input (chapter 11), so a statement never sees its own
new versions.

Two consequences for earlier chapters. The catalog no longer unlinks a
relation file in `DropTable`: a rolled-back `DROP TABLE` must keep its
data, so the file is removed by `EndTransaction(true)` and a rolled-back
`CREATE TABLE`'s file by `EndTransaction(false)`, PostgreSQL's pending
deletes. And the catalog reads with a fresh snapshot per scan rather
than the transaction's, as PostgreSQL's catalog snapshot does, so that
DDL committed by another session is seen at once even under REPEATABLE
READ; each session owns a `Catalog` whose relation cache is dropped at
every statement, in place of shared invalidation messages.

Rejected: a `Session` per goroutine over the old single-session design,
with a global lock around each statement. It would pass the plan's
tests without any concurrency, which is the opposite of the chapter's
point.

## D25: Update checks first and buries the new version if it loses

`heap.Update` inserts the new version between the two halves of the
delete: it asks `Modify` whether it may replace the old tuple, inserts,
and only then stamps xmax and the ctid. The order is chapter 5's, and it
is what makes a failed insert harmless — an insert that cannot get a
buffer must not leave a row deleted with nothing to replace it, and there
is no undo log to put it back.

The cost is that the check no longer decides the outcome. Another
transaction can take the row for the whole length of the insert, so the
stamp asks `Modify` again under the old version's page lock; that second
call is where two concurrent updates are serialised, and the loser comes
back with `ErrAlreadyDeleted` having already inserted a tuple. It stamps
that tuple with its own xid before returning, so xmin and xmax are the
same transaction and no snapshot sees it, its own included. Stamping a
version nobody has been told about is not an undo: the tuple was never
reachable, no ctid points at it, and no index entry was written for it
(the executor indexes only after `Update` returns).

PostgreSQL is not in this position. `heap_update` sets a *lock-only*
xmax on the old version before it goes looking for a page, so a
concurrent updater waits on a tuple that is still live, and the lock is
released by the transaction rather than undone. That needs
`HEAP_XMAX_LOCK_ONLY`, a visibility rule that ignores such an xmax, and
a `Lock` that writes — the row locks chapter 17 leaves out of scope.

Rejected: stamping the old version's xmax before the insert, as chapters
5 to 16 did before this. It serialises two updaters on the old tuple,
which is why it looked right, but a failed insert then leaves a row
deleted with no replacement and no way to put it back.

## D26: Implementation sketches fade from chapter 06

Chapters 01 to 05 print every function sketch in the open. From chapter 06
each entry but "Go you will need" is folded into `<details>`, and from chapter
12 one named area per chapter is left unsketched, introduced by a `**Yours to
design.**` paragraph that names it and the constraints the tests pin. The
alternative, keeping every sketch open and relying on the questions and
challenges to force generation, leaves a reader who follows the notes having
designed nothing; deleting the sketches instead of folding them strands a
reader who is stuck. Folding keeps the ladder from sketch to debugging to
design while the answer stays one click away.

## D27: Worked examples are prose, not Go `Example` tests

Every design section that pins a layout or an algorithm ends with a worked
example on concrete data: a labelled dump for a layout, a table with one row
per step for an algorithm. Go `Example` tests with `// Output:` blocks are
checkable and show up in godoc, but on `main` they panic like every other
test, they add names to the Suggested order list, and they cannot be read
until the package compiles. A prose example is readable on the first pass,
which is when the reader needs it.

## D28: Questions and challenges live in the chapter README

"Check your understanding" (five questions, answers in `<details>`) and
"Challenges" (two or three, the last a reading challenge) are sections of the
chapter README, not a separate exercises file and not an appendix of answers.
The reader who has just finished Out of scope is the one the questions are
for, and a second file would drift from the chapter it tests. Answers are
folded so the page still reads as one document.
