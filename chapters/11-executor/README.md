# Chapter 11: Executor and REPL

**Goal.** Run statements end to end with the iterator (Volcano) model: a
plan tree that says what to do, executor nodes that pull rows from each
other, and a session that takes SQL text from parse to psql-formatted
output. From here on the SQL regression suite judges every chapter.

**You edit.** `internal/plan/plan.go`: 2 functions with
`panic("not implemented")` bodies, `String` (on `ModifyOp`) and
`Explain`. `internal/planner/planner.go`: 1 function, `Plan`.
`internal/executor/executor.go`: 2 functions, `Build` and `Exec`; the
node types behind them are private and yours to design.
`internal/session/session.go`: 9 functions, `Open` through `Split`.
Nothing else changes; `plan_test.go`, `planner_test.go`,
`executor_test.go`, `session_test.go`, and `regress_test.go` with
`testdata/regress/expected/` are the contract. `cmd/pgdb` is provided
complete.

**Needs from earlier chapters.** `tuple` (02), `smgr` (03), `bufmgr`
(04), `heap` (05), `lexer` (06), `parser` (07), `catalog` (08),
`analyzer` and `query` (09), `expr` (10).

**Done when.** `make test-ch11` passes.

**Effort.** Long, about 8 hours, the longest chapter in the book and best
split over two sittings. The iterator contract in step 3 is the hard part;
steps 4 and 5 repeat it per node.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser (07) ──► Analyzer (09) ──► Planner (14, 15)
                                     │                 │
                              Catalog (08)             ▼
                                     │           Executor [10, 11]
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

Everything below this chapter is a library. This chapter makes it a
database: a plan tree that says what to do, executor nodes that do it by
pulling rows from each other, a session that runs SQL text from parse to
formatted output, and a shell to type into. From here on the SQL
regression suite runs real statements against real files, and every
later chapter is judged by it.

PostgreSQL source: `include/nodes/plannodes.h`, `executor/execMain.c`
(`ExecutorStart`, `ExecutorRun`, `ExecutorEnd`), `executor/execProcnode.c`
(`ExecInitNode`, `ExecProcNode`, `ExecEndNode`), `executor/execScan.c`,
`executor/nodeSeqscan.c`, `executor/nodeResult.c`,
`executor/nodeValuesscan.c`, `executor/nodeSort.c`, `executor/nodeLimit.c`,
`executor/nodeModifyTable.c`, `executor/execUtils.c`, `tcop/postgres.c`
(`exec_simple_query`), `tcop/pquery.c`, `tcop/utility.c`,
`commands/explain.c`, `src/bin/psql/print.c` (`print_aligned_text`),
`src/bin/psql/mainloop.c`, `src/test/regress/pg_regress.c`.

## Four packages

- `internal/plan`: the plan node structs, pure data, plus `Explain`.
  PostgreSQL's `plannodes.h`.
- `internal/planner`: `Plan(query.Stmt) plan.Node`. Until chapter 14
  there is exactly one plan for every statement, so this is a dozen
  lines that chapter 14 replaces with a cost-based search. It exists as
  its own package now so that the boundary is fixed: the executor never
  sees a `query.Stmt`, and the planner never sees a heap.
- `internal/executor`: one executor node per plan node, and `Exec` to
  drive a tree. PostgreSQL's `executor/`.
- `internal/session`: parse, analyze, plan, execute, format. PostgreSQL's
  `tcop/`, the traffic cop, plus the parts of `psql` that decide what a
  result looks like on the screen. The REPL in `cmd/pgdb` and the
  regression runner are both thin loops over `Session`.

## The iterator model

**Predict.** `SELECT a FROM t WHERE a > 1 ORDER BY b LIMIT 2` over a
three-row table. How many rows does the `SeqScan` produce before the
first row reaches the caller, and how many after?

Every executor node has `Open`, `Next`, and `Close`. `Open` acquires what
the node needs, `Next` returns one row or reports that there are none
left, `Close` releases. A node that has an input calls the input's
`Next` from inside its own; rows are pulled from the root down and flow
back up one at a time. Nothing is materialised unless a node has to
(`Sort`). This is the Volcano model of Graefe's 1994 paper and exactly
what `ExecProcNode` does: PostgreSQL's `ExecInitNode`, `ExecProcNode`,
and `ExecEndNode` dispatch on the node type to the same three
operations, and a `TupleTableSlot` is the row that comes back.

The nodes, and the PostgreSQL node each mirrors:

| plan node | rows out | PostgreSQL |
|---|---|---|
| `Result` | one empty row | `Result` (for `SELECT` without `FROM`) |
| `Values` | one row per expression list | `ValuesScan` |
| `SeqScan` | every visible tuple of a relation, with its TID | `SeqScan` |
| `Filter` | input rows for which the qual is `TRUE` | the `qual` of any scan |
| `Project` | one row of target values per input row | the `targetlist` of any node |
| `Sort` | input rows ordered by the keys | `Sort` |
| `Limit` | the first N input rows | `Limit` |
| `ModifyTable` | none; counts rows written | `ModifyTable` |

`Filter` and `Project` are separate nodes here where PostgreSQL attaches
a qual and a target list to every node and evaluates them inside
`ExecScan` and `ExecProject`. Separate nodes keep each one a few lines
and make the tree say everything; the cost is one extra `Next` call per
row, which we can afford. `Explain` prints the tree the way PostgreSQL
would anyway: `Project` is invisible and `Filter` becomes a `Filter:`
line under its input, so the output is what `EXPLAIN` shows.

Rows are `executor.Row`: an `expr.Row` (chapter 10) plus the TID of the
heap tuple it came from, so that `ModifyTable` can find the tuple to
update or delete. Each plan node reports, through `Range()`, the range
entries its rows are laid out over; a node compiles its expressions
against `expr.NewLayout(input.Range())`. With one relation per query
that is always the query's range table, and chapter 15's join nodes
concatenate their inputs.

`Open` is where compilation happens: a node compiles its expressions
once and `Next` only calls closures. `Exec` is `ExecutorStart` plus
`ExecutorRun` plus `ExecutorEnd`: open the root, pull until it is empty,
close it, even after an error.

