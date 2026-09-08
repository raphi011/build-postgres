# Chapter 15: Joins

**Goal.** Nested loop and hash joins for inner joins; a nested loop whose
inner index scan takes the outer row as its parameter; and a planner
that enumerates join orders over sets of relations by dynamic
programming, places every `WHERE` qual at the lowest level that has all
its columns, and keeps the cheapest plan.

**You edit.** `internal/planner/planner.go`: 1 function with a
`panic("not implemented")` body, `Plan`, which now searches join
orders. Nothing else is newly stubbed; the rest is your own bodies from
earlier chapters. `internal/sql/analyzer`: `addRel` fills the new
`RangeEntry.Index`. `internal/executor/expr`: `NewLayout` lays out by
`Index`. `internal/plan`: `Explain` prints the four new nodes
(`NestLoop`, `HashJoin`, `Hash`, `Materialize`). `internal/executor`:
every node gains `Rescan`, `Build` builds the new nodes, and the index
scan evaluates its bounds against the outer row. The tests of every
package above are the contract; `ch11_errors.sql` loses the line that
said joins were unsupported, and the chapter 14 `TestPlanErrors` loses
`ErrJoin`, which is gone.

**Needs from earlier chapters.** `internal/plan` and the executor's
nodes (chapters 11, 13), `internal/executor/expr` (`Layout`, `Compile`,
`Func.Qual`), `internal/catalog` (`RelationInfo.Indexes`,
`IndexInfo.Unique`), the cost model of chapter 14, `hash/fnv`,
`encoding/binary`, and `math/bits`. No earlier package was stubbed for
this chapter.

**Done when.** `make test-ch15` passes.

**Effort.** Long, about 7 hours. The hash join (step 5) and the path
search over relation sets (step 6) are the hard part; `Rescan` (step 3)
touches every node you already wrote.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser (07) ──► Analyzer (09) ──► Planner [14, 15]
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

Chapter 9 already flattened `t JOIN u ON t.a = u.a` into a range table
of two entries and one qual, and chapter 10 laid a row of both tables
out as one slice; the planner then refused anything with two entries.
This chapter removes that refusal. Two things have to exist first: an
executor node that pairs rows of two inputs, and a way to build the
plan tree above it. PostgreSQL has three join methods and this chapter
builds two of them. The nested loop is the general one: it runs the
inner side once per outer row, either replaying a `Materialize` of it
or, when an index of the inner relation matches the join key, probing
that index with the outer row's value, which is how a lookup on a large
table stays cheap. The hash join reads the inner side once into a hash
table and probes it with every outer row, and is what PostgreSQL picks
for an equality join between two large tables. Merge join, the third,
needs sorted inputs and adds nothing to the picture the two others
give.

The planner's job grows more than the executor's. With one relation it
compared scans; with several it must decide which pairs to join first
and which side to put where, and the number of orders grows with the
factorial of the relation count. PostgreSQL's answer, in
`standard_join_search`, is dynamic programming over sets of relations:
the best ways to read each single relation, then each pair, then each
triple, every level built from the levels below, with `add_path`
keeping only paths that are not beaten on every count. The same search
decides where quals go: one on a single relation is applied at its
scan, one over two relations at the first join that has both, which is
what "pushing quals down" means. Costs need one estimate the chapter 14
model lacked, how many rows an equality join keeps, and D22 records the
default PostgreSQL uses without column statistics.

Exhaustive is the word to watch. Enumerating every subset inside every
subset is `O(4^n)`, so each extra relation costs about three and a half
times the last: ten relations plan in a fifth of a second, thirteen in
ten seconds, and sixteen never finish while the session sits there with
no way to interrupt it. PostgreSQL hands the problem to a genetic search
above `geqo_threshold`, twelve relations. Here the query is refused
instead: `MaxJoinRelations` is the same twelve, and a longer `FROM` list
is `ErrTooManyRelations`. It also keeps `relSet` honest, since
`1 << len(rng)` wraps to zero at 64 relations and would leave the search
looping over an all-ones mask.

PostgreSQL source: `executor/nodeNestloop.c` (`ExecNestLoop`),
`executor/nodeHashjoin.c` (`ExecHashJoin`), `executor/nodeHash.c`
(`MultiExecHash`, `ExecChooseHashTableSize`),
`executor/nodeMaterial.c`, `executor/execAmi.c` (`ExecReScan`),
`optimizer/README` (the section on join order), `optimizer/path/allpaths.c`
(`make_one_rel`), `optimizer/path/joinrels.c` (`standard_join_search`,
`make_join_rel`), `optimizer/path/joinpath.c` (`add_paths_to_joinrel`,
`match_unsorted_outer`, `hash_inner_and_outer`),
`optimizer/path/indxpath.c` (`match_join_clauses_to_index`),
`optimizer/util/pathnode.c` (`add_path`, `create_nestloop_path`),
`optimizer/path/costsize.c` (`cost_material`, `cost_rescan`,
`initial_cost_nestloop`, `final_cost_nestloop`, `initial_cost_hashjoin`,
`final_cost_hashjoin`, `calc_joinrel_size_estimate`),
`utils/adt/selfuncs.c` (`eqjoinsel`, `var_eq_non_const`,
`get_variable_numdistinct`, `estimate_hash_bucket_stats`), and
`commands/explain.c` (the join cases of `ExplainNode`).

## Rows of several relations

