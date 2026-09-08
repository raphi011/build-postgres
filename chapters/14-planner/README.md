# Chapter 14: Planner and EXPLAIN

**Goal.** A cost model with PostgreSQL's parameters, `ANALYZE` to feed it
page and tuple counts, a planner that costs a sequential scan and every
usable index scan as whole plans and keeps the cheapest, an index scan
that replaces `ORDER BY`'s sort, and `EXPLAIN` output with costs and row
estimates in PostgreSQL's format.

**You edit.** `internal/planner/planner.go`: 1 function with a
`panic("not implemented")` body, `Plan`, now with a cost model behind
it. `internal/plan/plan.go`: 2, `Estimate.String` and `ExplainCosts`.
`internal/catalog/catalog.go`: 1, `UpdateStats`. `internal/sql/ast/ast.go`
and `internal/sql/query/query.go`: 1 each, `Analyze.String`. And your own
bodies from earlier chapters: `lexer.keywords` gains `analyze`;
`parseStmt` learns `ANALYZE [table]` and `EXPLAIN (COSTS OFF)`;
`ast.Explain.String` and `query.Explain.String` print `(COSTS OFF)`;
`Analyze` binds the two; `catalog.loadIndexes` fills the new
`IndexInfo.Pages` and `Tuples`; `session.run` runs `ANALYZE` and prints
`EXPLAIN` with costs. The tests of every package above are the contract;
`ch11_explain.sql` now says `EXPLAIN (COSTS OFF)`.

**Needs from earlier chapters.** `internal/plan` and the executor's
`IndexScan` (chapter 13), `internal/catalog` (`RelationInfo.Pages`,
`Tuples`, `Indexes`), `internal/heap` (`NBlocks`, `Scan`, `Update`),
`internal/page` (`PageSize`, `HeaderSize`), and `math`. No earlier
package was stubbed for this chapter.

**Done when.** `make test-ch14` passes.

**Effort.** Long, about 6 hours. The cost model (step 5) is the hard part;
the golden `EXPLAIN` output compares costs to two decimals, so every
constant and every rounding has to match.

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

Chapter 13 gave the executor an index scan and left the planner
choosing sequential scans. Choosing well needs two things PostgreSQL's
optimizer is built around: a cost model that turns a plan into a
number, and statistics about the tables so that the numbers mean
something. This chapter adds both in the shape PostgreSQL uses:
`ANALYZE` writes `relpages` and `reltuples` into `pg_class`, the planner
estimates every node's cost and row count from them with the constants
of `costsize.c`, and `EXPLAIN` shows the estimates. What it leaves out is
the per-column statistics of `pg_statistic`: every predicate gets the
default selectivity PostgreSQL falls back on when it has no histogram,
which is enough to see the planner change its mind as a table grows
(D21).

PostgreSQL source: `optimizer/README` (read the sections on paths and
costs), `optimizer/path/costsize.c` (`cost_seqscan`, `cost_index`,
`cost_sort`, `index_pages_fetched`), `optimizer/path/indxpath.c`
(`match_clause_to_indexcol`), `optimizer/path/clausesel.c`
(`clauselist_selectivity`), `utils/adt/selfuncs.c` (`genericcostestimate`,
`btcostestimate`, the `DEFAULT_*_SEL` constants in `selfuncs.h`),
`optimizer/util/plancat.c` (`estimate_rel_size`),
`optimizer/util/pathnode.c` (`adjust_limit_rows_costs`),
`commands/analyze.c` (`do_analyze_rel`), `commands/vacuum.c`
(`vac_update_relstats`), and `commands/explain.c`.

## Statistics

`ANALYZE [table]` counts a table's pages (`heap.NBlocks`) and live tuples
(a heap scan) and stores them in its `pg_class` row through
`catalog.UpdateStats`, a `heap.Update` of the row: the old version gets
`xmax`, the new one goes to the end of the catalog, and the cache is
invalidated. The table's indexes get their own row updated too, with
the index file's block count and the same tuple count, and `IndexInfo`
carries them as `Pages` and `Tuples`. `ANALYZE` alone does every table,
the catalogs included. Naming an index is a no-op (PostgreSQL warns
and skips; there are no warnings here); naming an unknown relation is
`relation "nope" does not exist` without a position, since ANALYZE opens
the table as a utility command. PostgreSQL samples the table and
computes histograms as well; the counts are all this chapter keeps.

