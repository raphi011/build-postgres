# Chapter 13: Index integration

**Goal.** `CREATE [UNIQUE] INDEX`, `DROP INDEX`, and `PRIMARY KEY` through
parser, analyzer, catalog, and session; every INSERT, UPDATE, and DELETE
keeps the table's B-trees in step with its heap; unique indexes reject
duplicate keys with PostgreSQL's message; and an `IndexScan` executor
node reads a key range through the tree.

**You edit.** `internal/index/index.go`: 5 functions with
`panic("not implemented")` bodies, `Open` through `Build`.
`internal/catalog/catalog.go`: 3, `CreateIndex` through `LookupIndex`.
`internal/sql/ast/ast.go`: 2, `CreateIndex.String` and `DropIndex.String`.
`internal/sql/query/query.go`: 2, the same names. And your own bodies
from earlier chapters: `lexer.keywords` gains `index` and `unique`;
`parser.parseStmt` learns the two statements; `analyzer.Analyze` binds
them and refuses an index where a table is expected; `catalog.Bootstrap`
creates `pg_index`, `Lookup`/`LookupOID`/`Tables` fill `Indexes`, and
`DropTable` drops a table's indexes; `plan.Explain` prints `IndexScan`;
`executor.Build` builds it and `ModifyTable` maintains indexes;
`session.run`, `Describe`, `Tables`, `Result.String`, and `wrap` learn
the new statements, the `Indexes:` footer, and the new errors. The tests
of every package above are the contract; the chapter 07 and 08 tests
change where noted below.

**Needs from earlier chapters.** `internal/btree` (chapter 12: `Create`,
`Open`, `Insert`, `Scan`, `Meta`), `internal/heap` (`Scan`, `Fetch`),
`internal/catalog`, `internal/executor/expr` (`Compile`, `Compare`), and
everything under them. No earlier package was stubbed for this chapter.

**Done when.** `make test-ch13` passes.

**Effort.** Medium, about 5 hours. Five stubs, but they are spread over
seven packages; the executor wiring (step 6), where every write has to
keep the indexes in step with the heap, is the hard part.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

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
                              Heap access method (05)   B-tree [12, 13]
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

Chapter 12 built a B-tree that nothing uses. A database needs three more
things before an index earns its keep: a catalog row that says which
table and column it covers, so that a restart finds it again; code on
the write path that adds an entry for every new tuple, so that the tree
never disagrees with the heap; and a scan node on the read path that
follows the tree to the heap instead of reading the whole table. This
chapter adds all three, plus the one constraint that PostgreSQL enforces
through an index rather than in the executor: `UNIQUE`, and with it
`PRIMARY KEY`.

The planner does not choose index scans yet. It has no statistics to
choose with; chapter 14 adds `ANALYZE`, costs, and `EXPLAIN` output that
shows the choice. Here the `IndexScan` node exists, is tested directly,
and prints as PostgreSQL does, so that chapter 14 only has to pick it.

PostgreSQL source: `catalog/index.c` (`index_create`, `index_build`,
`index_drop`), `commands/indexcmds.c` (`DefineIndex`,
`ComputeIndexAttrs`), `access/index/indexam.c` (`index_open`,
`index_insert`, `index_beginscan`), `access/nbtree/nbtinsert.c`
(`_bt_check_unique`), `access/nbtree/nbtsort.c` (`_bt_load`),
`executor/execIndexing.c` (`ExecInsertIndexTuples`, `ExecOpenIndices`),
`executor/nodeIndexscan.c` (`IndexNext`), `access/table/table.c`
(`table_open`), `include/catalog/pg_index.h`, and `bin/psql/describe.c`
(`describeOneTableDetails`).

## The statements

```
stmt: CREATE [UNIQUE] INDEX ident ON ident '(' ident ')'
    | DROP INDEX ident
```

Exactly one key column (D10), no `USING btree` (there is only one access
method), no unnamed index. `create` and `drop` now dispatch on the next
keyword: `parseStmt` sees `create`, advances, and expects `table`,
`index`, or `unique`; anything else is a syntax error with `Expected`
`TABLE or INDEX`. Two chapter 07 error cases change with that:
`create t (a int4)` and `drop t` now expect `TABLE or INDEX`. `index`
and `unique` become reserved keywords, so `create table t (a int4
unique)` still fails at the same place, and `unique` can no longer name
a column.

`ast.CreateIndex{Name, Table, Column, Unique}` prints as `CREATE [UNIQUE]
INDEX i ON t (a)`; `ast.DropIndex{Name}` as `DROP INDEX i`. Neither
carries a position: PostgreSQL reports every `CREATE INDEX` error without
one, because the table is opened by the utility command, not the parser.

The analyzer binds `CreateIndex` to `query.CreateIndex{Name, Rel
*catalog.RelationInfo, Attr (0-based), Unique}`, dumped as `CreateIndex i
ON t (16384) (a)` with ` UNIQUE` appended for a unique index, and
`DropIndex` to `query.DropIndex{Name}`, dumped as `DropIndex i`. The
errors, all without position, wrap the usual sentinels: `relation "nope"
does not exist` (`ErrUndefinedTable`), `column "nope" does not exist`
(`ErrUndefinedColumn`; the message with `named in key` that
`ComputeIndexAttrs` also has is for constraints), and the new one below.

## An index is a relation