A `Var` names its relation by position in the query's range table, and
chapter 10's `Layout` assumed a row held the range table's entries in
that order. A join breaks the assumption: its output row is the outer
row followed by the inner row, and the planner is free to put `u`
outside and `t` inside. So `query.RangeEntry` gains `Index`, its
position in the range table, set by the analyzer, and `expr.NewLayout`
keys the layout by it: element `i` is the slot of the first column of
the entry whose `Index` is `i`, `-1` for an entry the row does not
hold, the last element the total width. The columns themselves sit in
the order the entries were given.

| entries given | t (3 cols, Index 0), u (2 cols, Index 1) | layout |
|---|---|---|
| `[t, u]` | t at 0, u at 3 | `[0 3 5]` |
| `[u, t]` | u at 0, t at 2 | `[2 0 5]` |
| `[u]` | u at 0, t absent | `[-1 0 2]` |

Worked example. `SELECT t.b, u.d FROM t JOIN u ON t.a = u.a` with `t`
three columns wide and `u` two, planned with `u` on the outside. The
join's row is `u`'s two columns then `t`'s three, so `Range()` is
`[u, t]` and the layout is `[2 0 5]`: `t`'s columns start at slot 2 and
`u`'s at slot 0, while element 0 of the layout still belongs to `t`
because `t` has `Index` 0 in the query's range table. A `Var` for `t.b`
is `{Rel: 0, Attr: 1}` and resolves to slot 3, and one for `u.d` is
`{Rel: 1, Attr: 1}` and resolves to slot 1. Nothing above the join has
to know which side was outer.

A join node's `Range()` is the outer node's range followed by the
inner's, in a fresh slice, so a `Project`, `Filter`, or `Sort` above a
join compiles against `NewLayout(input.Range())` as before and the
`Var`s of any relation find their slots. `Hash` and `Materialize` pass
their input's range through.

## Rescan

The inner side of a nested loop has to start over for every outer row.
PostgreSQL restarts a node with `ExecReScan` rather than shutting it
down and initialising it again, because a node that stored something
on its first pass, a materialised inner side or a hash table, should
keep it. So `executor.Node` gains `Rescan() error`: after it, `Next`
yields the node's rows from the first again, and a node that depends
on an outer row reads the current one.

- `Result` and `Values` reset their position; `Sort` drops its sorted
  rows and rescans its input; `Limit` restores its count; `Filter`,
  `Project`, `ModifyTable`, and `Hash` rescan their input.
- `SeqScan` closes its heap scan and opens a new one.
- `IndexScan` closes its tree scan and evaluates its bounds again on
  the next `Next`, against the outer row when it has one.
- `Materialize` goes back to its first stored row; it does not read its
  input again.
- `NestLoop` rescans its outer side and forgets its current outer row;
  the inner side is rescanned when the next outer row arrives.
- `HashJoin` rescans its outer side and keeps the hash table.

## Nested loop

`plan.NestLoop{Outer, Inner, Qual}` yields, for each outer row in
order, that row joined with every inner row for which `Qual` is TRUE
(every inner row when `Qual` is nil), in the inner side's order. The
executor opens both sides in `Open`; for each outer row it stores the
row where the inner side can see it, calls `Rescan` on the inner side,
and pulls it dry. Joined rows have no TID.

The planner puts one of two things under the inner side. When the inner
path does not depend on the outer row, it is wrapped in
`plan.Materialize`, which stores the rows of its input on the first
pass and replays them on every `Rescan`, so a sequential scan under it
reads its pages once, as `cost_rescan` prefers. When an index of the
inner relation matches a join qual, the inner is an `IndexScan` whose
qual has the outer relation's column on the right: `u.a = t.a` with the
index on `u.a` becomes `Index Cond: (u.a = t.a)`, and the scan is
parameterised by `t`. The executor compiles each qual's right side
against the outer side's layout and evaluates it on the first `Next`
after `Open` or `Rescan`, which is after the loop has stored the outer
row; the bounds then tighten exactly as in chapter 13, a NULL bound
matches nothing, and an evaluation error stops the loop. A `Filter`
between the loop and the scan applies the inner relation's own quals.

## Hash join

**Predict.** `t JOIN u ON t.a = u.a` where `u` holds a row with a NULL
`a`. Which side is hashed, and what happens to the NULL row: is it
stored in the table, does it probe, or neither?

`plan.HashJoin{Outer, Inner, HashQuals, Qual}` joins on equalities.
`Inner` is a `plan.Hash` over the inner path, the node PostgreSQL shows
under the join, and every hash qual is an `OpExpr` with `=` whose left
operand refers only to outer columns and whose right operand only to
inner columns; the planner rewrites `u.a = t.a` as `t.a = u.a` when `t`
is the outer side.

On the first `Next` the join asks its `Hash` node to read the whole
inner side into a table keyed by the hash of the right-hand values,
each row appended to its bucket in input order. The hash is FNV-1a
(`hash/fnv`, 64 bits) over the values' bytes: an `int32` as 4 bytes
little-endian, an `int64` as 8, a `bool` as one byte, text as its bytes
followed by a zero byte; with several quals the values are hashed one
after the other. A row with a NULL in any key is not stored, and an
outer row with a NULL key probes nothing, since NULL equals nothing.
For each outer row the join hashes the left-hand values, takes the
bucket, and for each row in it builds the joined row, evaluates the
hash quals (two values can share a hash) and then `Qual`, and yields
the row when both are TRUE. Rows come out in outer order, then bucket
order. The `Hash` node yields nothing through `Next`; `Build` panics on
a `HashJoin` whose `Inner` is not a `Hash`.

Worked example. `t(a int4 PRIMARY KEY, b text)` holding `(1,'x')`,
`(2,'y')`, `(3,'z')`, `(4,NULL)` and `u(a int4, c int8, d text)` holding
`(1,10,'p')`, `(1,11,'q')`, `(3,30,'r')`, `(NULL,40,'s')`, `(5,50,'t')`.
The planner hashes `t` and probes with `u`:

```
 Hash Join
   Hash Cond: (u.a = t.a)
   ->  Seq Scan on u
   ->  Hash
         ->  Seq Scan on t
```

The two phases, on that data:

```
build: the whole inner side, before a single row comes out

  Seq Scan on t ──► Hash ──►  h(1) │ (1,'x')
       4 rows                 h(2) │ (2,'y')
                              h(3) │ (3,'z')
                              h(4) │ (4,NULL)      a row with a NULL
                                                   key would be dropped

probe: one outer row at a time, in outer order

  Seq Scan on u ──► hash(u.a) ──► bucket ──► compare keys ──► joined row
     (1,10,'p')        h(1)       (1,'x')      1 = 1          1 | x | 10
     (1,11,'q')        h(1)       (1,'x')      1 = 1          1 | x | 11
     (3,30,'r')        h(3)       (3,'z')      3 = 3          3 | z | 30
     (NULL,40,'s')     none       –            –              nothing
     (5,50,'t')        h(5)       empty        –              nothing
```

Build: every row of `t` is stored under the hash of its `a`, so the
table holds keys 1, 2, 3 and 4. The row `(4, NULL)` is stored, because
its *key* is 4; the NULL is in `b`, which is not a key column.

Probe, in `u`'s order:

| Outer row | Key | Bucket | Result |
|---|---|---|---|
| `(1,10,'p')` | 1 | holds `(1,'x')` | joined row |
| `(1,11,'q')` | 1 | holds `(1,'x')` | joined row |
| `(3,30,'r')` | 3 | holds `(3,'z')` | joined row |
| `(NULL,40,'s')` | NULL | not probed | no rows |
| `(5,50,'t')` | 5 | empty | no rows |

Three rows out, in outer order:

```
 a | b | c  
---+---+----
 1 | x | 10
 1 | x | 11
 3 | z | 30
```

The NULL key is the case to get right. It is not stored on the build
side and it probes nothing on the outer side, because `NULL = NULL` is
`NULL` and never `TRUE`. Row `(5,50,'t')` shows the other half: a bucket
that is empty, or that holds only rows whose key merely collides, yields
nothing, which is why the hash quals are re-evaluated on every candidate
pair rather than trusted from the bucket alone.

## Where the quals go

`WHERE` and every `ON` are one AND list after the analyzer. The planner
splits it into conjuncts and computes for each the set of range table
entries it references.

- A conjunct over one relation is a restriction qual of that relation:
  it goes into its scan, as an index qual or a `Filter`, as in chapter
  14. A conjunct without any column (`WHERE true`) is a restriction
  qual of range entry 0.
- A conjunct over several relations is a join qual. It is applied at
  the first join whose relation set contains all of them: the join of
  sets `L` and `R` takes the conjuncts within `L ∪ R` that are neither
  within `L` nor within `R`.
- Among a join's quals, an equality whose one side references only `L`
  and other side only `R` is a hash qual, written with the outer side
  on the left. The rest is the `Join Filter`, and everything is the
  `Join Filter` of a nested loop, minus the qual a parameterised inner
  scan enforces as its `Index Cond`.

## Paths and the search

A path is one way to produce the rows of a set of relations: the plan
node, whether its rows come out in the `ORDER BY` order, and the
relations whose current row it needs from a nested loop above it (its
parameters; empty for an ordinary path). The planner keeps a list of
paths per relation set, as `RelOptInfo.pathlist` does, and adds to it
with a reduced `add_path`: a new path is dropped when a kept path is at
least as good on every count, startup cost, total cost, order (an
ordered path is better than an unordered one), and parameters (fewer is
better, by set inclusion); otherwise it is kept and the kept paths it
beats that way are dropped. Ties keep the older path.

Single relations get their chapter 14 paths, the sequential scan first,
then per index the scan with the restriction quals it can use or the
`ORDER BY` order, and now also one parameterised path per join qual an
index can take: the qual is a comparison (`=`, `<`, `<=`, `>`, `>=`)
between the indexed column alone and an expression without that
relation's columns, rewritten with the column on the left, and the path
carries the restriction index quals too; its parameters are the other
relations the qual references. `ORDER BY` order counts only for the
outer role, so a parameterised path is never ordered.

Sets of two or more relations are built in order of size, each set from
every split into an outer set `L` and an inner set `R`, tried in
increasing order of `L` as a bitmask, for every kept outer path without
parameters and every kept inner path:

- an inner path without parameters gives a nested loop over its
  `Materialize`, and, when the join has a hash qual, a hash join;
- an inner path whose parameters are within `L` gives a nested loop
  over it, with the join quals it enforces left out of the join filter.

A nested loop keeps its outer path's order (its rows are the outer rows
in order, each followed by its matches); a hash join has no order, as
in PostgreSQL. Join paths are never parameterised, so a parameterised
scan only ever sits directly under a nested loop whose outer side
covers its parameters. The estimated row count of a set does not depend
on the path (below). At the top, every path of the full set is finished
as in chapter 14, `Sort` unless ordered, `Project`, `Limit`, and the
lowest total cost wins, the earliest on a tie. `UPDATE` and `DELETE`
still read one relation through its cheapest scan.

## The cost model