A table that was never analysed has `Pages == 0`. The planner then
assumes `DefaultPages` (10) pages of rows as wide as the columns say,
which is what `estimate_rel_size` does for a relation whose file is
still small: `(8192 - 24) / (24 + 4 + width rounded up to 8)` tuples per
page, rounded to the nearest integer, so a table `(a int4, b text)` is
1201 rows. An analysed empty table has `Pages == 0` too and gets the
same guess, as in PostgreSQL. An index without statistics counts as one
page holding the table's tuples.

Worked example. `CREATE TABLE t (a int4 PRIMARY KEY, b text)`, three
rows inserted, then `ANALYZE t`:

| Relation | Before | After |
|---|---|---|
| `t` | `relpages` 0, `reltuples` 0 | 1 page, 3 tuples |
| `t_pkey` | 0, 0 | 2 pages, 3 tuples |

Before the `ANALYZE` the planner works from `DefaultPages`: 10 pages of
`(a int4, b text)`, whose width is `4 + 32 = 36` rounded up to 40, so
`(8192 - 24) / (24 + 4 + 40) = 120.1` rows per page and 1201 rows in
total. That is the number every estimate below is built on, and it is
wrong by a factor of 400. Fixing it is the whole point of `ANALYZE`, and
the plans change accordingly.

## The cost model

Costs are in PostgreSQL's units: reading one page in sequence costs 1.
The parameters are the defaults of `costsize.c`:

| parameter | value | charged for |
|---|---|---|
| `SeqPageCost` | 1 | a page read by a sequential scan |
| `RandomPageCost` | 4 | a page read through an index |
| `CPUTupleCost` | 0.01 | a heap tuple produced |
| `CPUIndexTupleCost` | 0.005 | an index entry visited |
| `CPUOperatorCost` | 0.0025 | an operator evaluated |

Every width is the sum of the columns' sizes, 32 for `text` (PostgreSQL's
guess for a varlena without statistics). An operator is an `OpExpr`,
`Neg`, or `Cast`; `IS NULL` and `AND`/`OR`/`NOT` cost nothing, as in
`cost_qual_eval`. A row estimate is rounded to the nearest integer and
never below 1 (`clamp_row_est`).

The selectivity of a list of quals is the product of each qual's, with
`clauselist_selectivity`'s one refinement: inequalities on the same
column with a lower and an upper bound (`a > 1 AND a < 9`) count once as
a range.

| qual | selectivity |
|---|---|
| `x = y` | `DefaultEqSel` 0.005 |
| `x <> y` | 1 - 0.005 |
| one bound on a column (`a > 1`, `1 < a`, also two on the same side) | `DefaultIneqSel` 1/3 |
| a lower and an upper bound on a column | `DefaultRangeIneqSel` 0.005 |
| an inequality without a column on exactly one side | 1/3 |
| `x IS NULL` / `IS NOT NULL` | `DefaultUnkSel` 0.005 / 1 - 0.005 |
| a boolean column, or anything else | `DefaultBoolSel` 0.5 |
| `TRUE`, `FALSE` or `NULL` | 1, 0 |
| `NOT x` | 1 - s(x) |
| `x OR y` | s(x) + s(y) - s(x) s(y) |

Node estimates, where `pages` and `tuples` are the table's, `q` its
quals, `ops(e)` the operator count, `s(q)` the selectivity:

- Seq Scan: startup 0; total `pages * 1 + tuples * (0.01 + 0.0025 *
  ops(q))`; rows `tuples * s(q)`.
- Index Scan with index quals `iq` (selectivity `is`) and residual quals
  `rq`, over an index of `ipages` pages and `ituples` entries
  (`genericcostestimate`, `btcostestimate`, and `cost_index` without
  correlation):
  - descent: `50 * 0.0025`, plus `ceil(log2(ituples)) * 0.0025` when the
    index has more than one entry; this is the startup cost;
  - entries `n = is * ituples`; index pages `ceil(n * ipages / ituples)`
    when both are above 1, else 1; index cost `pages * 4 + n * (0.005 +
    0.0025 * ops(iq))`;
  - heap tuples fetched `f = clamp(tuples * is)`; heap pages by Mackert
    and Lohman for a table that fits in cache: `2 * T * f / (2 * T + f)`
    rounded up and at most `T = max(pages, 1)`; heap cost `pages * 4 + f
    * (0.01 + 0.0025 * ops(rq))`;
  - total = descent + index cost + heap cost; rows `tuples * is * s(rq)`.
