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
- `cmd/pgdb` is a REPL from chapter 11 and, from chapter 02, a page inspector:
  `pgdb sample DIR` writes a small relation and `pgdb dump FILE [BLOCK]` prints
  its page headers, line pointers and tuple headers.

```sh
go test ./internal/page/...        # run one chapter's tests
make test-ch05                     # run everything up to chapter 5
make regress                       # SQL regression suite (chapter 11+)
```

Design decisions are in [docs/DECISIONS.md](docs/DECISIONS.md).

## What comes after v1

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