The chapter 14 parameters stay. One constant joins them:
`DefaultNumDistinct` = 200 (`DEFAULT_NUM_DISTINCT`), and with it an
estimate of the distinct values of an expression, `nd`
(`get_variable_numdistinct`): for a column with a unique index, the
table's tuples; for a boolean column, 2; otherwise the tuples of the
expression's one table when they are below 200, else 200, the guess.
Tuples are the chapter 14 estimate, 1201 for a never-analysed
`(int4, text)` table. An expression over several tables is a guess of
200.

Selectivity gains the join cases (`clauselist_selectivity` with a join
clause):

| qual | at a join | as a parameterised scan's index qual |
|---|---|---|
| `x = y`, columns of two relations | `1 / max(nd(x), nd(y))` | `1 / tuples` when the scanned column has a unique index, else `DefaultEqSel` |
| `x <> y` | 1 minus the above | 1 minus the above |
| `<`, `<=`, `>`, `>=` between relations | `DefaultIneqSel` | `DefaultIneqSel` |

The chapter 14 rules hold for everything else, ranges included, so an
equality between two columns of the same relation is still
`DefaultEqSel`. The rows of a set of relations are the product of its
single relations' rows (after their restriction quals) and the
selectivities of every join qual within the set, taken as one list,
rounded and at least 1 (`calc_joinrel_size_estimate`, which PostgreSQL
computes pair by pair; the product is the same up to rounding).

Node estimates, with `o` the outer path's estimate, `i` the inner's,
and `ops(q)` the operator count of chapter 14:

- Materialize (`cost_material`): startup `i.startup`; total `i.total +
  2 * 0.0025 * i.rows`; rows and width of the input.
- Nested Loop (`initial_cost_nestloop`, `final_cost_nestloop`,
  `cost_rescan`): the inner side is run once in full and rescanned
  `o.rows - 1` times; a rescan of a `Materialize` costs 0 to start and
  `0.0025 * i.rows` to run, a rescan of anything else costs the same as
  the first run. Startup `o.startup + i.startup`; total = startup +
  `(o.total - o.startup) + (i.total - i.startup) + (o.rows - 1) *
  rescan + o.rows * i.rows * (0.01 + 0.0025 * ops(join filter))`; rows
  the set's; width `o.width + i.width`.
- Hash (`make_hash`): startup and total both `i.total`, rows and width
  the input's.
- Hash Join (`initial_cost_hashjoin`, `final_cost_hashjoin`,
  `ExecChooseHashTableSize`, `estimate_hash_bucket_stats`), with `n`
  hash quals:
  - startup `o.startup + i.total + (0.0025 * n + 0.01) * i.rows`;
  - buckets `b` = the smallest power of two at least `max(1024,
    ceil(i.rows))`;
  - bucket size: per hash qual, from the inner side's expression: 0.1
    when `nd` is a guess; else `nd` scaled by the fraction of its
    table's tuples the relation's own quals keep, rounded and at least
    1, and the size is `1 / min(nd, b)`, clamped to `[1e-6, 1]`; the
    smallest over the quals is `s`;
  - run `(o.total - o.startup) + 0.0025 * n * o.rows + 0.0025 *
    ops(hash quals) * o.rows * clamp(i.rows * s) * 0.5 + m * (0.01 +
    0.0025 * ops(join filter))` where `m = clamp(o.rows * i.rows *
    sel(hash quals))` is the pairs the hash quals pass;
  - total = startup + run; rows the set's; width `o.width + i.width`.

Worked example. The two tables above, first with no statistics at all:

```
 Hash Join  (cost=37.02..72.55 rows=1075 width=80)
   Hash Cond: (u.a = t.a)
   ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)
   ->  Hash  (cost=22.01..22.01 rows=1201 width=36)
         ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
```

Neither table has been analysed, so `t` is guessed at 1201 rows and `u`,
being wider, at 1075. `t.a` has a unique index, so its distinct count is
its row count and the join selectivity is `1 / max(1201, 1075) =
1/1201`, giving `1201 * 1075 / 1201 = 1075` output rows. The `Hash`
node's total is its input's total, and the join's startup is the outer
scan's startup plus the whole inner subtree plus the build cost.

Now `ANALYZE`, and the same query:

```
 Hash Join  (cost=1.09..2.20 rows=4 width=80)
   Hash Cond: (u.a = t.a)
   ->  Seq Scan on u  (cost=0.00..1.05 rows=5 width=44)
   ->  Hash  (cost=1.04..1.04 rows=4 width=36)
         ->  Seq Scan on t  (cost=0.00..1.04 rows=4 width=36)
```

Four rows in `t`, five in `u`, and `1 / max(4, 5) = 1/5` gives 4 rows
out. The actual answer is three, which is what a default selectivity
buys you. The shape did not change here, but it does when a join qual is
not an equality:

```
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a < u.a;
 Nested Loop
   Join Filter: (t.a < u.a)
   ->  Seq Scan on u
   ->  Materialize
         ->  Seq Scan on t
```

No equality means no hash qual, so the only join method left is a nested
loop, and the inner side is materialised so its pages are read once
rather than once per outer row.

A worked case from `ch15_plan.sql`: `t` analysed with 2 rows, `u` never
analysed (2269 rows of `(int4, int4)`) with a primary key on `a`. The
parameterised scan of `u_pkey` costs `0.15..8.17` for one row (descent
0.155, one index page and entry 4.0075, one heap page and tuple 4.01),
so the nested loop is `0.155 + 1.02 + 8.0175 + 1 * 8.1725 + 2 * 1 *
0.01 = 17.39` for 2 rows, against a hash join of all of `u` at 26 and
more. Without `ANALYZE`, `t` is 1201 rows and the same loop costs over
9000.

## EXPLAIN

The join nodes print as PostgreSQL does, with the inner join's type
folded into the name:

```
Hash Join  (cost=34.19..285.88 rows=6455 width=80)
  Hash Cond: (t.a = u.a)
  Join Filter: (t.b < u.b)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Hash  (cost=20.75..20.75 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)
```

`Nested Loop` has a `Join Filter:` line when it has a qual and none
otherwise; `Hash Join` has `Hash Cond:` (the hash quals ANDed) and then
`Join Filter:` when there is one; `Hash` and `Materialize` are plain
headlines with one child. Children print outer first. A parameterised
index scan prints its `Index Cond:` with the outer column on the right.

## API

`internal/sql/query`

```go
type RangeEntry struct { Index int; Alias string; Rel *catalog.RelationInfo }
```

`internal/executor/expr`

```go
type Layout []int                             // keyed by RangeEntry.Index
func NewLayout(rng []*query.RangeEntry) Layout
```

`internal/plan`

```go
type NestLoop struct { Outer, Inner Node; Qual query.Expr; Est Estimate }
type HashJoin struct { Outer, Inner Node; HashQuals []query.Expr; Qual query.Expr; Est Estimate }
type Hash struct { Input Node; Est Estimate }
type Materialize struct { Input Node; Est Estimate }
// each with Range, Estimate, and node; IndexScan.Quals may refer to other relations
```

`internal/executor`

```go
type Node interface { Open() error; Next() (Row, bool, error); Rescan() error; Close() error }
```

`internal/planner`

```go
const DefaultNumDistinct = 200
const MaxJoinRelations = 12
var ErrTooManyRelations error
func Plan(q query.Stmt) (plan.Node, error) // ErrJoin is gone
```

Semantics the tests depend on:

- Analyzer: `Range[i].Index == i`.
- A `FROM` list longer than `MaxJoinRelations` is `ErrTooManyRelations`,
  refused before the search runs.
- `NewLayout` lays entries out in the order given, at the positions of
  their `Index`, `-1` for absent ones; `NewLayout(nil)` is `Layout{0}`;
  the chapter 10 `TestLayout` is unchanged.
- `Range()` of a join is outer then inner in a fresh slice; of `Hash`
  and `Materialize` the input's.
- `Explain` as in the section above, `Explain` tests hand-built trees
  with a `Filter` under a `Hash` and a `Sort` above a join.
- `NestLoop` yields the cross product in outer order, each outer row
  followed by the inner rows in the inner's order, with or without a
  `Materialize`; `Qual` must be TRUE; empty sides give no rows; rows
  have a zero TID; a qual error is returned as is.
- A parameterised `IndexScan` (bounds with the outer relation's
  columns, any of the five operators, expressions allowed, several
  bounds tighten) returns the same rows as the loop with those bounds
  as `Join Filter`; a NULL bound matches nothing; the inner may be a
  `Filter` over the scan.
- `HashJoin` returns each outer row with every matching inner row,
  duplicates included, NULL keys never, with several quals all of them,
  keys through `Cast`, text and bool keys; a `Join Filter` applies
  after the match; `Build` panics without a `Hash`.
- `Rescan` on every node kind, whether drained or stopped after one
  row, yields the same rows again.
- Planner, shapes (`TestJoinGolden`): a cross join is `Nested Loop`
  over `Materialize`; an equality is a `Hash Join` with the hash cond
  outer-left; any other join qual, or an `OR`, is a `Join Filter`;
  single-relation quals sit under the scans; a `WHERE true` is a
  `Filter: TRUE` on the first relation; `small` joined to `big` on
  `big`'s unique column is a nested loop probing `big_c_key` with the
  join qual as `Index Cond`, and on the non-unique `big.a` a hash join
  with `small` hashed; a chain of three uses two parameterised scans
  with the qual over the ends as the top `Join Filter`; a qual over
  three relations waits for the top; four relations plan; `ORDER BY`
  sorts above a hash join and is absorbed by an ordered outer index
  scan under a `LIMIT`; `LIMIT 1` over an equality join prefers the
  nested loop with a `Join Filter` for its zero startup.
- Planner, numbers (`TestJoinEstimates`) follow the formulas above
  over chapter 14's tables: `t ⋈ u` on `a` is `34.19..285.88` for 6455
  rows, the cross join `16183.89` for 1291075, `small ⋈ big` on `big.c`
  `0.17..821.75` for 100, its inner scan `0.17..8.19` for 1.
- Planner, structure (`TestJoinShape`): `HashJoin.Inner` is a `Hash`,
  `NestLoop.Inner` a `Materialize`, an `IndexScan`, or a `Filter` over
  one; the parameter is a `Var` of the outer relation; the join's
  `Range()` may run against range table order; `Hash`'s startup equals
  its total; every node has an estimate.
- Session: `SELECT *` prints the range table's columns in its order
  whatever the join order, duplicate names as they are; the tag counts
  joined rows; the errors are the analyzer's (`ErrAmbiguousColumn`,
  `ErrTypeMismatch` for `ON`, `ErrDuplicateAlias`,
  `ErrUndefinedTable`); `TestJoinPlans` sees the hash join of two
  never-analysed tables become the nested loop with `Index Scan using
  u_pkey` once `t` is analysed with two rows (`0.15..17.39 rows=2`),
  and a hash join again when both are analysed and tiny;
  `TestJoinOracle` compares five predicates over random rows with the
  cross product filtered in Go, and the filtered join after `ANALYZE t`
  plans as a nested loop over `u_pkey`.
- Regress: `ch15_join.out`, `ch15_plan.out`, and `ch11_errors.out`
  without its join line.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** The hash table: what a bucket is, where the rows live,