- Sort (`cost_tuplesort`, in memory): `N = max(input rows, 2)`; startup
  `input total + 2 * 0.0025 * N * log2(N)`; total `startup + 0.0025 *
  N`; rows and width of the input.
- Limit (`adjust_limit_rows_costs`): `n` is the constant count, all the
  rows for NULL, a tenth of the input rows (rounded, at least 1) for
  any other expression, and at most the input rows; startup the
  input's; total `startup + (input total - startup) * n / input rows`;
  rows `max(n, 1)`.
- Result: startup 0, total 0.01, rows 1, the targets' width, even
  under a `One-Time Filter`.
- Values: total `N * (0.01 + 0.0025)`, rows `N`, the table's width.
- ModifyTable: the input's costs, rows 0, width 0.
- Project (invisible): the input's, plus `rows * 0.0025 * ops(targets)`
  on the total, and the targets' width. Filter (invisible): the scan's
  own estimate, which already includes the quals.

## Choosing a scan

**Predict.** A table with a primary key that has never been analysed.
`WHERE a = 1` and `WHERE a > 1`, same table, same index. Does the
planner pick the index for both, neither, or one of them?

For a query over one relation the planner builds every whole plan it
can and keeps the cheapest total cost, the sequential scan on a tie
(`set_plain_rel_pathlist`, `add_path` reduced to one relation):

1. `WHERE` is split into its ANDed conjuncts.
2. For each index of the table, the conjuncts an index scan can use are
   the comparisons (`=`, `<`, `<=`, `>`, `>=`) between the indexed
   column alone and an expression without columns
   (`match_clause_to_indexcol`); one with the column on the right is
   rewritten with it on the left and the operator mirrored, so that
   `5 < a` becomes `a > 5` for the executor's `IndexScan`. The rest
   becomes a `Filter` above the scan. An index with no usable qual is
   still a candidate when it delivers the `ORDER BY` order: one
   ascending key that is the indexed column.
3. Above each scan: `Sort` unless the scan is already ordered, `Project`,
   `Limit`. The totals compared are those of the whole plan, so a small
   `LIMIT` favours an index scan whose startup cost is small over a sort
   that must read everything first, the way PostgreSQL's fractional
   path comparison does.

Worked example. The same never-analysed table, two queries. For
`SELECT * FROM t WHERE a = 1`:

| Path | Startup | Total | Rows |
|---|---|---|---|
| Seq Scan, `Filter: (t.a = 1)` | 0.00 | 25.01 | 6 |
| Index Scan using `t_pkey`, `Index Cond: (t.a = 1)` | 0.15 | 24.26 | 6 |

The index scan wins by three quarters of a page read. Where the numbers
come from, with 10 pages, 1201 tuples, and an index counted as one page
of 1201 entries:

- Seq Scan: `10 * 1 + 1201 * (0.01 + 0.0025) = 25.0125`.
- Index descent: `50 * 0.0025 + ceil(log2(1201)) * 0.0025 = 0.125 +
  11 * 0.0025 = 0.1525`, the startup cost.
- Index side: selectivity 0.005 gives `n = 6.005` entries on one index
  page, so `1 * 4 + 6.005 * (0.005 + 0.0025) = 4.045`.
- Heap side: 6 tuples fetched, and Mackert and Lohman give
  `ceil(2 * 10 * 6 / (2 * 10 + 6)) = ceil(4.62) = 5` pages, so
  `5 * 4 + 6 * 0.01 = 20.06`.
- Total `0.1525 + 4.045 + 20.06 = 24.2575`.

Now `SELECT * FROM t WHERE a > 1`:

```
 Seq Scan on t  (cost=0.00..25.01 rows=400 width=36)
   Filter: (t.a > 1)
```

Same table, same index, sequential scan. One inequality has selectivity
1/3, so the index would visit 400 entries and fetch 400 tuples over
every page of the table at random-page prices; reading all ten pages in
sequence is far cheaper. The equality's 0.005 was what made the index
worth its descent.

Run `ANALYZE t` and both queries become