Worked example. `SELECT a FROM t WHERE a > 1 ORDER BY b LIMIT 2` over
`t` holding `(3, 'x')`, `(1, 'y')`, `(2, 'z')` at TIDs `(0,1)`, `(0,2)`,
`(0,3)`. The plan is `Limit → Project → Sort → Filter → SeqScan`, and
this is every call that crosses a node boundary:

| # | Call | What happens |
|---|---|---|
| 1 | `Limit.Open` | compiles its count, opens `Project` |
| 2 | `Project.Open` | compiles the target list, opens `Sort` |
| 3 | `Sort.Open` | compiles the sort keys, opens `Filter` |
| 4 | `Filter.Open` | compiles the qual, opens `SeqScan` |
| 5 | `SeqScan.Open` | starts a heap scan; no page is read yet |
| 6 | `Limit.Next` | count 0 < 2, calls `Project.Next` |
| 7 | `Project.Next` | calls `Sort.Next` |
| 8 | `Sort.Next` | not sorted yet: pulls its whole input |
| 9 | `Filter.Next` | `SeqScan` gives `(3,'x')`; qual `TRUE`, returns it |
| 10 | `Filter.Next` | `SeqScan` gives `(1,'y')`; qual `FALSE`, loops; `SeqScan` gives `(2,'z')`; returns it |
| 11 | `Filter.Next` | `SeqScan` is empty, returns false |
| 12 | `Sort.Next` | sorts `['x', 'z']`, returns `(3,'x')` |
| 13 | `Project.Next` | evaluates `a`, returns the row `[3]` |
| 14 | `Limit.Next` | count 1, yields `[3]` to the caller |
| 15 | `Limit.Next` | count 1 < 2, calls `Project.Next` |
| 16 | `Sort.Next` | already sorted, returns `(2,'z')` from its buffer |
| 17 | `Limit.Next` | count 2, yields `[2]` |
| 18 | `Limit.Next` | count 2 = 2, returns false without calling `Project` |
| 19 | `Limit.Close` | closes down the tree to `SeqScan` |

Row 8 is where the pipeline stops being a pipeline. Every other node
pulls one row at a time, but `Sort` cannot answer its first `Next`
without seeing every input row, so the whole table has been scanned
before the caller gets anything. That is why `LIMIT 2` saves nothing
here: rows 9 to 11 read the entire relation. Row 18 is the part `LIMIT`
does save; without it the `Sort` would be drained.

### The plan for a SELECT

```
Limit
  Project
    Sort
      Filter
        SeqScan
```

with every optional node omitted when its clause is absent. `Sort` is
below `Project` because the analyzer resolved `ORDER BY` keys against the
range table (chapter 09): `ORDER BY b` on `SELECT a FROM t` needs `b`,
which the projection has thrown away. PostgreSQL's `grouping_planner`
solves the same problem by adding `b` to the target list as a resjunk
column, sorting, and dropping it afterwards. Ours is simpler and costs
nothing until a projection is expensive. `Limit` sits on top so it stops
pulling as soon as it has enough rows; a `LIMIT 1` over a `Sort` still
sorts everything, as PostgreSQL's does without a top-N heap. `LIMIT
NULL` is no limit and a negative count is
`LIMIT must not be negative` (2201W), both PostgreSQL's rules.

Sort order is `expr.Compare` per key, `DESC` reversed, and `NULL` sorts
after everything ascending and before everything descending, which is
PostgreSQL's default (`NULLS LAST` for `ASC`, `NULLS FIRST` for `DESC`).
Rows equal under every key keep their input order; PostgreSQL makes no
such promise, so the regression files always add a tiebreaker.

### ModifyTable

`INSERT` is `ModifyTable` over `Values`: each row is formed into a tuple
with `tuple.Form` and given to `heap.Insert`. `UPDATE` and `DELETE` are
`ModifyTable` over a `Filter` over a `SeqScan`, and the input row's TID
says which tuple to change. For `UPDATE` the `SET` expressions are
evaluated against the old row, so `SET a = b, b = a` swaps, and the
result replaces the assigned columns of that row before it is formed
into the new tuple and handed to `heap.Update`.

`NOT NULL` is checked here, per row, for `INSERT` and `UPDATE`
(`ExecConstraints`): the analyzer only rejects literal `NULL`s, and
`UPDATE t SET a = b` finds out at run time. The message is the same
`null value in column "a" of relation "t" violates not-null constraint`.
There are no transactions yet, so rows written before the failing one
stay written; chapter 16 makes the statement atomic.

Worked example. `UPDATE t SET a = a + 1` over three rows, with the write
loop as described:

| Phase | Calls | Effect |
|---|---|---|
| pull | `Filter.Next` three times, then a fourth | three rows buffered with their TIDs |
| write | `heap.Update` on each buffered TID | three new versions, three stamped `xmax` |
| result | – | `UPDATE 3` |

Doing it a row at a time instead gives `UPDATE 4`, then 5, then forever.

There is a trap in updating the relation you are scanning. `heap.Update`
inserts the new tuple version wherever there is room, which may be a
page the scan has not reached yet, and the scan would then find the new
version and update it again, forever. This is the Halloween problem,
named for the day in 1976 the IBM System R team found it. PostgreSQL
avoids it with command IDs: a tuple inserted by the current command is
invisible to that same command's scan (`HeapTupleSatisfiesMVCC` checks
`cmin` against the snapshot's `curcid`). We have no snapshots, so
`ModifyTable` pulls every input row before it writes the first one. The
test `TestUpdateHalloween` fails if you do not.

## The session

`Session.Exec` is `exec_simple_query`: parse the whole string, then for
each statement analyze, and either run it as a utility command (`CREATE
TABLE`, `DROP TABLE`, `BEGIN`, `COMMIT`, `ROLLBACK`, `EXPLAIN`; PostgreSQL's
`ProcessUtility`) or plan and execute it. Results are collected; the
first error stops the loop and is returned with the results so far.

Every tuple written in this chapter is stamped with `tuple.FrozenXID`.
PostgreSQL uses that value to mean "committed so long ago that it is
visible to everyone", which is the right meaning until chapter 16 hands
out real transaction IDs.

`BEGIN`, `COMMIT`, and `ROLLBACK` succeed and do nothing. Chapter 16 gives
them meaning.

Worked example. Two statements through a session, as the regression
files write them:

```
EXPLAIN (COSTS OFF) SELECT a FROM t AS x WHERE x.a > 1 AND b IS NOT NULL ORDER BY b DESC, a LIMIT 3;
                       QUERY PLAN                        