and how a probe walks a bucket. The tests pin the output order (outer rows
in order, matches within one bucket in input order), the treatment of a
NULL key, and the plan shape; the map from hash to rows is yours.

**Go you will need.** `hash/fnv` (`fnv.New64a`, `Write`, `Sum64`),
`encoding/binary` (`LittleEndian.PutUint32/64`), `math/bits`
(`OnesCount64`, `TrailingZeros64`, `Len64` for the next power of two),
bit operations on a `uint64` set (`&`, `|`, `&^`, `1 << i`), a map
keyed by that set, and `append` onto a nil slice to copy without
aliasing.

<details><summary><b>Layout.</b></summary>

`NewLayout` first finds the highest `Index` to size the slice, fills it
with `-1`, then walks the entries in order assigning offsets; the last
element is the running total.
</details>

<details><summary><b>Executor, parameters.</b></summary>

A private `outerRow{layout, row}` shared between a nested loop and the
nodes under its inner side: `Build` becomes a wrapper around a `build(p,
env, outer *outerRow)` that passes the loop's own `outerRow` down its inner
side and whatever it received down the outer side. The index scan compiles
its right-hand sides in `Open` against `outer.layout` (or
`NewLayout(nil)`), keeps `scan == nil` until the first `Next` calls a
`start` that evaluates them on `outer.row` and opens the tree scan;
`Rescan` closes the scan and sets it back to nil.
</details>

<details><summary><b>Executor, nested loop.</b></summary>

State: the current outer row and whether there is one. `Next` loops: with
no outer row, pull one (done when the outer is done), store it in the
shared `outerRow`, `Rescan` the inner; pull an inner row, and when the
inner is dry drop the outer row and loop; join, test the qual, loop or
return. A `joinRows(outer, inner)` helper concatenates values and null
flags into fresh slices.
</details>

<details><summary><b>Executor, hash join.</b></summary>

`hashKeys(funcs, row) (uint64, null, error)` does the hashing for both
sides. The `Hash` node's private `build(keys)` pulls its input into a
`map[uint64][]Row`. The join compiles the outer keys against the outer
layout, the inner keys against `Inner.Range()`, and the hash quals and
`Qual` ANDed into one `Func` against the join's layout; `Next` builds the
table on first call (only then, so an empty join is cheap to `Open`), then
walks outer rows and buckets as the nested loop walks outer and inner.
</details>

<details><summary><b>Planner, sets.</b></summary>

A `relSet uint64` with `single(i)`, `has`, `subsetOf`, `size`;
`exprRels(e)` collects the relations of an expression and replaces chapter
14's `hasVar`. Sixty-four relations is the type's ceiling and
`MaxJoinRelations` the planner's, checked in `Plan` before any search
starts. The planner state is a struct holding the range table,
`ORDER BY`, the conjuncts with their sets, a `map[relSet]float64` of
single-relation row counts, and a `map[relSet][]path`. `cheapestScan` for
UPDATE and DELETE builds one with a one-entry range table and takes the
cheapest base path.
</details>

<details><summary><b>Planner, base paths.</b></summary>

As chapter 14's `scanPaths`, with `isIndexVar` and `indexQual` taking the
range entry so that `v.Rel == rel.Index` and the other side is checked with
`exprRels(other).has(rel.Index)` instead of `hasVar`; then, per index, one
more path per join qual that `indexQual` accepts, with `params = qrels &^
single(i)` and the qual remembered in the path (`enforced`) so that the
join can leave it out of its filter. Store the sequential scan's row count
as the relation's rows.
</details>

<details><summary><b>Planner, search.</b></summary>

`search` fills level 1, then for each size and each set of that size calls
`joinPaths(s)`; `joinPaths` enumerates `outer` from 1 up to `s - 1`,
skipping non-subsets, and `inner = s &^ outer`; the join quals are the
conjuncts whose set is within `s` but not within either side. `addPath` and
`dominates` implement the rule above; `dominates(a, b)` is `a.startup <=
b.startup && a.total <= b.total && (a.ordered || !b.ordered) && a.params ⊆
b.params`. `splitHashQuals(joinQuals, outer, inner)` picks and orients the
equalities. `joinRows(s)` multiplies the single-relation rows of `s` and
the selectivity of the join quals within `s`.
</details>

<details><summary><b>Planner, selectivity.</b></summary>

`selectivity(quals, rel)` takes the relation whose scan the quals belong
to, or -1 at a join; `eqSel(e, rel)` and `joinEqSel(e)` implement the
table; `ndistinct(e) (nd, guess)` the distinct count; `bucketSize(key,
buckets)` the hash bucket fraction; `nextPow2`. `rangeBound` tests
`exprRels(other) == 0` where it tested `!hasVar`.
</details>

<details><summary><b>Planner, join costs.</b></summary>

`materialize(node)`, `nestLoop(s, outer, inner path, quals)` (detect the
`Materialize` by type to pick the rescan cost), `hashJoin(s, outer, inner
path, hashQuals, rest)`, each returning a `path` whose `ordered` is the
outer's for the loop and `len(orderBy) == 0` for the hash join. `plan.Hash`
is built inside `hashJoin` with the inner's total as both costs.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch15` at the end.
Every test not named in that step or an earlier one still panics.

1. Range positions. `RangeEntry.Index` in `addRel`, `NewLayout` keyed
   by it. Green: analyzer `TestRangeIndex`; expr `TestLayout`,
   `TestLayoutJoinOrder`.

   ```sh
   go test -race ./internal/sql/analyzer/... ./internal/executor/expr/... -run 'TestRangeIndex|TestLayout$|TestLayoutJoinOrder'
   ```