`CREATE INDEX` makes a new relation: a `pg_class` row with `relkind` `i`
and its own OID, whose file `base/<oid>` is the tree. The index has no
`pg_attribute` rows (PostgreSQL gives it one per key column; we look the
column up through the table instead) and a row in the new catalog
`pg_index` (OID 2610):

| attnum | name | type | meaning |
|---|---|---|---|
| 1 | `indexrelid` | `int4` | OID of the index relation |
| 2 | `indrelid` | `int4` | OID of the table it indexes |
| 3 | `indkey` | `int4` | 1-based `attnum` of the key column |
| 4 | `indisunique` | `bool` | `UNIQUE` index or primary key |
| 5 | `indisprimary` | `bool` | created by `PRIMARY KEY` |

Bootstrap creates `pg_index` as the third catalog: its `pg_class` row
comes after `pg_attribute`'s, then its five `pg_attribute` rows follow
`pg_attribute`'s own. The chapter 08 tests expect that from the start
(three catalog rows, XIDs `{1, 1, 1, 10, 11}` in `TestCreateTable`,
`pg_index` in every `Tables` list, the dropped row of `TestDropTable`
at item 4).

`catalog.IndexInfo{OID, Name, Rel, Attr, Unique, Primary}` is the joined
form of that row, with `Attr` 0-based. `RelationInfo.Indexes` lists a
table's indexes, PostgreSQL's `rd_indexlist`, in the order psql prints
them: the primary key first, then by name. `Lookup`, `LookupOID`, and
`Tables` fill it; DDL invalidates the cache as before, so a new index is
seen by the next lookup. An index is itself found by `Lookup`, with
`Kind` `RelKindIndex`, an empty `Desc`, and no `Indexes`, and it appears
in `Tables()` like any `pg_class` row; `session.Tables` (`\dt`) filters
it out. `LookupIndex(name)` returns the `*IndexInfo` held in the table's
`Indexes`, the same pointer.

Because an index is a relation, a table's name space includes index
names: `CREATE INDEX t ON t (b)` fails with `relation "t" already
exists`, and `CREATE TABLE i ...` fails when `i` is an index. Using the
one for the other is the new error class `ErrWrongObjectType`
(SQLSTATE 42809): `SELECT * FROM i`, `INSERT INTO i`, `UPDATE i`, and
`DELETE FROM i` report `"i" is an index` at the table name, which is
`table_open` refusing the relation (through `parserOpenTable`, hence the
caret); `CREATE INDEX x ON i (a)` reports the same without a position;
`DROP TABLE i` says `"i" is not a table` and `DROP INDEX t` says `"t" is
not an index`.

Worked example. `CREATE TABLE t (a int4 PRIMARY KEY, b text)` followed by
`CREATE INDEX i ON t (b)` produces three relations, not one:

```
 oid   |   relname    | relkind
-------+--------------+---------
  1259 | pg_class     | r
  1249 | pg_attribute | r
  2610 | pg_index     | r
 16384 | t            | r
 16385 | t_pkey       | i
 16386 | i            | i
```

and two `pg_index` rows:

| indexrelid | indrelid | indkey | indisunique | indisprimary |
|---|---|---|---|---|
| 16385 | 16384 | 1 | t | t |
| 16386 | 16384 | 2 | f | f |

Three files exist under `base/`: 16384 is the heap, 16385 and 16386 are
B-trees. `indkey` is the 1-based `attnum`, so the primary key indexes
column 1 and `i` indexes column 2; `catalog.IndexInfo.Attr` is the same
number 0-based. Neither index has any `pg_attribute` rows, and both are
`Lookup`-able by name, which is why `CREATE TABLE i (...)` would now
fail with `relation "i" already exists`.

`PRIMARY KEY` creates the unique index `<table>_pkey` with `indisprimary`
set, right after the table. It cannot be dropped on its own: `DROP INDEX
t_pkey` fails with `cannot drop index t_pkey because constraint t_pkey
on table t requires it` (`ErrDependentObjects`, 2BP01, PostgreSQL's
dependency wording without quotes). `DROP TABLE` drops the table's
indexes with it. When the `_pkey` name is taken, `CREATE TABLE` fails
with `relation "t_pkey" already exists` and leaves no table behind, as
PostgreSQL's transaction rollback would; the session drops the table it
just created.

## Building and maintaining

**Predict.** A table has an index on `a` and three rows with keys 1, 2,
3. You update the second row's key to 9 and delete the third. How many
entries does the tree hold afterwards?

`CREATE INDEX` is `catalog.CreateIndex`, which creates the empty tree,
followed by `index.Build`, which scans the heap and inserts an entry for
every visible tuple (`index_build`; PostgreSQL sorts the entries first
and loads the leaves bottom-up in `_bt_load`, which is faster but not a
new idea). NULL keys are indexed as NULL, as the tree already supports.
A unique index over duplicate keys fails the build with `could not
create unique index "i"`, and the session drops the half-built index, so
nothing is left of the attempt.

From then on `ModifyTable` keeps the index current, `ExecInsertIndexTuples`
in `nodeModifyTable.c`: after an INSERT or UPDATE has written the new
heap tuple, `index.Insert` adds an entry pointing at its TID to every
index of the table. A DELETE leaves its entry behind, as does the old
version of an updated row: the tree cannot delete (D10), and PostgreSQL
leaves dead entries for VACUUM too. Readers therefore fetch the heap
tuple behind every entry and skip the ones with a non-zero `xmax`, the
same visibility rule the heap scan applies.