---------------------------------------------------------
 Limit
   ->  Sort
         Sort Key: x.b DESC, x.a
         ->  Seq Scan on t x
               Filter: ((x.a > 1) AND (x.b IS NOT NULL))
(5 rows)
```

The tree has five plan nodes and `EXPLAIN` prints four: `Project` is
invisible, and `Filter` is a line under the scan it filters rather than a
node of its own, because that is where PostgreSQL puts a qual. The alias
`x` shows up in both the scan line and every `Var`. `COSTS OFF` is what
keeps this output stable when chapter 14 adds estimates.

```
EXPLAIN (COSTS OFF) INSERT INTO t VALUES (2, 'y'), (3, 'z');
           QUERY PLAN            
---------------------------------
 Insert on t
   ->  Values Scan on "*VALUES*"
(2 rows)
```

`EXPLAIN` analyzes and plans its statement and returns the lines of
`plan.Explain` as a one-column result named `QUERY PLAN`, which is how
PostgreSQL returns it. Costs and row estimates arrive with the cost
model in chapter 14.

### Results and command tags

`Result` carries the command tag PostgreSQL sends at the end of every
statement: `SELECT 2`, `INSERT 0 3` (the 0 is the OID PostgreSQL once
returned for single-row inserts into tables with OIDs), `UPDATE 1`,
`DELETE 0`, `CREATE TABLE`, `DROP TABLE`, `BEGIN`, `COMMIT`, `ROLLBACK`,
`EXPLAIN`. A statement that returns rows has `Columns` and `Rows`; `NULL`
is a nil `Datum`.

`Result.String` is psql's aligned format (`print_aligned_text` with
`border 1`), byte for byte, because the regression files are meant to
be cross-checkable against a real server:

```
 a  |  b   
----+------
  1 | one
 22 | 
(2 rows)

```

- Every column is as wide as its widest header or value, in characters
  (not bytes), so `ü` is one wide.
- The header centres each name: `(width-len)/2` spaces on the left and
  the rest on the right.
- Cells are one space, the value, one space, joined by `|`; the
  separator line is `-` runs of `width+2` joined by `+`.
- `int4` and `int8` are right-aligned, `bool` and `text` left-aligned,
  and the last column is not padded on the right when it is
  left-aligned, so lines end where the value ends. A right-aligned last
  column is padded, so lines end at the column edge.
- `NULL` prints as nothing, `TRUE` as `t`, `FALSE` as `f`.
- The footer is `(1 row)` or `(n rows)`, then a blank line.
- A `Title` is centred over the whole table width, with no padding when
  it is wider than the table. `NoCount` drops the footer and blank line;
  `\d` output has no row count in psql either.

### Errors

Every error from `Exec` is a `*session.Error` with PostgreSQL's message
and, when the error has one, a position; `Report` renders it the way
psql does:

```
ERROR:  relation "nope" does not exist
LINE 1: select * from nope;
                      ^
```

Two spaces after `ERROR:`. `LINE n:` shows the line of the statement
containing the position, and the caret is under the column, counting
characters. Errors without a position (runtime errors, DDL failures)
print only the `ERROR` line. The messages come from wherever the error
arose: lexer and parser errors become `syntax error at or near "x"`,
`syntax error at end of input`, `type "x" does not exist`, and the
lexer's sentinel messages; analyzer and evaluation errors carry their
own text (chapters 09 and 10); catalog errors are worded as PostgreSQL
words them (`relation "t" already exists`, `table "t" does not exist`,
`permission denied: "pg_class" is a system catalog`,
`column "a" specified more than once`, `tables can have at most 1600
columns`). `Error` unwraps to the original
error, so `errors.Is(err, catalog.ErrExists)` still works.

### Splitting statements

psql sends a statement when it sees a semicolon outside a string,
quoted identifier, or comment, and drops leading whitespace and `--`
comments so that `LINE 1` is the first line of SQL. `Split` does that
with a small scanner of its own rather than chapter 06's lexer: it only
has to know where strings, quoted identifiers, and comments start and
end, and the lexer's error is sticky, so an unfinished statement (a
string the user has not closed yet) would look like a lexical error
instead of "not complete yet". An unterminated string or comment means
the statement is not complete yet. The REPL and the regression runner
both feed lines into a buffer and call `Split` until it says no.

## `\d` and `\dt`

`Session.Describe` and `Session.Tables` return `Result`s shaped like
psql's `\d table` and `\dt`, built from the catalog:

```
          Table "t"
 Column |  Type   | Nullable 
--------+---------+----------
 a      | integer | not null
 b      | text    | 
```

Type names are the SQL names PostgreSQL prints (`integer`, `bigint`,
`boolean`, `text`). `\dt` lists user tables only, sorted by name, with a
`table` type column; chapter 13 adds indexes to `\d`. An unknown table is
`Did not find any relation named "x".`, psql's wording.

## The REPL

`cmd/pgdb DATADIR` is provided complete; it is thirty lines over
`Session`. It bootstraps the directory if it is not one yet, prompts with
`pgdb=# ` and `pgdb-# ` for continuation lines like psql, prints result
sets with `Result.String` and command tags on their own line, reports
errors on stderr, and handles `\d`, `\dt`, `\q`, `\?`. When stdin is not
a terminal it prints no prompts, so `pgdb dir < file.sql` works.

One line of it is worth reading twice. `bufio.Scanner.Scan` returns
false at end of input *and* on an error, a line longer than its buffer
being the likely one; treating both as end of input runs the first third
of a piped script, prints nothing, and exits zero. Both the REPL and the
regression runner check `Err` after the loop, and the REPL turns it into
a non-zero exit.

## The regression suite