2. Plan nodes. `Explain` for the four new nodes and the parameterised
   `Index Cond`. Green: plan `TestJoinRange`, `TestExplainJoins`.

   ```sh
   go test -race ./internal/plan/... -run 'TestJoinRange|TestExplainJoins'
   ```
3. Rescan. `Rescan` on every existing node, `Materialize`, the
   parameterised `IndexScan` with `outerRow`, `Build` as a wrapper.
   Green: executor `TestRescan` (all but the three join cases).

   ```sh
   go test -race ./internal/executor/... -run 'TestRescan'
   ```
4. Nested loop. `nestLoop`, `joinRows`. Green: `TestNestLoop`,
   `TestNestLoopParam`, and the loop cases of `TestRescan`.

   ```sh
   go test -race ./internal/executor/... -run 'TestNestLoop|TestNestLoopParam|TestRescan'
   ```
5. Hash join. `hashKeys`, `hash.build`, `hashJoin`. Green:
   `TestHashJoin`, the rest of `TestRescan`, `TestJoinOracle`.

   ```sh
   go test -race ./internal/executor/... -run 'TestHashJoin|TestRescan|TestJoinOracle'
   ```
6. Planner. `relSet`, `exprRels`, the planner struct, `basePaths` with
   parameterised paths, `addPath`, `joinPaths`, the join costs and
   selectivities, `search`, `Plan` without `ErrJoin` and with the
   `MaxJoinRelations` check. Green: `TestJoinGolden`,
   `TestJoinEstimates`, `TestJoinShape`, `TestTooManyRelations`, and the
   chapter 11 and 14 planner tests unchanged apart from `TestPlanErrors`.

   ```sh
   go test -race ./internal/planner/... -run 'TestJoinGolden|TestJoinEstimates|TestJoinShape|TestTooManyRelations|TestPlanGolden|TestPlanShape|TestEstimates|TestScanChoice|TestIndexScanShape'
   ```
7. Session. Nothing to write: `run` already plans and runs any `Select`.
   Green: session `TestJoins`, `TestJoinPlans`, `TestJoinOracle`, and
   `TestRegress` with `ch15_join.sql`, `ch15_plan.sql`, and the changed
   `ch11_errors.sql`.

   ```sh
   REGRESS_CHAPTER=15 go test -race ./internal/session/... ./internal/regress/... -run 'TestJoins|TestJoinPlans|TestJoinOracle|TestRegress'
   ```

### When a test fails

- expr `TestLayoutJoinOrder` — the layout is right for `[t, u]` and
  wrong for `[u, t]`. Element `i` of the layout belongs to the entry
  whose `Index` is `i`, not to the `i`th entry in the slice; the columns
  themselves sit in the order the entries were given.
- expr `TestLayoutJoinOrder` — an entry the row does not hold gets slot
  0. It gets -1, so a `Var` that should be unreachable fails loudly
  rather than reading another relation's column.
- executor `TestRescan` — a `Sort` returns nothing on the second pass,
  or a `Limit` returns nothing after the first. `Rescan` restores every
  node's per-run state: a `Sort` drops its rows and rescans its input, a
  `Limit` restores its count.
- executor `TestRescan` — a `Materialize` reads its input twice. It
  stores the rows on the first pass and replays them; only that makes
  it worth putting under a nested loop.
- executor `TestRescan` — a `HashJoin` rebuilds its table on every
  rescan. It rescans the outer side and keeps the table, which is the
  whole reason `Rescan` exists rather than `Close` plus `Open`.
- executor `TestNestLoopParam` — the parameterised inner scan uses the
  previous outer row's values, or the first outer row's. The bounds are
  evaluated on the first `Next` after `Open` or `Rescan`, which is after
  the loop has stored the current outer row; evaluating them in `Open`
  is one row too early.
- executor `TestNestLoop` — the inner side is exhausted after the first
  outer row. `Rescan` it before each outer row, and pull it dry for
  each.
- executor `TestHashJoin` — a row with a NULL key joins with something.
  A NULL key is not stored on the build side and probes nothing on the
  outer side; `NULL = NULL` is `NULL`, never `TRUE`.
- executor `TestHashJoin` — rows come out in bucket order rather than
  outer order. The outer side drives the loop; within one outer row the
  matches come out in the bucket's order, which is the inner side's
  input order.
- executor `TestHashJoin` — two keys that share a hash join each other.
  The hash narrows the candidates; the hash quals still have to be
  evaluated on every pair in the bucket.
- executor `TestJoinOracle` — passes on small inputs and fails on one
  random case. Compare against the nested-loop result for the same
  inputs; the usual causes are the NULL rule and the hash of a type
  whose bytes are not what the spec says (`int32` as 4 bytes
  little-endian, text as its bytes then a zero byte).
- planner `TestJoinShape` — a join qual is applied twice, or not at all.
  A conjunct goes to the *first* join whose relation set contains all
  its relations and which is not entirely within either side; a
  parameterised inner scan that enforces a qual as its `Index Cond`
  takes it out of the join filter.
- planner `TestJoinShape` — a parameterised path is chosen as the outer
  side of a join, or as the top of a plan. Only paths without parameters
  can be outer, and a parameterised scan sits directly under a nested
  loop whose outer side covers its parameters.
- planner `TestJoinEstimates` — the row count of a three-way join
  differs depending on the join order. It must not: the rows of a set
  are the product of its relations' rows and every join qual within the
  set, computed once per set, not per path.
- planner `TestJoinEstimates` — an equality between two columns of one
  relation is estimated as a join. A join selectivity applies only when
  the two sides reference different relations; within one relation it is
  still `DefaultEqSel`.