Worked example. An index `i` on `t(a)`, then
`INSERT INTO t VALUES (1,'x'),(2,'y'),(3,'z')`, then
`UPDATE t SET a = 9 WHERE a = 2`, then `DELETE FROM t WHERE a = 3`. The
table has two live rows, and the tree has four entries:

| Key | Heap TID | Heap tuple |
|---|---|---|
| 1 | `(0,1)` | live |
| 2 | `(0,2)` | dead, `xmax` set by the update |
| 3 | `(0,3)` | dead, `xmax` set by the delete |
| 9 | `(0,4)` | live, the new version written by the update |

Nothing is ever removed. The update added an entry and left the old one;
the delete added nothing and left its entry. That is why every reader
fetches the heap tuple behind an entry and checks its `xmax`: the tree
alone cannot tell a live entry from a dead one, and two of these four
entries point at tuples nobody may see. PostgreSQL is in the same
position between vacuums.

Everything that can make the index insert fail is checked before the
heap tuple is written (D20), in `index.Check`, because there is no
rollback to remove the heap tuple afterwards. Two things can: a key
whose index tuple would exceed `btree.MaxItemSize` (a heap tuple may be
three times larger, so a long `text` value fits the table but not the
tree; PostgreSQL's `index row size 2712 exceeds btree version 4 maximum
2704 for index "i"`, `ErrTooLarge`, SQLSTATE 54000), and a duplicate key
in a unique index. The unique check, `_bt_check_unique`: for every unique
index of the table, scan the tree for the new key and fetch each entry's
heap tuple; a live one that is not the tuple being replaced is a
conflict, `duplicate key value violates unique constraint "t_pkey"`
(`ErrUniqueViolation`, 23505; PostgreSQL adds a `DETAIL` line naming the
key, which psql prints and we do not). An UPDATE passes its old TID as
the exception, so a row updated to its own key succeeds; an INSERT
passes the zero TID. NULL keys never conflict: `UNIQUE` allows any
number of NULLs. A dead tuple's entry does not conflict either, so a
deleted key can be inserted again. Because the check precedes the
write, a violation changes nothing, and a multi-row INSERT stops at the
offending row keeping the rows before it (no rollback until chapter 16).

## The index scan

`plan.IndexScan{Rel, Index, Quals}` reads the tuples whose key satisfies
`Quals` in key order. Every qual is an `OpExpr` whose left operand is the
indexed column's `Var`, whose right operand has no `Var`, and whose
operator is one of `=`, `<`, `<=`, `>`, `>=`; no quals reads the whole
index. `Open` evaluates each right-hand side once and turns the quals
into the two `btree.Bound`s of chapter 12: `=` sets both bounds
inclusive, `>` and `>=` the lower, `<` and `<=` the upper; a second qual
on the same side tightens the bound (the larger lower bound or the
smaller upper one wins, exclusive over inclusive at a tie). A NULL bound
value yields no rows, as `a = NULL` is never true. Each entry's heap
tuple is fetched and returned with its TID, dead ones skipped. A scan
with at least one qual stops at the first NULL entry (`_bt_checkkeys`);
a scan without quals returns the NULL-key rows last, as an index used
for `ORDER BY` must.

`EXPLAIN` prints it as PostgreSQL does, without costs until chapter 14:

```
Index Scan using i on t
  Index Cond: ((t.a > 1) AND (t.a <= 5))
  Filter: (t.b IS NULL)
```

`Index Cond` is the quals ANDed into one expression (a single qual prints
alone), and a `Filter` above the scan attaches beneath it as it does to
`Seq Scan`. An alias follows the table name as for `Seq Scan`.

Worked example, on `t(a int4 PRIMARY KEY, b text)` holding
`(1,'x')`, `(2,'y')`, `(5,'z')`:

```
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a > 1 AND a <= 5;
                QUERY PLAN                
------------------------------------------
 Index Scan using t_pkey on t
   Index Cond: ((t.a > 1) AND (t.a <= 5))
(2 rows)

SELECT * FROM t WHERE a > 1 AND a <= 5;
 a | b 
---+---
 2 | y
 5 | z
(2 rows)
```

Both quals became bounds: `> 1` an exclusive lower bound and `<= 5` an
inclusive upper one, so nothing is left over as a `Filter`. The rows
come back in key order without a `Sort`, which is the other thing an
index scan is for. Adding `AND b IS NULL` would leave that qual above
the scan as a `Filter`, because `IS NULL` is not one of the five
operators a bound can be built from.

## `\d`

`Describe(table)` gains psql's `Indexes:` footer, printed after the rows
and before the blank line, one line per index in `Indexes` order:

```
          Table "t"
 Column |  Type   | Nullable 
--------+---------+----------
 a      | integer | not null
 b      | text    | 
Indexes:
    "t_pkey" PRIMARY KEY, btree (a)
    "i" btree (b)
    "u" UNIQUE, btree (d)
```

`Describe(index)` prints psql's index form:

```
            Index "t_pkey"
 Column |  Type   | Key? | Definition 
--------+---------+------+------------
 a      | integer | yes  | a
primary key, btree, for table "t"
```

with `unique, ` in place of `primary key, ` for a unique index and
neither for a plain one. `Result` gets a `Footer []string` for both:
lines printed after the count line (or, for `NoCount`, after the rows)
and before the blank line.

Worked example. Four errors from one session, showing which carry a
caret and which do not:

```
INSERT INTO t VALUES (2, 'dup');
ERROR:  duplicate key value violates unique constraint "t_pkey"
DROP INDEX t_pkey;
ERROR:  cannot drop index t_pkey because constraint t_pkey on table t requires it
SELECT * FROM i;
ERROR:  "i" is an index
LINE 1: SELECT * FROM i;
                      ^
```

The unique violation and the dependency error have no position, because
PostgreSQL raises them from the utility and executor layers where no
parse location is at hand. `"i" is an index` does have one, at the table
name, because it comes from `table_open` under `parserOpenTable`, which
knows where the name was written. Getting that split right is most of
what `TestErrors` checks.

## API

`internal/sql/ast`

```go
type CreateIndex struct { Name, Table, Column string; Unique bool }
type DropIndex struct { Name string }
```

`internal/sql/query`

```go
type CreateIndex struct { Name string; Rel *catalog.RelationInfo; Attr int; Unique bool }
type DropIndex struct { Name string }
```

`internal/sql/analyzer`

```go
var ErrWrongObjectType error // 42809
```

`internal/catalog`

```go
const IndexOID tuple.OID = 2610
var IndexDesc *tuple.Desc
const RelKindIndex RelKind = 'i'
var ErrWrongObjectType, ErrDependentObjects error

type IndexInfo struct { OID tuple.OID; Name string; Rel tuple.OID; Attr int; Unique, Primary bool }
// RelationInfo gains Indexes []*IndexInfo

func (c *Catalog) CreateIndex(name string, rel tuple.OID, attr int, unique, primary bool, xid tuple.XID) (*IndexInfo, error)
func (c *Catalog) DropIndex(name string, xid tuple.XID) error
func (c *Catalog) LookupIndex(name string) (*IndexInfo, error)
```

`internal/index`

```go
var ErrUniqueViolation, ErrTooLarge error // 23505, 54000
type Error struct { Err error; Msg string }

func Open(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo) *btree.Tree
func Check(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID) error
func CheckUnique(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID) error
func Insert(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, tid tuple.TID) error
func Build(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo) error
```

`internal/plan`

```go
type IndexScan struct { Rel *query.RangeEntry; Index *catalog.IndexInfo; Quals []query.Expr }
```

`internal/session`

```go
// Result gains Footer []string
```

Semantics the tests depend on:

- Parser: the golden forms above; `create` and `drop` alone, or followed
  by anything but `TABLE`/`INDEX`/`UNIQUE`, fail with `Expected` `TABLE
  or INDEX` (`create unique t` expects `INDEX`); the positions in
  `TestSyntaxErrors` are those of the token that failed.
- Analyzer: the three `CREATE INDEX` errors have no position; the
  DML-on-index error points at the table name; `create index i on t
  (a)` binds `Attr` 0 whatever the column's constraints.
- `Bootstrap`: `pg_class` rows `pg_class`, `pg_attribute`, `pg_index`;
  `pg_attribute` rows of each in that order; `pg_index` empty.
- `CreateIndex` checks, in order, that `rel` exists (`ErrNotFound`), is a
  table (`ErrWrongObjectType`), is not a catalog (`ErrSystemTable`), and
  that `name` is free in `pg_class` (`ErrExists`), before allocating the
  OID; the index's OID is the next one; the tree is empty
  (`Meta().Root == None`) and keyed by the column's type; the `pg_class`
  row is `(oid, name, i, 0, 0)`, the `pg_index` row `(oid, rel, attr+1,
  unique, primary)`; no `pg_attribute` rows; all rows stamped with `xid`.
- `Indexes`: primary key first, then by name; `nil` for a table without
  indexes and for an index; the same `*IndexInfo` from `LookupIndex`.
- `DropIndex`: `ErrNotFound`, `ErrWrongObjectType` for a table or
  catalog, `ErrDependentObjects` for a primary key, all leaving
  everything in place; otherwise both rows deleted with `xmax = xid`
  and the file gone. `DropTable` drops the table's indexes the same way
  and returns `ErrWrongObjectType` for an index name.
- `index.Open` does no I/O; `Build` on an empty table leaves the tree
  empty; `Build` over duplicates under a unique index fails as above
  and NULLs do not count as duplicates; `Insert` on a table without
  indexes does nothing; entries are in `(key, TID)` order and `Check`
  passes after every operation.
- `CheckUnique` conflicts only with a live tuple other than `except`;
  never on a NULL key, a non-unique index, or a table without indexes;
  it writes nothing. `Check` runs it after refusing any key whose index
  tuple (8-byte header, the key's encoding from chapter 12) would exceed
  `btree.MaxItemSize`, naming the size and the index; a NULL key is never
  too large; `Build` over such a value fails the same way.
- Executor: after INSERT the TIDs in each index equal the table's live
  TIDs sorted by that column (NULLs last, ties by TID); after UPDATE the
  index holds the old entries too, pointing at tuples with a non-zero
  `xmax`; after DELETE the entry stays and `IndexScan` skips it; a
  unique violation or an oversized key on INSERT or UPDATE returns
  `*index.Error` and leaves heap and every index unchanged; `IndexScan`
  results in `TestIndexScan`;
  a bound expression error (`1/0`) is returned unchanged; an UPDATE fed
  by an `IndexScan` updates each row once.
- Session: tags `CREATE INDEX` and `DROP INDEX`; the messages in
  `TestErrors`; a failed unique build and a failed `_pkey` creation leave
  no `pg_class` row; the `\d` forms above; `\dt` without indexes;
  everything survives `Close` and `Open`.
- Regress: `ch13_index.out` and `ch13_unique.out`. `ch11_ddl.sql` was
  reworded to filter `relkind = 'r'`, so its output does not change
  when `t_pkey` appears; `make test-ch11` runs only chapter 11's files.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Yours to design.** When an index is opened and how long the handle lives:
per statement, per plan node, or per write. The tests pin that every insert
and update adds an entry to every index of the table, that a unique
violation leaves neither heap tuple nor index entry behind, and that `DROP
INDEX` takes effect for the next statement.

**Go you will need.** Nothing new. `sort.Slice` with a two-key
comparison for `Indexes`; `errors.As` for `*index.Error` in `wrap`;
`fmt.Sprintf("%q", name)` for the quoted names in messages.

<details><summary><b>Parser.</b></summary>

`parseStmt` sends `create` to a `parseCreate` that advances over the
keyword and switches on `p.tok.Text`: `table` calls `parseCreateTable`,
which now starts at `expect("table")`; `unique` sets the flag and expects
`index`; `index` continues in `parseCreateIndex` (`ident`, `expect("on")`,
`ident`, `(`, `ident`, `)`); anything else is `syntaxError("TABLE or
INDEX")`. `drop` goes the same way. Nothing else changes; `Parse`'s
trailing check produces the `end of statement` error.
</details>

<details><summary><b>Analyzer.</b></summary>

`addRel` checks `info.Kind == catalog.RelKindIndex` after the lookup and
returns `ErrWrongObjectType` at `pos` with `%q is an index`. `createIndex`
looks the table up itself with `noPos`: not found, an index, then
`attrIndex` for the column; the result carries the `*RelationInfo`, not a
range entry. `DropIndex` binds like `DropTable`.
</details>

<details><summary><b>Catalog, reading.</b></summary>

A `loadIndexes(info)` next to `loadAttributes`: scan `pg_index` for rows
with `indrelid == info.OID`, then one `scanClass` to map each `indexrelid`
to its name (PostgreSQL's `RelationGetIndexList` does the join through the
syscache). Sort by `Primary` descending, then `Name`. Call it from
`Lookup`, `LookupOID`, and `Tables` for tables only; leave `Indexes` nil
otherwise, and note that `loadAttributes` on an index finds no rows and
yields an empty `Desc`, which is what `Lookup` of an index returns.
`LookupIndex` finds the `pg_class` row by name (`ErrNotFound`, then the
kind check), reads its `pg_index` row for `indrelid`, looks the table up
through the cache with the private `lookupOID`, and returns the matching
entry of its `Indexes`.
</details>

<details><summary><b>Catalog, writing.</b></summary>

`CreateIndex` under `c.mu`: `findClass` by OID for the four checks in the
order the semantics list, then `newOID`, `btree.Create(pool, oid,
desc.Attrs[attr].Type)`, `insertClass` with kind `i` (give it a kind
parameter), a `pg_index` row through `tuple.Form(IndexDesc, ...)`,
`invalidate`. `DropIndex`: `findClassTID` by name, the kind check, find the
`pg_index` row's TID and `Primary` flag with a private `findIndexTID(oid)`,
the dependency check, delete both rows, `invalidate`, `Discard`,
`Unlink`. `DropTable` calls the same private drop for each row `findIndexTIDs(info.OID)` returns before dropping the
table, and refuses `Kind == RelKindIndex` first. Bootstrap adds `pg_index`
to its two loops and `newCatalog` opens it.
</details>

<details><summary><b>index.Open</b></summary>

`index.Open` is `btree.Open` with `idx.OID` and the type of `rel.Desc.Attrs[idx.Attr]`.
</details>

<details><summary><b>index.Build.</b></summary>

`heap.Open(pool, rel.OID, rel.Desc).Scan()`; per tuple `Deform`, take
`vals[idx.Attr]` (nil when `nulls[idx.Attr]`), and for a unique index keep
a `map[tuple.Datum]bool` of non-NULL keys seen: a repeat is the `could not
create unique index` error before any further insert. Then
`tree.Insert(key, s.TID())`. The scan's `Err` after the loop.
</details>

<details><summary><b>index.Insert</b></summary>

`index.Insert` loops over `rel.Indexes`, opens each, and inserts the row's key or nil.
</details>

<details><summary><b>index.CheckUnique.</b></summary>

For each index with `Unique` and a non-NULL key: `tree.Scan(&Bound{key,
true}, &Bound{key, true})`; for each entry whose `TID() != except`,
`heap.Fetch` and test `Xmax() == 0`; the first live one is
the violation. Close the scan on every path.
</details>

<details><summary><b>index.Check.</b></summary>

For every index with a non-NULL key, `btree.FormIndexTuple(typ, key,
tuple.TID{})`; `ErrKeyTooLarge` becomes the `ErrTooLarge` error, whose size
is `btree.HeaderSize` plus the key's encoded size (4, 8, 1, or 4 plus the
bytes). Then `CheckUnique`. `Build` maps `ErrKeyTooLarge` from
`tree.Insert` the same way.
</details>

<details><summary><b>executor.indexScan.</b></summary>

Fields: `env`, `rel`, `index`, `quals`, the `*btree.Scan`, the open
`*heap.Relation`, a `none` flag for a NULL bound. `Open` compiles each
qual's `Right` against `expr.NewLayout(nil)`, evaluates it, and folds it
into `lo, hi *btree.Bound` by the operator; tighten with `expr.Compare` as
the design section says. `Next`: `false` when `none`; `scan.Next()`; if
`len(quals) > 0 && scan.IsNull()` stop; `Fetch(scan.TID())`, skip `Xmax()
!= 0`, `Deform`, return the row with the TID. `Close` closes the scan.
`Build` maps `*plan.IndexScan` to it.
</details>

<details><summary><b>executor.modifyTable.</b></summary>

`write` for Insert and Update becomes: `form` (the NOT NULL check first, as
before), `index.Check(pool, rel, vals, nulls, except)` with `except` the
old TID for Update and the zero TID for Insert, the heap write, then
`index.Insert` with the new TID. Delete is unchanged. Use `n.rel.Rel` for
the relation: the session refreshed it after every DDL, so its `Indexes`
are current.
</details>

<details><summary><b>plan.describe.</b></summary>

`*IndexScan`: head `Index Scan using <index> on <table>[ alias]` with
`ast.QuoteIdent`; one property `Index Cond: ` with the quals as a
`query.BoolExpr{Op: And}` when there are two or more, the qual alone for
one, nothing for none. `Filter` already appends to its input's properties,
which puts `Filter:` after `Index Cond:`.
</details>

<details><summary><b>session.run.</b></summary>

`CreateTable`: after `cat.CreateTable`, if `q.PrimaryKey >= 0` call
`cat.CreateIndex(q.Name+"_pkey", oid, q.PrimaryKey, true, true, xid)` and
on error `cat.DropTable` before returning the index error worded for the
`_pkey` name. `CreateIndex`: `cat.CreateIndex` (`ErrSystemTable` is worded
with the table's name), then `index.Build` with a fresh `cat.Lookup` of the
table, and `cat.DropIndex` if the build fails. `DropIndex`:
`cat.DropIndex`. A private `indexError(err, name, table)` words `ErrExists`
(`relation %q already exists`), `ErrNotFound` (`index %q does not exist`),
`ErrWrongObjectType` (`%q is not an index`), `ErrDependentObjects` (`cannot
drop index %s because constraint %s on table %s requires it`), and
`ErrSystemTable`; `ddlError` gains `ErrWrongObjectType` (`%q is not a
table`). `wrap` adds `*index.Error`. `Tables` keeps `Kind == RelKindTable`.
</details>

<details><summary><b>session.Describe.</b></summary>

Look the name up; for an index (`Kind == RelKindIndex`) fetch `LookupIndex`
and the table by `LookupOID`, and build the four-column result with the
footer line; for a table build the `Indexes:` footer when `len(Indexes) >
0`, each line ` %q PRIMARY KEY, btree (%s)`, ` %q UNIQUE, btree (%s)`, or `
%q btree (%s)`. `Result.String` writes `Footer` lines after the count line.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch13` at the end.
Every test not named in that step or an earlier one still panics.

1. Syntax. `lexer.keywords`, `ast.CreateIndex.String`,
   `ast.DropIndex.String`, `parseStmt` and the two new rules. Green:
   ast `TestString`; parser `TestGolden`, `TestRoundTrip`,
   `TestSyntaxErrors`, `FuzzParse` (seeds unchanged). See "Parser".

   ```sh
   go test -race ./internal/sql/ast/... ./internal/sql/parser/... -run 'TestString|TestGolden|TestRoundTrip|TestSyntaxErrors|FuzzParse'
   ```
2. Catalog. `Bootstrap` with `pg_index`, `CreateIndex`, `DropIndex`,
   `LookupIndex`, `Indexes` in `Lookup`/`LookupOID`/`Tables`, `DropTable`
   over indexes. Green: the chapter 08 tests again
   (`TestBootstrapDescribesItself`, `TestBootstrapIsDurable`,
   `TestCreateTable`, `TestCreateTableErrors`, `TestTablesSorted`,
   `TestPersistsAcrossReopen`, `TestDropTable`, `TestDropTableErrors`),
   then `TestCreateIndex`, `TestIndexesOrder`, `TestCreateIndexErrors`,
   `TestDropIndex`, `TestDropTableDropsIndexes`, `TestIndexCache`. See
   the two catalog notes.

   ```sh
   go test -race ./internal/catalog/... -run 'TestBootstrapDescribesItself|TestBootstrapIsDurable|TestCreateTable$|TestCreateTableErrors|TestTablesSorted|TestPersistsAcrossReopen|TestDropTable$|TestDropTableErrors|TestCreateIndex$|TestIndexesOrder|TestCreateIndexErrors|TestDropIndex|TestDropTableDropsIndexes|TestIndexCache'
   ```
3. Binding. `query.CreateIndex.String`, `query.DropIndex.String`,
   `Analyze` for the two statements and the index check in `addRel`.
   Green: analyzer `TestGolden`, `TestErrors`, `TestStmtKinds`; planner
   `TestPlanErrors` (`ErrUtility` for both).

   ```sh
   go test -race ./internal/sql/analyzer/... ./internal/planner/... -run 'TestGolden|TestErrors|TestStmtKinds|TestPlanErrors'
   ```
4. The index package. `Open`, `Build`, `Insert`, `CheckUnique`, `Check`.
   Green: `TestOpen`, `TestBuild`, `TestBuildUnique`, `TestInsert`,
   `TestCheckUnique`, `TestCheck`, `TestBuildMatchesInsert`.

   ```sh
   go test -race ./internal/index/... -run 'TestOpen|TestBuild$|TestBuildUnique|TestInsert|TestCheckUnique$|TestCheck$|TestBuildMatchesInsert'
   ```
5. Plan. `Explain` for `IndexScan`. Green: plan `TestRange`,
   `TestExplain`.

   ```sh
   go test -race ./internal/plan/... -run 'TestRange$|TestExplain$'
   ```
6. Executor. `indexScan`, `Build`, `modifyTable.write`. Green:
   `TestInsertMaintainsIndexes`, `TestUpdateMaintainsIndexes`,
   `TestDeleteLeavesIndexEntries`, `TestUniqueViolation`,
   `TestIndexRowTooLarge`, `TestIndexScan`, `TestUpdateThroughIndexScan`.

   ```sh
   go test -race ./internal/executor/... -run 'TestInsertMaintainsIndexes|TestUpdateMaintainsIndexes|TestDeleteLeavesIndexEntries|TestUniqueViolation|TestIndexRowTooLarge|TestIndexScan|TestUpdateThroughIndexScan'
   ```
7. Session. `run`, `indexError`, `ddlError`, `wrap`, `Describe`,
   `Tables`, `Result.String`. Green: `TestResultString`, `TestDescribe`,
   `TestErrors`, `TestIndexes`, and the regression suite `TestRegress`
   with `ch13_index.sql`, `ch13_unique.sql`, and the changed
   `ch11_ddl.out`.

   ```sh
   REGRESS_CHAPTER=13 go test -race ./internal/session/... ./internal/regress/... -run 'TestResultString|TestDescribe|TestErrors|TestIndexes|TestRegress'
   ```

`TestCheckUniqueWaits` and `TestBuildSnapshot` in `internal/index`
belong to chapter 17 and are not part of `make test-ch13`.

### When a test fails

- parser `TestSyntaxErrors` — `create t (a int4)` still expects `TABLE`.
  `create` and `drop` now dispatch on the following keyword, so both
  report `TABLE or INDEX`. Two chapter 07 cases change with this; the
  test table has been updated for them.
- parser `TestGolden` — `create table t (a int4 unique)` now parses.
  `unique` became a reserved keyword, so it can no longer be an
  identifier, and the statement fails at the same place it did before,
  for a different reason.
- catalog `TestBootstrapDescribesItself` — the `pg_attribute` rows are
  in the wrong order after `pg_index` was added. `pg_index`'s `pg_class`
  row comes third, and its five `pg_attribute` rows come after
  `pg_attribute`'s own five, so a user table's first column is item 16.
- catalog `TestIndexesOrder` — the indexes come back sorted by name with
  the primary key among them. The primary key comes first, then the rest
  by name; that is the order psql prints and the order `\d` expects.
- catalog `TestIndexCache` — a freshly created index is invisible to the
  next `Lookup`. `CreateIndex` and `DropIndex` invalidate the whole name
  cache like the other DDL.
- catalog `TestDropTableDropsIndexes` — the table goes and its index
  files stay. `DropTable` drops the table's indexes with it, files and
  catalog rows both.
- index `TestBuildUnique` — a duplicate key builds an index that then
  misbehaves. The build fails with `could not create unique index "i"`,
  and the caller drops the half-built index so nothing is left of the
  attempt.
- index `TestCheckUnique` — a row updated to its own key reports a
  violation. An `UPDATE` passes its old TID as the exception, so an
  entry pointing at the tuple being replaced does not conflict; an
  `INSERT` passes the zero TID.
- index `TestCheckUnique` — two NULLs conflict. `UNIQUE` permits any
  number of NULLs, so a NULL key never conflicts with anything.
- index `TestCheckUnique` — a deleted key cannot be inserted again. An
  entry whose heap tuple has a non-zero `xmax` is not a conflict, which
  is why the check fetches every candidate tuple rather than trusting
  the tree.
- executor `TestUniqueViolation` — the heap tuple is written and then the
  index insert fails, leaving a row nothing points at. Everything that
  can fail is checked *before* the heap write (D20), because there is no
  rollback until chapter 16.
- executor `TestIndexRowTooLarge` — a long `text` value is accepted by
  the table and then breaks the tree. A heap tuple may be about three
  times the largest index tuple, so the size has to be checked against
  `btree.MaxItemSize` in `index.Check`, with PostgreSQL's message naming
  both sizes.
- executor `TestDeleteLeavesIndexEntries` — the delete tries to remove
  the entry. The tree cannot delete (D10); readers skip dead entries by
  fetching the heap tuple.
- executor `TestIndexScan` — `a > 1 AND a <= 5` returns rows outside the
  range, or the bounds are swapped. `>` and `>=` set the lower bound,
  `<` and `<=` the upper, `=` both; a second qual on the same side keeps
  the tighter one, with exclusive winning a tie.
- executor `TestIndexScan` — a scan with quals returns the NULL-key rows.
  A scan with at least one qual stops at the first NULL entry; a scan
  with none returns them last, which is what `ORDER BY` needs.
- executor `TestUpdateThroughIndexScan` — the update loops or misses
  rows. The Halloween rule from chapter 11 still applies, and an index
  scan makes it easier to hit: a new version with a larger key is ahead
  of the scan in key order.
- plan `TestExplain` — `Index Cond` prints two lines for two quals. They
  are ANDed into one expression; a single qual prints alone.
- session `TestErrors` — `"i" is an index` prints without a caret, or
  the unique violation prints with one. Only the errors that come from
  opening a relation named in the statement carry a position.
- session `TestDescribe` — the `Indexes:` footer lands after the blank
  line. `Result.Footer` is printed after the count line and before the
  blank line.
- `TestRegress` — `ch11_ddl.out` differs. It changed in this chapter,
  because `pg_class` now holds a `pg_index` row; regenerate with
  `-update` and read the diff before committing.

## Why the unique check comes before the write

PostgreSQL inserts the heap tuple first and lets the index insertion fail
afterwards, because a transaction abort removes the tuple it wrote. We have
no abort until chapter 16, so a violation has to leave nothing behind:
`CheckUnique` and the size check `index.Check` run before `heap.Insert`, and
`index.Insert` is then arranged so it cannot fail after the heap write
(D20). The consequence is a window PostgreSQL does not have: between the
check and the write nothing stops a second session from inserting the same
key, which is exactly what chapter 17 revisits with `Dirty` snapshots and a
wait.

## Out of scope

Multi-column and expression indexes, partial indexes (`WHERE`), `USING`,
`CREATE INDEX` without a name, `UNIQUE` as a table constraint (only
`CREATE UNIQUE INDEX` and `PRIMARY KEY`), `ALTER TABLE ... ADD
CONSTRAINT`, the `DETAIL` line of the unique violation, `pg_attribute`
rows for indexes, index-only scans and backward scans, planner use of
indexes (chapter 14), deletion of index entries (VACUUM, v2), and the
unique check under concurrent writers (chapter 17 revisits it with
snapshots).

## Check your understanding

1. `CREATE TABLE t (a int4 PRIMARY KEY, b text)` then
   `CREATE INDEX i ON t (b)` on a fresh cluster. How many files are under
   `base/`, and what are the `pg_index` rows?
   <details><summary>Answer</summary>

   Six: three catalogs (`pg_class` 1259, `pg_attribute` 1249, `pg_index`
   2610) and three new relations, `t` at 16384, `t_pkey` at 16385, `i` at
   16386. `pg_index` holds two rows, `(16385, 16384, 1, t, t)` and
   `(16386, 16384, 2, f, f)`. `indkey` is 1-based, so the primary key
   indexes column 1 and `i` indexes column 2.
   </details>
2. An index on `a`, rows with keys 1, 2, 3, then `UPDATE t SET a = 9
   WHERE a = 2` and `DELETE FROM t WHERE a = 3`. What is in the tree?
   <details><summary>Answer</summary>

   Four entries: 1, 2, 3 and 9, pointing at heap TIDs `(0,1)` through
   `(0,4)`. Two of them, keys 2 and 3, point at tuples whose `xmax` is
   set. Nothing is removed, so the tree grows with every update and
   delete until a VACUUM that does not exist yet. Readers fetch the heap
   tuple behind each entry and skip the dead ones.
   </details>
3. Why is the unique check done before the heap tuple is written, rather
   than inserting and undoing the insert on a conflict?
   <details><summary>Answer</summary>

   Because there is nothing to undo with. Until chapter 16 there is no
   rollback, so a heap tuple that has been written stays written and the
   table would gain a row that violates its own constraint. D20 records
   this: every check that can fail happens before the write. PostgreSQL
   inserts first and lets the transaction abort clean up, which is also
   why its unique violation can name the conflicting row in a `DETAIL`
   line and ours does not.
   </details>
4. Why does an index scan fetch the heap tuple for every entry instead of
   answering from the tree, when the tree already holds the key?
   <details><summary>Answer</summary>

   Because the tree does not know which entries are live. `xmax` lives
   in the heap tuple, and an entry left behind by an update or a delete
   is indistinguishable from a good one until the tuple is read. That is
   the same reason PostgreSQL's index-only scans need a visibility map:
   without a per-page record of "everything here is visible to everyone",
   an index-only scan is not safe.
   </details>
5. Our `CREATE INDEX` scans the heap and inserts one entry at a time,
   where PostgreSQL sorts the entries and loads the leaves bottom-up.
   What does that buy, and what does it cost to skip?
   <details><summary>Answer</summary>

   Sorting first means every leaf is written once, in order, packed to
   the fill factor, with no page splits at all, and the internal levels
   are built from the leaves rather than by repeated descent. Building
   by insertion re-descends the tree for every row and splits pages
   continually, which is several times slower and leaves the index less
   densely packed. It is a performance difference only: the tree that
   comes out is a valid B-tree either way, which `TestBuildMatchesInsert`
   checks by comparing the two paths.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Partial indexes.** `CREATE INDEX ... WHERE a > 0`: the predicate is
   stored in the catalog, `Build` and every insert evaluate it, and the
   planner may only use the index for a query whose qual implies it. The
   last part is the hard one.
2. **Index-only scans.** When every column a query needs is in the index,
   skip the heap fetch. Without a visibility map you cannot skip the
   visibility check, so first work out what your version is allowed to
   assume, then measure what is left of the saving.
3. **Read `ExecInsertIndexTuples` and `_bt_doinsert`'s speculative
   insertion.** PostgreSQL's unique check and its insert are one descent
   that holds a buffer lock, not two. Find the point where it decides to
   wait for another transaction, and compare it with our two-phase check.
