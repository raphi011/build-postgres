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