```
 Seq Scan on t  (cost=0.00..1.04 rows=1 width=36)
```

because the table is now known to be one page holding three rows, and no
index can beat reading one page. The row estimate is 1 rather than 0.015
because `clamp_row_est` never returns less than one.

`UPDATE` and `DELETE` choose their scan the same way, without ordering.
A descending `ORDER BY` never uses an index: there are no backward scans.

## EXPLAIN

`EXPLAIN stmt` prints every headline with its estimate, two spaces before
the parenthesis, as PostgreSQL does; `EXPLAIN (COSTS OFF) stmt` prints
the chapter 11 form. The option words are identifiers, not keywords,
like type names (D19): `(COSTS)` and `(COSTS ON)` are accepted too.

```
Limit  (cost=42.30..42.31 rows=2 width=4)
  ->  Sort  (cost=42.30..43.30 rows=400 width=4)
        Sort Key: t.b
        ->  Seq Scan on t  (cost=0.00..25.01 rows=400 width=36)
              Filter: (t.a > 1)
```

Worked example, from `ch14_explain.sql`, showing what the choice looks
like with costs hidden:

```
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a > 1 AND a < 5;
               QUERY PLAN                
-----------------------------------------
 Index Scan using t_pkey on t
   Index Cond: ((t.a > 1) AND (t.a < 5))
(2 rows)

EXPLAIN (COSTS OFF) SELECT * FROM t WHERE 1 = a AND b = 'x';
          QUERY PLAN          
------------------------------
 Index Scan using t_pkey on t
   Index Cond: (t.a = 1)
   Filter: (t.b = 'x')
(3 rows)
```

The first is the range refinement: two inequalities on one column count
once, at 0.005 rather than 1/9, which is enough to beat the sequential
scan that a single `a > 1` chose. In the second, `1 = a` was rewritten
with the column on the left and the operator mirrored, because the
executor's `IndexScan` expects that shape, and `b = 'x'` had no usable
index on this path so it stayed above the scan as a `Filter`.

A transparent node lends its estimate to the headline of its input: the
scan line above shows the `Filter`'s row count, and a scan under a
`Project` shows the projected width. `Sort` prints its input's width,
where PostgreSQL would already show the projected one (its scans
project; ours project above the sort).

## API

`internal/sql/ast`

```go
type Explain struct { Stmt Stmt; CostsOff bool }
type Analyze struct { Table string }
```

`internal/sql/query`

```go
type Explain struct { Stmt Stmt; CostsOff bool }
type Analyze struct { Rel *catalog.RelationInfo } // nil: every table
```

`internal/catalog`

```go
// IndexInfo gains Pages int32, Tuples int64
func (c *Catalog) UpdateStats(oid tuple.OID, pages int32, tuples int64, xid tuple.XID) error
```

`internal/plan`

```go
type Estimate struct { StartupCost, TotalCost, Rows float64; Width int }
func (e Estimate) String() string
// every node gains Est Estimate and Node gains Estimate() Estimate
func ExplainCosts(n Node) []string
```

`internal/planner`

```go
const SeqPageCost, RandomPageCost, CPUTupleCost, CPUIndexTupleCost, CPUOperatorCost
const DefaultEqSel, DefaultIneqSel, DefaultRangeIneqSel, DefaultUnkSel, DefaultBoolSel
const DefaultPages = 10
func Plan(q query.Stmt) (plan.Node, error) // now cost-based
```

Semantics the tests depend on:

- Parser: `explain (costs off) select 1` prints `EXPLAIN (COSTS OFF)
  SELECT 1`; `(costs)` and `(costs on)` print plain `EXPLAIN`; anything
  else in the parentheses is a syntax error expecting `COSTS`; `explain
  analyze` is still `SELECT, INSERT, UPDATE, or DELETE`. `analyze` and
  `analyze t` print `ANALYZE` and `ANALYZE t`; a second name is `end of
  statement`.
- Analyzer: `Analyze t (16384)`, `Analyze` for the database, `Explain
  (COSTS OFF)` in the dump; `analyze nope` is `relation "nope" does not
  exist` without position; an index name binds (the session skips it).
- `UpdateStats` changes only the `pg_class` row, which moves to the end
  as a new version with the old one stamped `xmax = xid`; the values are
  visible through `Lookup`, `LookupOID`, `LookupIndex`, and the table's
  `Indexes`; `ErrNotFound` for an unknown OID.