- planner `TestJoinGolden` — the hash qual is written with the inner
  side on the left. It is rewritten so the outer side is on the left,
  which is what the executor's build and probe expect.
- planner `TestJoinGolden` — a cheaper path is dropped. `addPath` drops
  a new path only when a kept one is at least as good on startup, total,
  order *and* parameters; a path with fewer parameters can be worth
  keeping at a higher cost.
- `TestRegress` — `ch11_errors.out` differs. It changed in this chapter,
  because a query the planner used to refuse with `ErrJoin` now plans.

## Why a default distinct count

A join's row estimate turns on how many distinct values the key has, and a
single constant would make every join look alike, so this chapter takes
PostgreSQL's `eqjoinsel` fallback: as many distinct values as rows when a
unique index says so or the table is smaller than 200, and 200 otherwise
(D22). Real counts come from the sampling pass that histograms need, and the
fallback already separates the two cases that matter: a primary-key join
estimates one match per outer row, and two large unanalysed tables one in
200. Where it goes wrong is a large table with few distinct values, which we
estimate as 200 and which will pick the wrong side to hash.

## Out of scope

Outer, semi, and anti joins (`JOIN_LEFT` and the rest of `JoinType`);
merge join and the sorted-input paths it needs; parameterised join
paths and outer paths, so a parameterised scan always sits directly
under its nested loop; bushy plans whose inner side is a parameterised
join; hash tables that spill to disk (batches, `work_mem`), so every
hash join and `Materialize` is costed in memory; the `Inner Unique`
optimisation; join removal; `from_collapse_limit` and GEQO, so the
search is exhaustive up to `MaxJoinRelations` and refuses anything
longer rather than searching it differently, and the tests stay at four
relations; real
distinct counts and most-common-value lists for join selectivity
(D22); `EXPLAIN ANALYZE`'s row counts and `Rows Removed by Join
Filter`.

## Check your understanding

1. A join puts `u` (2 columns, range index 1) outside and `t` (3
   columns, range index 0) inside. What does `NewLayout` return, and
   which slot does `t.b` read?
   <details><summary>Answer</summary>

   `[2 0 5]`: element 0 is `t`'s starting slot, which is 2, element 1 is
   `u`'s, which is 0, and the last element is the width. `t.b` is
   `Var{Rel: 0, Attr: 1}`, so slot `2 + 1 = 3`. The layout is keyed by
   the range-table index and filled from the order the entries were
   given, which is what keeps a `Project` above the join independent of
   which side was outer.
   </details>
2. `t` holds keys 1, 2, 3, 4 and `u` holds 1, 1, 3, NULL, 5, with `t`
   hashed. How many rows come out, and what happens to each `u` row?
   <details><summary>Answer</summary>

   Three. `(1,...)` twice and `(3,...)` each find their bucket and join;
   the NULL row probes nothing, because `NULL = NULL` is never `TRUE`;
   and `5` finds an empty bucket. `t`'s row with key 4 is in the table
   and is simply never probed. Note a row whose *non-key* column is NULL
   is stored and joined normally.
   </details>
3. Why is the inner side of a nested loop wrapped in `Materialize` when
   it does not depend on the outer row?
   <details><summary>Answer</summary>

   Because it is read once per outer row. Without it, a sequential scan
   under the loop re-reads every page of the inner table for every outer
   row, at a page cost each time; with it, the rows are stored on the
   first pass and every later pass costs `0.0025` per row and no I/O.
   That is exactly what `cost_rescan` charges, and it is why a
   parameterised index scan is *not* materialised: its rows differ for
   each outer row.
   </details>
4. Why does the planner keep a list of paths per relation set rather than
   the single cheapest path?
   <details><summary>Answer</summary>

   Because "cheapest" depends on what sits above. A path with a higher
   total but rows already in `ORDER BY` order can win once a `Sort` is
   avoided; a path with a lower startup wins under a small `LIMIT`; and
   a parameterised path is useless on its own but is the only way to get
   an index-driven nested loop. `add_path` keeps a path unless another
   is at least as good on every one of those counts, which is why the
   comparison is four-dimensional rather than one number.
   </details>
5. We have only inner joins, hash and nested loop, with no merge join
   and no outer joins. What does each omission cost?
   <details><summary>Answer</summary>

   A merge join wins when both inputs are already sorted on the join key,
   which an index scan can deliver for free; without it, a join over two
   indexed columns must still build a hash table. Outer joins are not a
   missing method but a missing semantics: they cannot be flattened into
   a range table and a conjunction (chapter 09 relies on that), so
   supporting them means keeping the join tree through the analyzer and
   the planner, tracking which quals may be pushed below which join, and
   teaching every join node to emit null-extended rows. That is why
   PostgreSQL's `deconstruct_jointree` exists at all, and it is the
   single largest thing this book leaves out of the planner.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Merge join.** Sort both inputs on the key and walk them in step, with
   the mark-and-restore that duplicates on the inner side need. Then give it
   a cost that can reuse an index's ordering, which is where merge join
   earns its place.
2. **Outer joins.** Add `LEFT JOIN` for the nested loop only: a flag on the
   node, a null-extended row when the inner side yields nothing, and a
   planner that may no longer reorder the two sides freely. The last part is
   the reason `JoinType` exists.
3. **Read `join_search_one_level` and the GEQO threshold in `allpaths.c`.**
   Our search is exhaustive and our tests stop at four relations. Work out
   at how many relations the exhaustive search stops being affordable, and
   what `from_collapse_limit` protects.