`internal/regress` is chapter 11's second test layer (D6) and the one
every later chapter adds to. For each `testdata/regress/sql/<name>.sql`
the runner opens a fresh data directory, then does what `psql -a -q`
does: echoes every nonempty input line as it reads it, runs each
statement when its semicolon arrives, and prints result sets and error
reports in place. Command tags are not printed (`-q`). The output is
written to `testdata/regress/results/<name>.out` and compared with
`testdata/regress/expected/<name>.out`; `go test ./internal/regress
-update` rewrites the expected files from the current output, to be
reviewed with `git diff` before committing. Later chapters add files;
`make test-chNN` sets `REGRESS_CHAPTER=NN` so that only the files up to
chapter NN run, and the chapter 11 files avoid output that later
chapters change (an index's `pg_class` row, the OID after one).

Files this chapter adds:

- `ch11_ddl`: `CREATE TABLE`, `DROP TABLE`, the catalogs queried as
  tables, quoted identifiers.
- `ch11_insert`: value lists, column lists, literal typing, every
  `INSERT` error.
- `ch11_select`: projection, expressions, `WHERE`, three-valued logic.
- `ch11_order`: `ORDER BY` including `NULL` placement, `LIMIT`.
- `ch11_dml`: `UPDATE` and `DELETE`.
- `ch11_errors`: syntax and analysis errors with their positions.
- `ch11_explain`: plan trees without costs.

Worked example. The runner echoes each input line and interleaves the
output, so `testdata/regress/expected/ch11_order.out` opens with the
input lines from `sql/ch11_order.sql` and then the first result:

```
-- ORDER BY and LIMIT. NULLs sort last ascending and first descending;
-- text sorts bytewise, as under the C collation: 'B' < 'a' < 'ab' < 'b'.
CREATE TABLE t (a int4, b text, c bool);
INSERT INTO t VALUES (3, 'b', true), (1, NULL, false), (4, 'B', NULL),
  (2, 'a', true), (5, NULL, false), (6, 'ab', true);
SELECT * FROM t ORDER BY b, a;
 a | b  | c 
---+----+---
 4 | B  | 
 2 | a  | t
 6 | ab | t
 3 | b  | t
 1 |    | f
 5 |    | f
(6 rows)
```

Comments and blank input lines are echoed, statements are echoed, and no
`CREATE TABLE` or `INSERT 0 6` tag appears, because the runner is
`psql -a -q`. The result block itself is `Result.String`, right down to
the trailing space after `c` in the header and the two-space separator
`+`. A `NULL` prints as nothing at all, which is why rows 5 and 6 have an
empty `b`, and `f` and `t` are how a boolean prints. The ordering is the
`C` collation from chapter 10: `B` before `a`, and both nulls last.

## API

`internal/plan`

```go
type Node interface { Range() []*query.RangeEntry }
type Result struct{}
type Values struct { Rows [][]query.Expr }
type SeqScan struct { Rel *query.RangeEntry }
type Filter struct { Input Node; Qual query.Expr }
type Project struct { Input Node; Targets []query.Target }
type Sort struct { Input Node; Keys []query.SortKey }
type Limit struct { Input Node; Count query.Expr }
type ModifyOp int // Insert, Update, Delete
type ModifyTable struct { Op ModifyOp; Rel *query.RangeEntry; Input Node; Set []query.Assignment }
func Explain(n Node) []string
```

`internal/planner`

```go
var ErrUtility, ErrJoin error
func Plan(q query.Stmt) (plan.Node, error)
```

`internal/executor`

```go
var ErrNotNull, ErrInvalidRowCount error
type Error struct { Err error; Msg string }
type Row struct { expr.Row; TID tuple.TID }
type Node interface { Open() error; Next() (Row, bool, error); Close() error }
type Env struct { Pool *bufmgr.Pool; XID tuple.XID; Snapshot heap.Snapshot; Isolation ast.Isolation } // the last two are chapter 17's
func Build(p plan.Node, env *Env) Node
func Exec(p plan.Node, env *Env) (rows []Row, processed int, err error)
```

`internal/session`

```go
const NFrames = 256
type Session struct{ ... }
func Open(dir string) (*Session, error)
func (s *Session) Catalog() *catalog.Catalog
func (s *Session) Close() error
func (s *Session) Exec(sql string) ([]*Result, error)
func (s *Session) Describe(name string) (*Result, error)
func (s *Session) Tables() (*Result, error)

type Column struct { Name string; Type tuple.TypeID }
type Result struct { Tag, Title string; NoCount bool; Columns []Column; Rows [][]tuple.Datum }
func (r *Result) String() string
func FormatDatum(v tuple.Datum) string

type Error struct { Msg string; Pos lexer.Pos; Query string; Err error }
func (e *Error) Report() string
func Split(src string) (stmt, rest string, ok bool)
```

Semantics the tests depend on:

- `plan.Explain` output as in `plan_test.go`: `Seq Scan on t`, `Seq Scan
  on t x` for an alias, `Filter: <expr>` and `One-Time Filter: <expr>`
  (under `Result`) as properties, `Sort` with `Sort Key: a, b DESC`,
  `Limit`, `Insert on t` over `Values Scan on "*VALUES*"`, `Update on t`,
  `Delete on t`, `Result`; `Project` invisible; children as `->  ` lines
  indented like PostgreSQL (properties at +2, children at +2 with the
  arrow, grandchildren at +6 from there). Expressions print with
  `query.Expr.String`.
- `planner.Plan` builds the trees in `planner_test.go` and returns
  `ErrJoin` for more than one range entry and `ErrUtility` for anything
  but `SELECT`, `INSERT`, `UPDATE`, `DELETE`.
- `Exec` returns the root's rows and, for `ModifyTable`, the number of
  rows written; a query's `processed` is its row count.
- `SeqScan` rows own their storage and carry the tuple's TID; nodes pin
  nothing between calls, so abandoned-then-closed scans do not exhaust
  the pool.
- `Filter` passes `TRUE` only; `Sort` as described above; `Limit` as
  described above; `ModifyTable` as described above including the
  Halloween test and the `NOT NULL` message.
- `Session.Exec` returns tags as listed, `Columns` only for statements
  that return rows, results before the first error alongside it, and
  every error as `*Error` with `Query` set to the whole input. Positions
  are relative to the input; the regression runner passes one statement
  at a time so they match psql.
- `Result.String`, `Error.Report`, `Describe`, `Tables`, and `Split`
  produce the exact strings in `session_test.go`.
- `Open` on a directory without a control file bootstraps it; `Close`
  flushes, so a reopened directory sees every committed row.
- The expected regression outputs in `testdata/regress/expected`.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** A type switch (`switch n := n.(type)`) is how
`Explain`, `Plan`, `Build`, and the session dispatch on node and
statement kinds. `sort.SliceStable` keeps equal rows in input order.
`errors.As(err, &e)`, with `e` a pointer to the error type, picks a typed
error out of a chain; `errors.Is` matches sentinels. A deferred closure
can assign to a named result, which is how `Exec` folds `Close`'s error
in. `append([]T(nil), s...)` copies a slice. `utf8.RuneCountInString`
counts characters; `strings.Repeat` and `strings.Builder` build padded
lines; `strconv.Itoa` and `strconv.FormatInt` print counts and numbers.

<details><summary><b><code>Explain</code>.</b></summary>

Two private functions: `describe(n)` returns a node's headline, its
property lines, and its children; `explain(n, level, &lines)` appends them
recursively. The root has indent 0; a child at level L > 0 gets `-> `
before its headline at 2+6(L-1) spaces, and a node's properties go at 6L+2.
`describe` on `Project` is `describe` on its input; on `Filter` it is the
input's description plus a `Filter: ` property (`One-Time Filter: ` when
the input is a `*Result`); `SeqScan` quotes the name with `ast.QuoteIdent`
and appends the alias only when it differs; `Sort` joins the keys with `, `
and a ` DESC` suffix; `ModifyTable` is `Op.String() + " on " + name` over
its input.
</details>

