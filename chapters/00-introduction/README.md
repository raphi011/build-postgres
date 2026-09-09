# Chapter 00: Introduction

## What you are building

A relational database that behaves like a small PostgreSQL: tables stored in
8 KiB pages on disk, an on-disk B-tree index, a SQL parser, a cost-based
planner, an iterator executor, and transactions with MVCC snapshot isolation.
Everything is written in Go with no dependencies outside the standard
library.

## How PostgreSQL is put together

A query enters at the top and data lives at the bottom. Each box is a
chapter, and every chapter reprints this map with its own box in
brackets.

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
                              Pages (01) and tuples (02) on disk
```

We build from the bottom up. That means several chapters pass before you can
type a query, but nothing you write is throwaway: the page format from
chapter 01 is the page format the finished database uses.

## Repository layout

```
chapters/NN-name/README.md   the text for each chapter
internal/<pkg>/              one or two packages per chapter, with tests
cmd/pgdb/                    the binary; a REPL from chapter 11
testdata/regress/            SQL regression tests, from chapter 11
docs/DECISIONS.md            why things are the way they are
```

Every package on `main` ships with its exported API declared and each body
replaced by `panic("not implemented")`. Your job in each chapter is to make
the package's tests pass without changing the tests. The `solution` branch
holds a reference implementation, one commit per chapter, tagged `ch01`
through `ch17`.

## Working through a chapter

1. Read the chapter text. The block under the title says what the
   chapter builds, which files you edit, when you are done, and roughly
   how long it takes and which step is the hard one. The rest explains
   the concept, points at the PostgreSQL source files it mirrors, and
   specifies the on-disk format or API exactly where the tests depend on
   it. "Suggested order" at the end lists the steps and the tests each
   one turns green.
2. Read the tests. They are the contract.
3. Implement until `go test ./internal/<pkg>/...` passes, then run the
   chapter's `make` target to confirm nothing earlier broke.
4. Compare with the solution branch when you want to, under "Using the
   solution branch" below.

Five things in a chapter are there to be used, not skimmed:

- A **Predict.** paragraph opens the hardest design section. Answer it
  before you read on, in your head or on paper. Being wrong is the point;
  the section that follows tells you why.
- Every section that pins a byte layout or an algorithm ends with a
  worked example on concrete data. When a golden-byte test fails, the
  worked example is the thing to compare your output against, field by
  field. Chapter 01's page dump is the model.
- "Suggested order" gives each step the exact `go test -race ... -run`
  line for that step's tests. Every test not named in that step or an
  earlier one still panics; that is expected and not a regression.
  Chapter 01 shows what one step looks like from panic to `ok`.
- "When a test fails" at the end of "Suggested order" is keyed by the
  symptom, not by the fix. Find your failure there before you reach for
  the solution branch.
- "Check your understanding" and "Challenges" close every chapter. The
  questions take a minute each and their answers are folded away; the
  challenges are untested extensions, and since every later chapter
  expects the implementation the tests describe, they belong on a branch
  of their own.

The hand-holding thins out on purpose. Chapters 01 to 05 sketch every
function in the open. From chapter 06 those sketches are folded away, so
the default reading of a chapter is its design, its API and its tests,
and the sketch is there when you want to compare or when you are stuck.
From chapter 12 each chapter also names one area it does not sketch at
all and leaves to you.

## Using the solution branch

The `solution` branch is there to be read. Reading it too early costs you
the chapter; refusing to read it costs you an evening.

When to look: after "When a test fails" in that chapter's "Suggested
order" has not explained your failure and the test has been red for
something like an hour with no hypothesis left to try.

How to look: at the one function the failing test names, not at the file
and not at the chapter. The tags let you do that without leaving your
branch or touching your working tree.

```sh
git show ch04:internal/bufmgr/bufmgr.go                       # whole file
git show ch04:internal/bufmgr/bufmgr.go | sed -n '/^func (p \*Pool) Pin/,/^}/p'
```

Read it, close it, write your own version rather than pasting it; the
next chapter assumes you understand this one. When a chapter is done,
`git diff ch04 ch05 -- internal/` shows everything the reference
implementation did in it, which is worth reading even when your tests
pass.

If it is an earlier chapter that blocks you and you would rather move on,
take its implementation wholesale:

```sh
git checkout ch03 -- internal/smgr    # borrow chapter 03
git checkout main -- internal/smgr    # put the skeleton back
```

That chapter's tests then pass without your code, and every later chapter
you build sits on someone else's storage manager.

## Conventions

- Page size is 8192 bytes. All multi-byte integers on disk are little-endian.
- Offsets and item numbers on a page are 1-based, as in PostgreSQL.
  Block numbers within a file are 0-based, as in PostgreSQL.
- Types in v1: `int4`, `int8`, `bool`, `text`.
- Errors are values. Corrupted on-disk data is an error, not a panic.
  Programming errors (wrong argument types, nil pointers) may panic.
- Tests run with `-race`. Anything that claims to be safe for concurrent use
  must be.

## PostgreSQL source

Chapter texts reference paths under `src/backend/` and `src/include/` in the
PostgreSQL repository. Reading the real code next to your own is the point
of the exercise. Clone it:

```sh
git clone --depth 1 https://git.postgresql.org/git/postgresql.git
```

## Check your toolchain

```sh
go version        # 1.26 or newer
go build ./...
```

## Check your understanding

1. Trace `SELECT * FROM t WHERE id = 5` down the diagram above, given a
   B-tree index on `t.id`. Which boxes does it touch, in order?
   <details><summary>Answer</summary>

   Lexer (06) to parser (07) to analyzer (09), which resolves `t` and
   `id` through the catalog (08), to the planner (14, 15), which decides
   between a sequential scan and an index scan. The executor (11) then
   drives an index scan (13) over the B-tree (12), which yields tuple
   identifiers into the heap (05). Every page either access method wants
   comes from the buffer manager (04) and, on a miss, the storage manager
   (03), and is decoded with the page (01) and tuple (02) code. MVCC (17)
   decides which versions of the matching rows this transaction may see.
   </details>
2. Pages are 8192 bytes and integers on disk are little-endian. At what
   byte offset does block 3 of a relation file start, and how is the
   `uint16` value 8192 written there?
   <details><summary>Answer</summary>

   Block numbers are 0-based, so block 3 starts at `3 * 8192 = 24576`.
   8192 is `0x2000`, and little-endian puts the low byte first: `00 20`.
   Getting that order backwards is the most common way to fail chapter
   01's first test.
   </details>
3. Building bottom-up means eleven chapters before you can type a query.
   What does that buy over starting with a SQL front end?
   <details><summary>Answer</summary>

   Nothing gets thrown away. The page format written in chapter 01 is the
   one the finished database uses, so each chapter adds a layer rather
   than replacing a placeholder. The alternative, a top-down toy with an
   in-memory store, means rewriting the executor and the access methods
   once real storage arrives. D2 records the compromise: storage first,
   then just enough front end to reach a REPL at chapter 11, after which
   every chapter improves a system that already runs.
   </details>
4. Why does the reference implementation live on a separate branch rather
   than in the same tree behind a build tag or a directory?
   <details><summary>Answer</summary>

   So that the skeleton and the solution are the same files. `main` holds
   the API, the doc comments and the tests; `solution` holds the same
   files with the bodies filled in. That makes `git diff ch04 ch05 --
   internal/` exactly the code one chapter adds, and it means you cannot
   read the answer by accident while looking up a signature. D14 explains
   why `solution` merges `main` rather than rebasing onto it.
   </details>
5. Here, corrupted on-disk data is an error value and never a panic.
   PostgreSQL reports most corruption as an ordinary error too, but some
   of it as `PANIC`, which restarts the whole server. Why does it need
   that option and we do not?
   <details><summary>Answer</summary>

   PostgreSQL is many processes sharing one buffer pool and one lock
   table. A backend that finds shared state inconsistent cannot promise
   that unwinding its own transaction leaves that state usable for
   everyone else, so the safe move is to kill every backend and rebuild
   from the write-ahead log during crash recovery. Our database is one
   process, and v1 has no shared memory across processes and no WAL to
   recover from, so returning an error up the call stack loses nothing.
   </details>
