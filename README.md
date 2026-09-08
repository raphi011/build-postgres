# Build Your Own PostgreSQL

A guided, test-driven walk through building a relational database in the image
of PostgreSQL, starting from an empty directory.

Every chapter adds one subsystem, mirrors the design PostgreSQL actually uses,
and ships with tests that tell you when you are done. By the end you have a
single Go binary that stores tables in 8 KiB slotted pages, indexes them with an
on-disk B-tree, parses and plans SQL, and runs concurrent transactions with
MVCC snapshot isolation.

## Who this is for

You know a systems language, have used PostgreSQL, and want to understand what
happens between `psql` and the disk. No prior database-internals knowledge is
assumed. Each chapter names the PostgreSQL source files it corresponds to so
you can read the real thing alongside your own.

## How it works

- `chapters/NN-name/README.md` explains the concept, the PostgreSQL design, and
  what to build.
- `internal/...` holds package skeletons with exported signatures and tests.
  You fill in the bodies until `go test` for that package passes.
- Later chapters build on your earlier code. Nothing is thrown away.
- The `solution` branch has a reference implementation, one commit per chapter,
  so `git diff ch05 ch06` shows exactly what a chapter adds.

```sh
go test ./internal/page/...        # run one chapter's tests
make test-ch05                     # run everything up to chapter 5
make regress                       # SQL regression suite (chapter 11+)
```

## Roadmap

See [docs/PLAN.md](docs/PLAN.md) for the full chapter-by-chapter plan and
[docs/DECISIONS.md](docs/DECISIONS.md) for the design decisions behind it.

| Part | Chapters | You end up with |
|---|---|---|
| 1. Storage | 01–05 | Pages, tuples, files, buffer pool, heap scans that survive restart |
| 2. SQL front end | 06–09 | Lexer, parser, system catalog, name/type resolution |
| 3. Execution | 10–11 | Expression evaluation, iterator executor, a working REPL |
| 4. Indexing | 12–13 | On-disk B-tree, `CREATE INDEX`, index scans, `PRIMARY KEY` |
| 5. Planning | 14–15 | Cost-based scan choice, joins, `EXPLAIN` |
| 6. Transactions | 16–17 | Transaction log, MVCC snapshots, isolated concurrent sessions |

Version 2 (not yet planned in detail): VACUUM, write-ahead log and crash
recovery, aggregates, the PostgreSQL wire protocol so real `psql` connects.

## Status

Parts 1 and 2 (chapters 00–09: storage, lexer, parser, catalog,
analyzer) and chapter 10 (expression evaluation) are written on `main`
with a reference implementation on `solution`. Chapter 11 (executor and
REPL) is next.