<details><summary><b><code>Plan</code>.</b></summary>

A type switch on the statement. `INSERT` is `ModifyTable` over
`Values{q.Rows}`; `UPDATE` and `DELETE` are `ModifyTable` over a private
`scan(rel, where)`, a `SeqScan` wrapped in `Filter` when `where` is non-
nil. `SELECT` with no range entry starts at `Result` (plus `Filter` for a
`WHERE`), with one at `scan`, with more returns `ErrJoin`; then `Sort` if
there are keys, always `Project`, and `Limit` if `q.Limit` is non-nil.
Everything else is `ErrUtility`.
</details>

<details><summary><b>Node types and <code>Build</code>.</b></summary>

One private struct per plan node: `result`, `values`, `seqScan`, `filter`,
`project`, `sortNode`, `limit`, `modifyTable`. `Build` is a type switch
that builds the input first and stores, for every node that evaluates
expressions against its input, `expr.NewLayout(p.Input.Range())` next to
the uncompiled expressions; nothing is compiled until `Open`. Two helpers
serve every node: `compileAll(exprs, layout)` returns a slice of
`expr.Func`, and `evalAll(funcs, row)` evaluates them into a fresh
`expr.Row`, so an output row never aliases its input.
</details>

<details><summary><b><code>Exec</code>.</b></summary>

`Build`, `Open` (return its error; nothing to close yet), then `defer` a
closure that calls `Close` and stores its error in the named `err` when
there is none. Pull `Next` until it reports no row, appending. `processed`
is `len(rows)` unless the root is a `*modifyTable`, whose private `count`
is the number of rows written.
</details>

<details><summary><b><code>seqScan</code>.</b></summary>

Fields: the `Env`, the range entry, and a `*heap.Scan`. `Open` does
`heap.Open(pool, OID, Desc).Scan(env.Snapshot)` and returns `scan.Err()`;
`Next` returns `scan.Err()` when `scan.Next()` is false, else
`tuple.Deform` of `scan.Tuple()` with `scan.TID()`; `Close` closes the
scan. Chapter 05's scan copies a page's tuples and unpins before returning,
which is what makes rows own their storage and keeps `TestNodeLifecycle`
inside the pool.
</details>

<details><summary><b><code>result</code>, <code>filter</code>, <code>project</code>.</b></summary>

`result` has a `done` flag: `Open` clears it, the first `Next` sets it and
returns an empty `Row`. `filter` compiles its qual at `Open`, then opens
the input; `Next` loops on the input until `f.Qual(r.Row)` is true,
returning any error from either. `project` compiles the targets' `Expr`s at
`Open`; `Next` pulls one row, `evalAll`s it, and keeps the input row's TID,
which `TestSort` checks survives a sort above it.
</details>

<details><summary><b><code>sortNode</code>.</b></summary>

Fields: input, layout, keys, compiled key funcs, the materialised `rows`, a
`sorted` flag, and `pos`. `Open` compiles and resets. The first `Next`
calls a private `load`: pull every input row, evaluate its keys once into a
parallel `expr.Row`, then `sort.SliceStable` with a comparator that walks
the keys and returns at the first nonzero `compareNullable`, negated for
`Desc`. `compareNullable` is 0 for two NULLs, puts a NULL after any value,
and is `expr.Compare` otherwise; the negation for `DESC` is what puts NULL
first. After that `Next` hands out `rows[pos]`.
</details>

<details><summary><b><code>limit</code>.</b></summary>

Fields: input, the count expression, `left int64`, `all bool`. `Open`
evaluates the count once against `expr.NewLayout(nil)` and an empty row:
NULL sets `all`; a negative value returns the `*Error` before the input is
opened. `Next` returns no row when `left` is 0 without touching the input,
else decrements and forwards.
</details>

<details><summary><b><code>values</code>.</b></summary>

`rows [][]query.Expr`, the compiled `funcs`, and `pos`. `Open` compiles
every list against `expr.NewLayout(nil)`; `Next` evaluates list `pos`
against `expr.Row{}` and advances.
</details>

<details><summary><b><code>modifyTable</code>.</b></summary>

Fields: `Env`, op, range entry, input, the input's layout, the `SET`
assignments with their compiled funcs, `count`, and `done`. `Open` compiles
the assignment values and opens the input. `Next` runs once: pull every
input row into a slice, then `heap.Open` the relation and `write` each row,
incrementing `count`; it never returns a row. `write` switches on the op:
`Delete` is `h.Delete(TID, XID)`; `Update` evaluates the funcs against the
old row, copies the old `Values` and `Nulls`, overwrites index `Attr` of
each assignment, forms, and `h.Update(TID, t, XID)`; `Insert` forms and
`h.Insert`. A private `form(vals, nulls)` walks `Desc.Attrs` and returns
the `*Error` for the first `NotNull` attribute that is null, else
`tuple.Form`.
</details>

