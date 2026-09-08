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
there too (`index.Check`), so that `index.Insert` cannot fail after the
heap write. Index scans exist as an executor node from this chapter; the
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