- `Estimate.String` prints costs with two decimals and rows with none;
  `ExplainCosts` lines are `Explain`'s with `  (cost=...)` appended to
  the headlines, taking the estimate of the outermost transparent node;
  `Explain` stays as it was; a hand-built node has a zero estimate.
- The numbers in `TestEstimates` and `TestScanChoice` follow the
  formulas above for a table `t (a int4, b text)` without statistics
  (1201 rows, width 36), `u (a int4, b text, c int8)` with indexes on `a`
  and `c` and no statistics (1075 rows, width 44), `small` (1 page, 100
  rows, indexes of 2 pages), and `big` (5000 pages, 100000 rows, indexes
  of 300 pages). The scan choices: on `u`, an equality or a range on an
  indexed column uses the index, a single inequality or a qual on `b`
  does not, `ORDER BY a` uses the index and `ORDER BY a DESC` sorts; on
  `small` everything scans sequentially except `ORDER BY a LIMIT 1`; on
  `big` the index wins for `=` and a range, loses for a single
  inequality, and a full index scan loses to a sort without a `LIMIT`.
- The index scan's quals have the column on the left (`5 = a` becomes
  `(u.a = 5)`), the other quals form the `Filter`, `Sort` is absent when
  the index provides the order, and every node has a non-zero estimate.
- Session: the `EXPLAIN` outputs in `TestExplainCosts` before and after
  `ANALYZE` of a three-row table; `ANALYZE` tags `ANALYZE`, records `1`
  page and the live tuple count, `2` pages for a fresh index, skips an
  index, updates the catalogs' own rows, and persists; `TestPlansAgree`
  compares every predicate through the planner's choice and through a
  forced sequential scan (`a + 0` is no index qual).
- Regress: `ch14_analyze.out`, `ch14_explain.out`, and `ch11_explain.out`
  with `(COSTS OFF)`.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** How candidate paths are represented before one becomes
the plan. The tests pin `EXPLAIN`'s text and its costs to two decimals, and
nothing about the structures that produced them: a list of paths, a
cheapest-so-far and a comparison, or plans costed directly all pass.