<details><summary><b><code>Result.String</code>.</b></summary>

Nil `Columns` is the empty string. First pass: `FormatDatum` every cell and
take each width as the rune count of the widest header or cell. The table
width for the title is the sum of `width+2` plus `ncols-1` separators; the
title's left padding is `max(0, (total-len)/2)`. Then the header (a space,
the names centred as described above, joined by ` | `, a trailing space),
the separator, and the rows: `Int4` and `Int8` get their padding before the
value, the last column gets none, every other column after. The footer
chooses `(1 row)` or `(n rows)` unless `NoCount`; always end with the blank
line.
</details>

<details><summary><b><code>FormatDatum</code>.</b></summary>

A type switch: nil, `int32`, `int64`, `bool`, `string`.
</details>

<details><summary><b><code>Report</code>.</b></summary>

The `ERROR: ` line; then, if `Pos.Line > 0`, split `Query` on `\n`, take
line `Pos.Line-1` (empty when out of range), print it after `LINE n: `, and
put the caret after `len(prefix)+Column-1` spaces. `Column` already counts
characters and the prefix is ASCII, so byte length is right there.
</details>

<details><summary><b><code>Split</code>.</b></summary>

A byte loop over `src`. On `'` or `"` find the closing quote with
`strings.IndexByte` and jump past it (none: not complete; `''` inside a
string works because the second quote opens another string). On `--` skip
to the newline. On `/*` run a depth counter over nested comments; end of
input at depth > 0 is not complete. On `;` stop. No semicolon: not
complete. Then strip the statement: trim leading whitespace and, while it
starts with `--`, drop through the newline; `rest` is everything after the
semicolon, untouched.
</details>

<details><summary><b><code>Open</code> and <code>Close</code>.</b></summary>

`smgr.Open`, `bufmgr.New(store, NFrames)`, `catalog.Open`; when that fails
with `catalog.ErrNotBootstrapped`, `catalog.Bootstrap` instead. Any other
failure closes the store. The session's `xid` is `tuple.FrozenXID`. `Close`
is `pool.FlushAll` then `store.Close`, first error wins.
</details>

<details><summary><b><code>Exec</code> and the statement runner.</b></summary>

`parser.Parse` the whole string, then per statement `analyzer.Analyze` and
a private `run`; every error returns through `wrap(err, sql)` with the
results so far. `run` is a type switch: `CreateTable` and `DropTable` call
the catalog with `s.xid` and word its errors through `ddlError`; `Begin`,
`Commit`, `Rollback` are tags only; `Explain` plans its statement and turns
the `plan.Explain` lines into rows of one `QUERY PLAN` text column.
Anything else is `planner.Plan`, `executor.Exec` with `Env{pool, xid}`,
then a second switch to shape the `Result`: a `Select` gets a `Column` per
target (`Name`, `Expr.Type()`) and rows with NULL as nil, tag `SELECT n`;
the writes get their tag with `n`.
</details>

<details><summary><b>The error reporter.</b></summary>

`wrap` first tries `errors.As` for `*Error`, which then only lacks `Query`.
Otherwise it makes a new `*Error` with `Err` and `Query` and fills `Msg`
and `Pos` by type: `*lexer.Error` gives `Pos` and its sentinel's `Error()`
(not the outer message, which has the position in it); `*parser.Error`
gives `Pos` and one of `type X does not exist` (`ErrUnknownType`), `syntax
error at end of input` (`Found` is `end of input`), or `syntax error at or
near X`, where `Found` is already quoted; `*analyzer.Error` gives `Msg` and
`Pos`; `*expr.Error` and `*executor.Error` give `Msg` only; anything else
is `err.Error()`. `ddlError` maps the catalog sentinels to the messages
listed under Errors; for `ErrDuplicateColumn` walk `desc.Attrs` with a
seen set to name the column, since the catalog does not, and
`ErrTooManyColumns` names `catalog.MaxColumns`.
</details>

<details><summary><b><code>Describe</code> and <code>Tables</code>.</b></summary>

`Describe` is `cat.Lookup`, with `ErrNotFound` turned into the `*Error`
with psql's wording; the `Result` has `Title`, `NoCount`, three text
columns, and per attribute the name, a private `sqlTypeName` (`integer`,
`bigint`, `boolean`, `text`), and `not null` or empty. `Tables` is
`cat.Tables()`, already sorted by name, filtered to `OID >=
catalog.FirstUserOID`, with `Title` `List of relations` and columns `Name`
and `Type` (always `table`).
</details>

## Suggested order

One step at a time with the line under it; `make test-ch11` at the end.
Every test not named in that step or an earlier one still panics.

1. Plan tree. `String` on `ModifyOp`, `Explain`. Green: `TestRange`
   (passes already; `Range` is not stubbed), `TestModifyOpString`,
   `TestExplain`. Indentation as in the `Explain` note above.

   ```sh
   go test -race ./internal/plan/... -run 'TestRange$|TestModifyOpString|TestExplain$'
   ```
2. Planner. `Plan`. Green: `TestPlanGolden`, `TestPlanShape`,
   `TestPlanErrors`. Shape as in the `Plan` note above; `TestPlanShape`
   checks the node order that EXPLAIN output hides.

   ```sh
   go test -race ./internal/planner/... -run 'TestPlanGolden|TestPlanShape|TestPlanErrors'
   ```
3. Driver and scan. `Build`, `Exec`, and the SeqScan node. Green:
   `TestSeqScan`, `TestSeqScanManyPages`, `TestNodeLifecycle`. Hold no
   pin between `Next` calls (see `seqScan` above) or `TestNodeLifecycle`
   exhausts the 16-frame pool.

   ```sh
   go test -race ./internal/executor/... -run 'TestSeqScan|TestSeqScanManyPages|TestNodeLifecycle'
   ```
4. Query nodes. The Result, Filter, Project, Sort, and Limit nodes.
   Green: `TestResult`, `TestFilter`, `TestProject`, `TestSort`,
   `TestLimit`. One note per node above; a negative count is `*Error`
   wrapping `ErrInvalidRowCount` with the message `LIMIT must not be
   negative`.

   ```sh
   go test -race ./internal/executor/... -run 'TestResult|TestFilter|TestProject|TestSort|TestLimit'
   ```
5. Writes. The Values and ModifyTable nodes. Green: `TestInsert`,
   `TestUpdate`, `TestUpdateHalloween`, `TestDelete`. See the
   `modifyTable` note above; stamp tuples with `env.XID`.

   ```sh
   go test -race ./internal/executor/... -run 'TestInsert$|TestUpdate$|TestUpdateHalloween|TestDelete$'
   ```
6. Formatting. `String` on `Result`, `FormatDatum`, `Report`, `Split`.
   Green: `TestResultString`, `TestFormatDatum`, `TestErrorReport`,
   `TestSplit`. Golden bytes with trailing spaces; see the four notes
   above.

   ```sh
   go test -race ./internal/session/... -run 'TestResultString|TestFormatDatum|TestErrorReport|TestSplit'
   ```
7. Session. `Open`, `Close`, `Exec`, `Describe`, `Tables`. Green:
   `TestExec`, `TestExecMany`, `TestPersistence`, `TestErrors`,
   `TestTooManyColumns`, the session's `TestExplain`, `TestDescribe`.
   See the session notes above; `TestErrors` lists the message and
   position of every error, and `TestTooManyColumns` the wording of
   `catalog.ErrTooManyColumns`, `tables can have at most 1600 columns`.

   ```sh
   go test -race ./internal/session/... -run 'TestExec$|TestExecMany|TestPersistence|TestErrors|TestTooManyColumns|TestExplain$|TestDescribe'
   ```
8. Regression suite. No stubs. `TestRegress` in `internal/regress` runs
   every `testdata/regress/sql/ch11_*.sql` through a fresh `Session` the
   way `psql -a -q` would and compares the output byte for byte with
   `testdata/regress/expected/`; it is the judge of the whole chapter.
   Review any `-update` diff line by line before committing.

   ```sh
   REGRESS_CHAPTER=11 go test -race ./internal/regress/... -run 'TestRegress'
   ```

```sh
go test -race ./internal/regress/...          # or: make regress
go test ./internal/regress/... -update        # regenerate expected files
go run ./cmd/pgdb /tmp/pgdata                 # try it
```

The four packages also hold tests belonging to later chapters:
`TestEstimateString`, `TestExplainCosts`, `TestEstimates`,
`TestScanChoice`, `TestAnalyze` and `TestPlansAgree` for chapter 14;
`TestIndexScan`, `TestIndexScanShape`, `TestInsertMaintainsIndexes`,
`TestUpdateMaintainsIndexes`, `TestDeleteLeavesIndexEntries`,
`TestUniqueViolation`, `TestIndexRowTooLarge`,
`TestUpdateThroughIndexScan` and `TestIndexes` for chapter 13;
`TestJoinRange`, `TestExplainJoins`, `TestJoinGolden`,
`TestJoinEstimates`, `TestJoinShape`, `TestNestLoop`,
`TestNestLoopParam`, `TestHashJoin`, `TestRescan`, `TestJoinOracle`,
`TestJoins` and `TestJoinPlans` for chapter 15; and the transaction,
snapshot and concurrency tests for chapters 16 and 17. None of them is
part of `make test-ch11`.

### When a test fails

- `TestExplain` — the tree is right and the indentation is not. Each
  level adds two spaces, and the `->  ` arrow has two spaces after it, so
  a child's text starts six columns right of its parent's.
- `TestExplain` — a `Project` node appears in the output. `Project` is
  invisible and `Filter` prints as a `Filter:` line under its input, so
  the printed tree is PostgreSQL's, not ours.
- `TestPlanShape` — `EXPLAIN` output matches but the shape test fails.
  `Sort` goes *below* `Project`, because the analyzer resolved the sort
  keys against the range table and the projection has already discarded
  the columns they need. The printed plan hides the difference.
- `TestPlanErrors` — a negative `LIMIT` is planned rather than rejected.
  It is an `*Error` wrapping `ErrInvalidRowCount` with the message
  `LIMIT must not be negative`; `LIMIT NULL` is no limit at all.
- `TestNodeLifecycle` — `ErrNoUnpinnedBuffers` out of the 16-frame pool.
  The scan must hold no pin between `Next` calls, exactly as chapter 05's
  heap scan does; the pool is deliberately small enough to catch a leak.
- `TestNodeLifecycle` — a node reopened after `Close` returns stale rows.
  `Open` resets every piece of per-run state, including a `Sort`'s
  buffer and position and a `Limit`'s count; it is not only a
  compilation step.
- `TestSort` — nulls sort first ascending. The default is `NULLS LAST`
  for `ASC` and `NULLS FIRST` for `DESC`, so a null is treated as
  greater than everything and the `DESC` reversal then moves it to the
  front.
- `TestSort` — rows equal under every key come back in a different
  order. The sort must be stable; the regression output depends on it
  even though PostgreSQL promises nothing.
- `TestLimit` — the count is evaluated once per row. Evaluate it in
  `Open`; it cannot reference columns, which is what makes that legal.
- `TestProject` — the projected row keeps the input's TID and
  `ModifyTable` then updates the wrong tuple. Decide deliberately what
  each node passes through, and remember `ModifyTable` sits under no
  `Project` in an `UPDATE` plan.
- `TestUpdate` — `SET a = b, b = a` leaves both columns equal. Every
  assignment is evaluated against the *old* row before any of them is
  applied, so the swap works.
- `TestUpdateHalloween` — the test hangs or reports far too many rows
  updated. `heap.Update` puts the new version wherever there is room,
  possibly on a page the scan has not reached, and the scan then finds
  it again. Pull every input row into a slice before writing the first
  one.
- `TestInsert` — a `NOT NULL` violation is not raised for
  `INSERT INTO t SELECT`-shaped values that are not literal nulls. The
  analyzer only rejects literal `NULL`; the executor checks every row.
- `TestResultString` — the golden bytes differ by whitespace. Every
  column is padded to its widest value, the separator is ` | `, the rule
  line uses `-` and `+`, and every data line and the header end with a
  trailing space. Compare with `cat -A`.