**Go you will need.** `math.Log2`, `math.Ceil`, `math.RoundToEven`
(PostgreSQL's `rint`), `math.Max`/`Min`; `fmt.Sprintf("%.2f")` for the
costs; `os.Getenv` is already in the regress runner.

<details><summary><b>Parser.</b></summary>

`parseExplain` accepts `(` `costs` [`off` | `on`] `)` before the statement:
`costs` and `off` are `Ident` tokens compared by text, `on` is a keyword.
`parseAnalyze` takes an optional identifier; `Parse`'s trailing check
produces the `end of statement` error.
</details>

<details><summary><b>Catalog.</b></summary>

`UpdateStats` under `c.mu`: `findClassTID` by OID, `Form` a new row with
the same OID, name, and kind, `c.class.Update`, `invalidate`. `loadIndexes`
already scans `pg_class` for the index names; keep the whole `RelationInfo`
per OID and copy `Pages` and `Tuples` into the `IndexInfo`.
</details>

<details><summary><b>Session.</b></summary>

`ANALYZE`: for a table, `heap.NBlocks`, a counting `heap.Scan`,
`UpdateStats` for the table, then for each index `store.NBlocks(idx.OID)`
and `UpdateStats` with the table's count; a non-table is skipped; `Rel ==
nil` loops over `cat.Tables()`. `EXPLAIN`: `plan.ExplainCosts` unless
`CostsOff`.
</details>

<details><summary><b>plan.ExplainCosts.</b></summary>

Give `explain` a `costs` flag; when set, append `" (" +
n.Estimate().String() + ")"` to the headline before the `-> ` prefix.
`describe` is unchanged: the headline comes from the innermost node, the
estimate from the node `explain` was called with, which is the outermost of
a Project/Filter/scan chain.
</details>

<details><summary><b>Planner, sizes and widths.</b></summary>

Private helpers: `relSize(rel)` (the statistics, or `DefaultPages` and the
density formula with `page.PageSize`, `page.HeaderSize`, an 8-byte-aligned
width, 24 bytes of tuple header, 4 of line pointer), `typeWidth`
(`TypeID.Size()`, 32 for text), `relWidth`, `targetsWidth`, `clampRows`,
`opCount(exprs...)`.
</details>

<details><summary><b>Planner, selectivity.</b></summary>

`selectivity(quals)` walks the list with the table above, collecting
inequalities that `rangeBound` classifies (a `Var` on one side, no `Var` on
the other; `Lt`/`Le` with the column on the left or `Gt`/`Ge` with it on
the right are upper bounds) into a map from `(Rel, Attr)` to a lower/upper
pair, and multiplies the pairs in after the loop. `hasVar` is the
analyzer's, copied.
</details>

<details><summary><b>Planner, paths.</b></summary>

`conjuncts(where)` flattens the top-level `AND`; `splitQuals(quals, idx)`
uses `indexQual`, which accepts an `OpExpr` of the five operators with
`isIndexVar` on one side and `!hasVar` on the other, rebuilding it with the
sides swapped and the operator mirrored when the column is on the right;
`providesOrder(idx, orderBy)`; `scanPaths(rel, where, orderBy)` returns the
sequential scan and one index scan per candidate index, each a
`scanPath{node, ordered}` where `node` is the scan wrapped in a `Filter`
when quals remain. `seqScan` and `indexScan` compute the estimates above
and give the `Filter` the same estimate; `pagesFetched(n, T)` is Mackert-
Lohman.
</details>

<details><summary><b>Planner, plans.</b></summary>

`planSelect`: zero relations is `result` (cost 0.01) with an optional
`Filter`; one relation loops over `scanPaths`, calls `finish(node, q,
ordered)` on each (`sortNode` unless ordered, `project`, `limit`), and
keeps the lowest `TotalCost`, the first on a tie. `Plan` for
`Update`/`Delete` uses `cheapestScan` (the same loop without ordering)
under `modify`, which copies the input's costs with zero rows and width;
`Insert` costs its `Values`.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch14` at the end.
Every test not named in that step or an earlier one still panics.

1. Syntax. `lexer.keywords`, `ast.Analyze.String`, `ast.Explain.String`,
   `parseExplain`, `parseAnalyze`. Green: ast `TestString`; parser
   `TestGolden`, `TestRoundTrip`, `TestSyntaxErrors`.

   ```sh
   go test -race ./internal/sql/ast/... ./internal/sql/parser/... -run 'TestString|TestGolden|TestRoundTrip|TestSyntaxErrors'
   ```
2. Binding. `query.Analyze.String`, `query.Explain.String`, `Analyze` for
   both. Green: analyzer `TestGolden`, `TestErrors`, `TestStmtKinds`;
   planner `TestPlanErrors`.

   ```sh
   go test -race ./internal/sql/analyzer/... ./internal/planner/... -run 'TestGolden|TestErrors|TestStmtKinds|TestPlanErrors'
   ```
3. Statistics. `UpdateStats`, `loadIndexes` with `Pages` and `Tuples`.
   Green: catalog `TestUpdateStats`.

   ```sh
   go test -race ./internal/catalog/... -run 'TestUpdateStats'
   ```
4. Estimates in the plan tree. `Estimate.String`, `ExplainCosts`. Green:
   plan `TestEstimateString`, `TestExplainCosts`; the chapter 11
   `TestExplain` still passes.

   ```sh
   go test -race ./internal/plan/... -run 'TestEstimateString|TestExplainCosts|TestExplain$'
   ```
5. The cost model. `Plan` with sizes, selectivity, and the node
   estimates, sequential scans only. Green: planner `TestEstimates`, and
   the chapter 11 `TestPlanGolden` and `TestPlanShape` unchanged.

   ```sh
   go test -race ./internal/planner/... -run 'TestEstimates|TestPlanGolden|TestPlanShape'
   ```
6. Index paths. `scanPaths`, `indexScan`, the sort elimination, the
   choice. Green: planner `TestScanChoice`, `TestIndexScanShape`.

   ```sh
   go test -race ./internal/planner/... -run 'TestScanChoice|TestIndexScanShape'
   ```
7. Session. `ANALYZE`, `EXPLAIN` with costs. Green: session
   `TestExplain` (now with `COSTS OFF`), `TestExplainCosts`,
   `TestAnalyze`, `TestPlansAgree`, and `TestRegress` with
   `ch14_analyze.sql`, `ch14_explain.sql`, and the changed
   `ch11_explain.sql`.

   ```sh
   REGRESS_CHAPTER=14 go test -race ./internal/session/... ./internal/regress/... -run 'TestExplain$|TestExplainCosts|TestAnalyze|TestPlansAgree|TestRegress'
   ```

### When a test fails

- catalog `TestUpdateStats` — the counts are written and the next
  `Lookup` still returns the old ones. `UpdateStats` is a `heap.Update`
  of the `pg_class` row, so it must invalidate the name cache like any
  other catalog change.
- catalog `TestUpdateStats` — the table's row is updated and the index's
  is not. An index gets its own `pg_class` row updated, with the index
  file's own block count and the table's tuple count.
- planner `TestEstimates` — every cost is out by the same small factor.
  A row estimate is rounded to the nearest integer and clamped at 1
  before it is used, not after; feeding an unrounded count into the next
  node's arithmetic accumulates the difference.
- planner `TestEstimates` — a never-analysed table estimates zero rows.
  `Pages == 0` means "no statistics", not "empty": assume
  `DefaultPages` and derive the tuple count from the width, which for
  `(a int4, b text)` gives 1201.
- planner `TestEstimates` — `a > 1 AND a < 9` comes out at 1/9. Two
  inequalities bounding the same column from opposite sides count once,
  as a range, at 0.005. Two on the *same* side stay at 1/3 between them.
- planner `TestEstimates` — `IS NULL` or `AND` adds operator cost. Only
  `OpExpr`, `Neg` and `Cast` count as operators; the boolean nodes and
  the null test are free, as in `cost_qual_eval`.
- planner `TestEstimates` — a `Sort` over one row costs nothing or
  divides by zero. `N` is `max(input rows, 2)`, because `log2(1)` is 0.
- planner `TestScanChoice` — the index is used for a single inequality.
  Compare whole-plan totals, not scan totals: at 1/3 selectivity an
  index scan pays random-page prices for every page of the table and
  loses to a sequential scan.
- planner `TestScanChoice` — a tie picks the index. The sequential scan
  wins a tie, which is what `add_path` does for equal total costs.
- planner `TestIndexScanShape` — `5 < a` is left as a `Filter`. A
  comparison with the indexed column on the right is rewritten with the
  column on the left and the operator mirrored, because the executor's
  `IndexScan` expects that shape.
- planner `TestIndexScanShape` — an index that matches no qual is never
  considered. It is still a candidate when it delivers the `ORDER BY`
  order, which is what lets a `LIMIT` skip the sort.
- planner `TestIndexScanShape` — a descending `ORDER BY` uses the index.
  There are no backward scans, so only one ascending key on the indexed
  column eliminates a sort.
- plan `TestExplainCosts` — the estimate is right and the spacing wrong.
  Two spaces separate the headline from the parenthesis, and `COSTS OFF`
  must reproduce the chapter 11 output byte for byte.
- plan `TestExplainCosts` — the scan line shows its own row count rather
  than the filter's. A transparent node lends its estimate to the
  headline of its input: the scan shows the `Filter`'s rows and, under a
  `Project`, the projected width.
- session `TestPlansAgree` — the plan `EXPLAIN` prints differs from the
  one that ran. `EXPLAIN` must plan the statement exactly as execution
  would, from the same statistics, and not re-analyse in between.
- session `TestAnalyze` — `ANALYZE nope` reports a position. `ANALYZE`
  opens the table as a utility command, so the error has no caret;
  naming an index is a silent no-op.
- `TestRegress` — `ch11_explain.out` differs. It changed in this
  chapter: `EXPLAIN` now takes options and the file uses `COSTS OFF`.
  Regenerate with `-update` and read the diff.

## Why no column statistics

Every predicate here takes the selectivity PostgreSQL uses when it has no
histogram: `0.005` for an equality, a third for an inequality. Real
statistics mean a sampling pass in `ANALYZE`, a `pg_statistic` catalog,
histograms and most-common-value lists, and they feed the same formulas this
chapter already implements (D21). The defaults are enough to make the
planner switch from an index scan to a sequential scan as a table grows,
which is what the chapter is about. Two consequences show up in `EXPLAIN`
and in the tests: the row estimate for `a = 1` does not depend on the value,
and a small analysed table scans sequentially even on an indexed equality,
because fetching half a percent of a one-page table costs more than reading
the page.

## Out of scope

Per-column statistics (histograms, most common values, null fraction,
distinct counts), so every selectivity is a default; index correlation;
the tree height in the descent cost (taken as one level); `EXPLAIN
ANALYZE`, `VERBOSE`, and the other options; backward index scans;
bitmap scans; index-only scans; parameterised paths (chapter 15 adds
the join cases); `effective_cache_size` and the second branch of
Mackert-Lohman for tables larger than the cache; sorts that spill to
disk; joins and join ordering (chapter 15).

## Check your understanding

1. `CREATE TABLE t (a int4, b text)` and no `ANALYZE`. How many rows does
   the planner think `t` has, and what does a sequential scan with one
   qual cost?
   <details><summary>Answer</summary>

   1201 rows over 10 pages. The width is `4 + 32 = 36`, rounded up to 40,
   so a page holds `(8192 - 24) / (24 + 4 + 40) = 120.1` rows and ten
   pages hold 1201. The scan costs
   `10 * 1 + 1201 * (0.01 + 0.0025) = 25.01`: ten page reads plus a tuple
   cost and one operator per row.
   </details>
2. On that table with an index on `a`, why does `WHERE a = 1` choose the
   index and `WHERE a > 1` not?
   <details><summary>Answer</summary>

   Selectivity. Equality is 0.005, so the index visits 6 entries and
   fetches 6 tuples from 5 pages, totalling 24.26 against the scan's
   25.01. One inequality is 1/3, so the index would fetch 400 tuples
   spread over the whole table at 4 per page; reading all ten pages
   sequentially is far cheaper. Adding a second, opposite bound makes it
   a range at 0.005 and the index wins again.
   </details>
3. `ANALYZE t` on a table with three rows makes both plans sequential
   scans costing 1.04 with `rows=1`. Where does the 1 come from, given
   `3 * 0.005 = 0.015`?
   <details><summary>Answer</summary>

   `clamp_row_est`: a row estimate is rounded to the nearest integer and
   never allowed below 1. Zero rows would make every cost above the scan
   zero and every comparison between plans meaningless, so the planner
   refuses to believe a qual matches nothing.
   </details>
4. Why does the planner build whole plans and compare their totals,
   rather than picking the cheapest scan and then adding the sort, the
   projection and the limit?
   <details><summary>Answer</summary>

   Because the nodes above the scan change which scan is best. An index
   scan that delivers rows in `ORDER BY` order removes a `Sort`, which
   can be worth more than the scan itself costs; and a small `LIMIT`
   favours a low startup cost over a low total, because the plan stops
   early. Comparing scans alone cannot see either effect. PostgreSQL
   does the same thing with a fractional path comparison over startup
   and total cost.
   </details>
5. Every selectivity here is a constant from `costsize.c`. What does
   PostgreSQL compute instead, and what does the difference cost us?
   <details><summary>Answer</summary>

   `ANALYZE` samples the table and stores, per column, the fraction of
   nulls, the most common values with their frequencies, a histogram of
   the rest, and the number of distinct values, plus the correlation
   between the column's order and the physical order. With those,
   `a = 1` is estimated from the actual frequency of 1 rather than a flat
   0.005, and `a > 1` from where 1 falls in the histogram. Without them
   every equality looks equally selective, so the planner cannot tell a
   primary key lookup from a scan of a column with two distinct values,
   and a range on a skewed column is estimated as if it were uniform.
   The machinery is real work and the cost model here does not need it to
   demonstrate the choice, which is why it is out of scope rather than
   deferred.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **A histogram.** Have `ANALYZE` sample rows, sort the values, and store
   equal-frequency bucket bounds; then estimate `a < k` by interpolation
   instead of by a third. `EXPLAIN`'s row counts are your test.
2. **`EXPLAIN ANALYZE`.** Run the plan and print actual rows and time next
   to the estimates. Every node needs a counter and a clock, and the
   regression output needs a way to hide the timings.
3. **Read `clauselist_selectivity` in `clausesel.c`.** Ours multiplies the
   clauses' selectivities. Find where PostgreSQL does the same, what it does
   about clauses on the same column, and construct a query where the
   independence assumption is off by a factor of a hundred.