- `TestFormatDatum` — a `NULL` prints as `NULL` or `<nil>`. It prints as
  the empty string; booleans print as `t` and `f`.
- `TestErrorReport` — a `LINE` appears for an error whose `Pos.Line` is
  0. Chapter 09 leaves the position zero for errors PostgreSQL reports
  without one, and the report must respect that.
- `TestSplit` — a statement is split inside a string literal or a
  comment containing a semicolon. `Split` scans quotes and comments by
  hand rather than calling the lexer, so a bad input cannot arm a sticky
  lexer error and hang the REPL.
- `TestErrors` — the loop stops at the first error but the earlier
  results are lost. `Exec` returns the results so far alongside the
  error.
- `TestRegress` — a diff in the trailing whitespace of a header line.
  The runner is `psql -a -q`: input lines are echoed, command tags are
  not, and the result blocks are `Result.String` verbatim. Compare
  `testdata/regress/results/` with `expected/` using `git diff
  --no-index`.
- `TestRegress` — an output changes when a later chapter is implemented.
  A chapter 11 file must not print anything a later chapter alters, such
  as a `pg_class` row that an index will add or an OID that shifts.

## Why the REPL is chapter 11 and not chapter 02

The alternative order is the one most tutorials take: build the SQL front
end first over an in-memory table, get something running in an evening, and
swap in real storage later. It gets you a demo ten chapters earlier and an
executor that is rewritten when the page format arrives, along with the
tests that pinned the wrong behaviour. Bottom-up costs ten chapters before
you can type a query and buys an executor that runs on the page format from
chapter 01 with nothing thrown away (D2), and a second test layer that
speaks SQL from here on (D6). This chapter is where that bet pays out: every
subsystem below it is already tested, so what breaks in the REPL is the
wiring you wrote today.

## Out of scope

Joins (chapter 15), aggregates and `GROUP BY` (v2), `RETURNING`, `DEFAULT`
values, `INSERT ... SELECT`, prepared statements and the extended
protocol, cursors and `FETCH`, `SELECT INTO`, a real `psql` (`\copy`,
`\x`, history, tab completion), long-line truncation in error reports
(psql's `...`), psql's other output formats, `COMMIT` warnings outside a
transaction (chapter 16), `HINT:` and `DETAIL:` lines under an error
(PostgreSQL adds them to type mismatches and constraint violations), and
any atomicity of statements that fail halfway (chapter 16).

## Check your understanding

1. `SELECT a FROM t WHERE a > 1 ORDER BY b LIMIT 2` over a 1000-row
   table. How many rows does the `SeqScan` return, and how many does the
   `Project` node evaluate?
   <details><summary>Answer</summary>

   All 1000 from the scan, and 2 from the projection. `Sort` cannot
   return its first row without its whole input, so the `LIMIT` saves
   nothing below the sort; above it, `Limit` stops calling `Project`
   after two rows. A top-N heap in `Sort` would fix the memory but not
   the scan; PostgreSQL has one and we do not.
   </details>
2. `t` holds `(3,'b')`, `(1,NULL)`, `(4,'B')`, `(2,'a')`. What is
   `SELECT a FROM t ORDER BY b` in order, and why?
   <details><summary>Answer</summary>

   4, 2, 3, 1. Text compares bytewise, so `'B'` (0x42) sorts before
   `'a'` (0x61) and `'a'` before `'b'`, and the `NULL` sorts last because
   ascending order is `NULLS LAST`. Under PostgreSQL's usual `en_US`
   collation the first two would swap.
   </details>
3. Why does `Sort` sit below `Project` in the plan, when PostgreSQL puts
   the sort above the target list?
   <details><summary>Answer</summary>

   Because chapter 09 resolved `ORDER BY` keys against the range table,
   so a key may name a column the projection discards:
   `SELECT a FROM t ORDER BY b` needs `b` at sort time. PostgreSQL
   handles it by adding `b` to the target list as a resjunk column,
   sorting, and dropping it again. Sorting the input rows costs nothing
   extra here and needs no junk-column machinery; it would start to cost
   something if a projection were expensive enough to be worth doing
   before a sort discards rows, which `LIMIT` never lets happen anyway.
   </details>
4. Why does `ModifyTable` read its entire input before writing anything,
   and what would PostgreSQL do instead?
   <details><summary>Answer</summary>

   Because `heap.Update` writes the new version wherever there is room,
   which can be a page the scan has not reached; the scan would then find
   the new version, update it again, and never stop. That is the
   Halloween problem. PostgreSQL streams the rows and relies on command
   IDs instead: a tuple written by the current command carries a `cmin`
   the command's own snapshot excludes, so its scan cannot see it. We
   have no snapshots until chapter 17, and buffering is correct at any
   size of table we test.
   </details>
5. `Filter` and `Project` are separate nodes here, where PostgreSQL hangs
   a qual and a target list on every node. What does each choice cost?
   <details><summary>Answer</summary>

   Ours costs one extra `Next` call and one row copy per row per node,
   on the hottest path in the system; PostgreSQL's `ExecScan` applies the
   qual and the projection inside the scan's own loop with no dispatch at
   all, which is why `EXPLAIN` shows them as annotations rather than
   nodes. What we get is that every node is a few lines long and the
   plan tree says exactly what happens, with no rule about which node
   secretly evaluates what. `Explain` then has to hide the difference,
   which is the small price of making the output match PostgreSQL's.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **`RETURNING`.** Give `INSERT`, `UPDATE` and `DELETE` a target list
   evaluated over the row that was written. `ModifyTable` already
   materialises its input, so the rows are in hand; the parser, analyzer and
   plan node all need a place to put them.
2. **Aggregates.** Add `COUNT(*)` and `SUM(a)` without `GROUP BY` first: a
   node that consumes its whole input and yields one row. `GROUP BY` then
   wants either a sort or a hash table, which is the interesting decision.
3. **Read `psql`'s `\d` implementation** (`describe.c`) next to our
   `Describe`. It is a SQL query against the catalogs, sent like any other;
   ours is Go code calling `Catalog`. Which parts of `\d` could we express
   as SQL today, and which need catalog features we do not have?
