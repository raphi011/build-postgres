# Chapter 09: Analyzer

**Goal.** Turn a syntax tree into a bound statement: every table a catalog
entry, every column reference a (range entry, attribute) pair, every
expression typed, and every name or type that does not resolve reported
with PostgreSQL's message and position.

**You edit.** `internal/sql/query/query.go`: 18 functions with
`panic("not implemented")` bodies, `BoolOp.String` through `Cast.String`
(one `String` per statement and expression type).
`internal/sql/analyzer/analyzer.go`: 1 function, `Analyze`, behind which
you write all the transform functions. Nothing else changes;
`analyzer_test.go` is the contract (`query` has no test file of its own).

**Needs from earlier chapters.** `parser.Parse` (chapter 07): every test
parses its source, and the error positions come from the `Loc` fields
the parser sets. `ast.QuoteIdent`, `ast.QuoteString`, and
`ast.BinOp.String` for the dump;
`tuple.TypeID.String` and `tuple.NewDesc` (chapter 02);
`catalog.RelationInfo` and `catalog.ErrNotFound` (chapter 08);
`lexer.Pos` (chapter 06).

**Done when.** `make test-ch09` passes.

**Effort.** Long, about 6 hours. The range table (step 2) and the type
rules (step 3) are the hard part; the clause and statement walkers after
them are mechanical.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser (07) ──► Analyzer [09] ──► Planner (14, 15)
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

The parser knows that `SELECT a FROM t WHERE b > 1` has a column `a`, a
table `t`, and a comparison. It does not know whether `t` exists, which of
its columns `a` is, what type `b` has, or whether `>` makes sense between
that type and an integer. The analyzer answers those questions by looking
names up in the catalog, and turns the syntax tree into a bound tree in
which every table is a catalog entry, every column reference is a position,
and every expression has a type. Everything downstream, expression
evaluation, the executor, the planner, works on the bound tree and never
sees a name it has to resolve.

PostgreSQL source: `parser/analyze.c` (`transformStmt`,
`transformSelectStmt`, `transformInsertStmt`), `parser/parse_clause.c`
(`transformFromClause`, `transformWhereClause`, `transformSortClause`,
`transformLimitClause`), `parser/parse_relation.c` (`addRangeTableEntry`,
`colNameToVar`, `expandRelAttrs`), `parser/parse_expr.c`
(`transformExprRecurse`, `transformAExprOp`), `parser/parse_oper.c`
(`oper_select_candidate`), `parser/parse_coerce.c` (`coerce_type`),
`parser/parse_target.c` (`FigureColname`), `include/nodes/parsenodes.h`
(`Query`, `RangeTblEntry`), `include/nodes/primnodes.h` (`Var`, `Const`,
`OpExpr`, `BoolExpr`, `NullTest`).

## Two packages again

`internal/sql/query` holds the bound node types, `internal/sql/analyzer`
produces them. The split is the same as `ast` and `parser` (chapter 07):
chapters 10, 11, and 14 need the node types and nothing else. PostgreSQL's
`Query` is one struct with a `commandType`; we have one struct per
statement kind, so a type switch replaces a field check.

The analyzer takes a `Catalog` interface with a single `Lookup` method
rather than `*catalog.Catalog`, so the tests can use an in-memory map and
never touch a data directory.

## The range table

Every table a query reads gets a `RangeEntry` in the query's range table:
the catalog's `RelationInfo` plus the name the query uses for it, the alias
if there is one, else the table name. A column reference becomes a `Var`
holding the index of its range entry and the index of the column in that
entry's descriptor, both 0-based. That pair is the column's identity from
here on; the executor turns it into an offset into a row. PostgreSQL's
`Var` holds the same pair as `varno` and `varattno`, 1-based.

Resolving an unqualified name means searching every range entry
(`colNameToVar`): found in none is `column "x" does not exist`, in more
than one is `column reference "x" is ambiguous`. A qualified name `t.x`
first finds the entry whose alias is `t`; if none matches, the error is
`missing FROM-clause entry for table "t"`, unless a table named `t` is in
the range table under a different alias, which is
`invalid reference to FROM-clause entry for table "t"`. An alias hides the
table name. Two entries with the same alias are
`table name "t" specified more than once`, so `FROM t, t` needs an alias on
one side, as in PostgreSQL.

`*` expands to a `Var` per column of every range entry in order
(`expandRelAttrs`). A `SELECT *` without `FROM` is an error.

Worked example. The test catalog has `t(a int4 NOT NULL, b text,
c bool)` at OID 16384 and `u(a int4, d int8)` at OID 16385.
`select * from t, u` binds to

```
Select
  From: t (16384), u (16385)
  Target: a int4 := t.a
  Target: b text := t.b
  Target: c bool := t.c
  Target: a int4 := u.a
  Target: d int8 := u.d
```

Five targets, in range-entry order then column order. The two range
entries are indices 0 and 1, so `t.b` is `Var{Rel: 0, Attr: 1}` and `u.d`
is `Var{Rel: 1, Attr: 1}`. Two output columns are both called `a` and
that is fine: an output name is a label, not an identifier. Writing
`select a from t, u` instead is `column reference "a" is ambiguous`,
because now the name has to resolve to one thing.

Worked example of the flattening.
`select t.a, t.b, u.d from t join u on t.a = u.a where t.a > 1 order by
t.b desc limit 10` binds to

```
Select
  From: t (16384), u (16385)
  Target: a int4 := t.a
  Target: b text := t.b
  Target: d int8 := u.d
  Where: ((t.a = u.a) AND (t.a > 1))
  Order: t.b DESC
  Limit: 10::int8
```

There is no join node left. The `ON` condition and the `WHERE` condition
are one `BoolExpr` with two arguments, `ON` first. `Limit` is
`10::int8`, an `int4` literal retyped rather than wrapped in a `Cast`,
and the output names come from `FigureColname`: the column name for a
plain reference.

Joins are flattened: `t JOIN u ON p JOIN v ON q WHERE r` produces a range
table of three entries and the qual `p AND q AND r`. PostgreSQL keeps the
join tree in the `Query` and flattens inner joins in the planner
(`deconstruct_jointree`); since we have only inner joins, flattening here
loses nothing and spares chapters 11 and 15 a tree walk. `ON` conditions
come first in the qual, in source order, then `WHERE`.

## Types

**Predict.** Column `a` is `int4`. What type does the analyzer give
`'5'` in `WHERE a = '5'`, and what does it give the same literal in
`SELECT 'x'`?

Every bound expression answers `Type()` with one of the four types. The
rules, in the order the analyzer applies them:

- Integer literals are `int4` if they fit in 32 bits, `int8` if in 64,
  else an error. Unary minus directly on a literal is folded first, so
  `-2147483648` is `int4`, and `-9223372036854775808` is representable
  (`doNegate` in `gram.y`).
- String literals and `NULL` start with no type at all. PostgreSQL calls
  this the `unknown` type, and it is how `WHERE a = '5'` works against an
  integer column: the literal takes the type of the other operand and is
  converted at analysis time by parsing its text, exactly as the input
  function of that type would (`coerce_type`). `'5'` becomes the `int4`
  constant 5, `'t'` the boolean `TRUE`, and `'x'` against an integer is
  `invalid input syntax for type integer: "x"`. Boolean spellings are
  PostgreSQL's: `true`, `false`, `yes`, `no`, `on`, `off`, `1`, `0`, any
  unambiguous prefix, case-insensitive, with surrounding whitespace ignored.
  Integers allow surrounding whitespace and a sign.
- An untyped literal that meets no typed operand (`SELECT 'x'`,
  `'a' = 'b'`, `NULL IS NULL`, `ORDER BY 'k'`) becomes `text`, which is
  what PostgreSQL does at the output of a query.
- The only implicit conversion between typed values is `int4` to `int8`.
  An `int4` constant is retyped; anything else gets a `Cast` node. There is
  no conversion to or from `bool` or `text`.
- Arithmetic (`+ - * /`) requires both operands to be integers after
  widening and yields the operand type; `/` is integer division (chapter
  10). Unary minus requires an integer. Anything else is
  `operator does not exist: text + integer`, named the way PostgreSQL
  names types in messages: `integer`, `bigint`, `boolean`, `text`.
- Comparisons (`= <> < <= > >=`) require both operands to have the same
  type after widening and yield `bool`. All four types are comparable.
- `AND`, `OR`, `NOT` require `bool` operands
  (`argument of AND must be type boolean, not type integer`) and are
  flattened: `a AND b AND c` and `a AND (b AND c)` both give one `BoolExpr`
  with three arguments, which is how the planner will split a qual into
  its conjuncts (`make_ands_implicit`).
- `IS [NOT] NULL` accepts any type and yields `bool`.
- `WHERE` and `ON` must be `bool`. `LIMIT` must be `int8` (an `int4` is
  widened, an untyped literal converted) and may not reference columns.

Worked example. Against `t(a int4, b text, c bool)`:

| Expression | Bound as | Type |
|---|---|---|
| `a = '5'` | `(t.a = 5)` | `bool` |
| `'x'` alone | `'x'` | `text` |
| `'x' + 1` | error: `invalid input syntax for type integer: "x"` | – |
| `a + b` | error: `operator does not exist: integer + text` | – |
| `a + 1` | `(t.a + 1)` | `int4` |
| `10` in `LIMIT` | `10::int8` | `int8` |

The first row is the whole unknown-type rule: `'5'` never becomes a text
value that then fails to compare with an integer. It has no type until it
meets `a`, at which point it is parsed as an `int4` and the dump shows a
bare `5`. In the third row the same mechanism produces the error, because
`'x'` is asked to be an integer and cannot. Row two is the fallback: a
literal that meets no typed operand becomes `text` at the query's output.

Constants are not folded: `SELECT 1 + 1` stays an `OpExpr`. PostgreSQL
folds in the planner (`eval_const_expressions`), not the analyzer.

## Statements

`SELECT`: range table from `FROM` and the joins, then the select list, then
`WHERE`, `ORDER BY`, `LIMIT`, in that order, so an error in `FROM` is
reported before one in the select list, as PostgreSQL does. Output column
names follow `FigureColname`: the alias if given, the column name for a
plain column reference, `?column?` for anything else.

`ORDER BY` keys are expressions over the range table, so the executor
can sort before projecting. A key that is an integer literal is a
position in the select list, `ORDER BY 2`; a bare name that matches an
output column name is that column's expression, `SELECT a + 1 AS b ...
ORDER BY b`; anything else is an ordinary expression resolved against the
range table (`findTargetlistEntrySQL92`, then `SQL99`). A name matching
two output columns with different expressions is `ORDER BY "x" is
ambiguous`.

`INSERT`: the target relation is a range entry the value expressions
cannot see; `VALUES (a)` is `column "a" does not exist`. The column list,
or all columns when omitted, maps expression positions to attributes; a
column named twice or unknown is an error. With a column list, the counts
must match. Without one, fewer values than columns is fine and the rest
are `NULL`, more is an error; both messages are PostgreSQL's. Every bound
row has one expression per column of the relation in column order,
converted to the column's type
(`column "a" is of type integer but expression is of type boolean`). A
literal `NULL`, explicit or from an omitted column, in a `NOT NULL` column
is rejected here rather than at execution, since it can never succeed.

Worked example of the `INSERT` rules, again against
`t(a int4 NOT NULL, b text, c bool)`:

| Statement | Bound rows |
|---|---|
| `insert into t (a, b) values (1, 'x')` | `Row: 1, 'x', NULL::bool` |
| `insert into t values (1)` | `Row: 1, NULL::text, NULL::bool` |
| `insert into t (a) values (null)` | error, no position |

Both accepted rows have one expression per column of the relation, in
column order, with the omitted columns filled in as typed nulls; the
executor never has to consult the column list again. The third fails with

```
null value in column "a" of relation "t" violates not-null constraint
```

and `Pos.Line` is 0, because PostgreSQL raises this at execution time and
therefore reports it without a caret. We can see it is unsatisfiable at
analysis time and say so early, but we keep PostgreSQL's message and its
missing position.

`UPDATE` and `DELETE`: the range table is the one target relation, so
`SET a = a + 1` and `WHERE` see its columns. Assigning a column twice is
`multiple assignments to same column "a"`; values are converted like
`INSERT` values, with the same `NOT NULL` check for literal `NULL`.

`CREATE TABLE` becomes a name, a `tuple.Desc`, and the index of the
`PRIMARY KEY` column or -1. `PRIMARY KEY` implies `NOT NULL`. Two primary
keys is `multiple primary keys for table "x" are not allowed`. Duplicate
column names are left to the catalog (`catalog.ErrDuplicateColumn`), and
whether the table already exists is left to execution, as PostgreSQL does
for both. `DROP TABLE`, `BEGIN`, `COMMIT`, `ROLLBACK` pass through.
`EXPLAIN` analyzes its statement.

## Errors

```go
type Error struct {
	Pos lexer.Pos // Line == 0: no position
	Err error     // one sentinel per SQLSTATE class
	Msg string    // PostgreSQL's message text
}
```

`Msg` is what chapter 11 prints after `ERROR:  `, and `Pos` is where it
puts the caret, so both follow PostgreSQL. Positions come from the syntax
tree: a column reference points at its first identifier, an operator error
at the operator, a type error at the offending expression, a missing table
at its name, an `INSERT` column-list problem at the column name. Errors
that PostgreSQL reports without a position (`NOT NULL` violations, which
it detects at execution; a column assigned twice in `SET`; two `PRIMARY
KEY` columns) have `Pos.Line == 0`. Chapter 11 must not print a `LINE`
for those.

| Sentinel | SQLSTATE | Messages |
|---|---|---|
| `ErrUndefinedTable` | 42P01 | `relation "t" does not exist`, `missing FROM-clause entry for table "t"`, `invalid reference to FROM-clause entry for table "t"` |
| `ErrUndefinedColumn` | 42703 | `column "x" does not exist`, `column t.x does not exist`, `column "x" of relation "t" does not exist` |
| `ErrAmbiguousColumn` | 42702 | `column reference "x" is ambiguous` |
| `ErrDuplicateAlias` | 42712 | `table name "t" specified more than once` |
| `ErrDuplicateColumn` | 42701 | `column "a" specified more than once` |
| `ErrUndefinedOperator` | 42883 | `operator does not exist: text + integer`, `operator does not exist: - text` |
| `ErrTypeMismatch` | 42804 | `argument of WHERE must be type boolean, not type integer` (also `AND`, `OR`, `NOT`, `JOIN/ON`), `argument of LIMIT must be type bigint, not type boolean`, `column "a" is of type integer but expression is of type boolean` |
| `ErrInvalidColumnRef` | 42P10 | `ORDER BY position 2 is not in select list`, `ORDER BY "x" is ambiguous`, `argument of LIMIT must not contain variables` |
| `ErrInvalidInput` | 22P02 | `invalid input syntax for type integer: "x"` (also `bigint`, `boolean`) |
| `ErrOutOfRange` | 22003 | `value "2147483648" is out of range for type integer` (also `bigint`) |
| `ErrNotNull` | 23502 | `null value in column "a" of relation "t" violates not-null constraint` |
| `ErrSyntax` | 42601 | `SELECT * with no tables specified is not valid`, `INSERT has more expressions than target columns`, `INSERT has more target columns than expressions`, `VALUES lists must all be the same length`, `multiple assignments to same column "a"` |
| `ErrTableDefinition` | 42P16 | `multiple primary keys for table "x" are not allowed` |

Worked example. Eight bad queries, with the column each error points at:

| Query | Column | Message |
|---|---|---|
| `select x from t` | 8 | `column "x" does not exist` |
| `select a from t, u` | 8 | `column reference "a" is ambiguous` |
| `select t.a from u` | 8 | `missing FROM-clause entry for table "t"` |
| `select * from missing` | 15 | `relation "missing" does not exist` |
| `select a + b from t` | 10 | `operator does not exist: integer + text` |
| `select b from t where b` | 23 | `argument of WHERE must be type boolean, not type text` |
| `select a from t limit 'x'` | 23 | `invalid input syntax for type bigint: "x"` |
| `insert into t (a) values (null)` | none | `null value in column "a" ...` |

The fifth is the one to check your implementation against: column 10 is
the `+`, not column 8 where the expression starts, because the complaint
is about the operator. The sixth points at `b` in the `WHERE`, column 23,
not at the `WHERE` keyword. Positions come from the syntax tree, so
getting them right is a matter of using the node's own `Pos()` rather
than the nearest one to hand.

A catalog error other than `catalog.ErrNotFound` is returned unchanged.

## The dump format

`String()` on a bound statement renders a multi-line dump that the golden
tests compare against. Two-space indentation, no trailing newline:

```
Select
  From: t (16384), u AS v (16385)
  Target: a int4 := t.a
  Target: ?column? int8 := (t.a::int8 + v.d)
  Where: ((t.a = v.a) AND (t.a > 1))
  Order: t.a ASC, t.b DESC
  Limit: 10::int8

Insert t (16384)
  Row: 1, 'x', NULL::bool

Update t (16384)
  Set: a := (t.a + 1)
  Where: t.c

Delete t (16384)
  Where: (t.a = 1)

CreateTable x (id int4 NOT NULL PRIMARY KEY, name text)
DropTable x
Begin
Explain
  Select
    ...
```

Worked example. `select a + 1 as b from t order by b` dumps as

```
Select
  From: t (16384)
  Target: b int4 := (t.a + 1)
  Order: (t.a + 1) ASC
```

The `ORDER BY` key is the target's expression, not a reference to the
output column: `b` matched an output name, so the analyzer substituted
what that name stands for. The executor therefore sorts on a value it can
compute from the input row and never needs the projection to have
happened first. `ASC` is printed here although the source omitted it,
because the dump is canonical rather than a copy of the input.

`From:` lists range entries as `name (oid)` or `name AS alias (oid)` with
identifiers quoted by `ast.QuoteIdent`; absent clauses are omitted.
Expressions print as SQL: a `Var` as `alias.column`; an `int4` constant as
its digits, an `int8` one as `1::int8`, a boolean as `TRUE`, a text one as
a quoted literal, a null as `NULL::type`; a `Cast` as `x::int8`; every
operator application in parentheses with `AND`/`OR` arguments joined
inside one pair; `(NOT x)`, `(-x)`, `(x IS NULL)`. Types are visible
either directly or from the operands, which is what makes the dump a
sufficient check that every expression was typed correctly.

## API

`internal/sql/query`

```go
type Node interface{ String() string }
type Stmt interface{ Node; stmtNode() }
type Expr interface{ Node; Type() tuple.TypeID; exprNode() }

type RangeEntry struct { Alias string; Rel *catalog.RelationInfo }

type Select struct { Range []*RangeEntry; Targets []Target; Where Expr; OrderBy []SortKey; Limit Expr }
type Target struct { Name string; Expr Expr }
type SortKey struct { Expr Expr; Desc bool }
type Insert struct { Rel *RangeEntry; Rows [][]Expr }
type Update struct { Rel *RangeEntry; Set []Assignment; Where Expr }
type Assignment struct { Attr int; Value Expr }
type Delete struct { Rel *RangeEntry; Where Expr }
type CreateTable struct { Name string; Desc *tuple.Desc; PrimaryKey int }
type DropTable struct { Name string }
type Begin struct{}; type Commit struct{}; type Rollback struct{}
type Explain struct { Stmt Stmt }

type Var struct { Rel, Attr int; Typ tuple.TypeID; Alias, Column string }
type Const struct { Typ tuple.TypeID; Value tuple.Datum; Null bool }
type OpExpr struct { Op ast.BinOp; Typ tuple.TypeID; Left, Right Expr }
type BoolOp int // And Or Not
type BoolExpr struct { Op BoolOp; Args []Expr }
type Neg struct { X Expr }
type NullTest struct { X Expr; Not bool }
type Cast struct { X Expr; Typ tuple.TypeID }
```

`internal/sql/analyzer`

```go
type Catalog interface { Lookup(name string) (*catalog.RelationInfo, error) }
type Error struct { Pos lexer.Pos; Err error; Msg string }
func Analyze(stmt ast.Stmt, cat Catalog) (query.Stmt, error)
```

`ast.Insert`, `ast.Update`, and `ast.Delete` gain a `Loc` field holding
the position of the table name, set by the parser, so that
`relation "nope" does not exist` can point at it; `ast.Insert.ColumnLocs`
and `ast.Assignment.Loc` do the same for the column names in an INSERT
column list and an UPDATE's SET, which PostgreSQL also points at.

Semantics the tests depend on:

- The dump format above; the golden table in `analyzer_test.go` is the
  reference.
- `Const.Value` is `int32`, `int64`, `bool`, or `string` by `Typ`, and
  `nil` with `Null` set. `Var.Alias` and `Var.Column` are the range
  entry's alias and the column name.
- `Insert.Rows` are full-width in column order. `Assignment.Attr` and
  `Var.Attr` are 0-based.
- `CreateTable.PrimaryKey` is -1 when there is none; the key column's
  `Attr.NotNull` is set.
- Every error is a `*Error` with the sentinel, message, and position from
  the table above. `Error()` returns `Msg`. Positions on line 1 are
  checked column by column in `TestErrors`.
- `FROM` is analyzed before the select list, so the `ON` error in
  `SELECT a FROM t JOIN u ON t.a` wins over the ambiguity of `a`.
- A join's `ON` clause resolves only against the relations of that join.
  `SELECT 1 FROM t, u JOIN v ON t.a = v.a` is `invalid reference to
  FROM-clause entry for table "t"`, not an accepted plan.
- Every row of a multi-row `VALUES` list has the same length, whether or
  not a column list was written, and the length check runs before the
  per-row ones.
- `Analyze(nil, cat)` returns an error rather than panicking.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** A type switch, `switch x := e.(type)`, with one
case per `ast` node is how every transform dispatches, and the dump does
the same over `query` nodes. `strconv.ParseInt(s, 10, bits)` with 32 or
64 does the range check; on failure, `errors.As` to a `*strconv.NumError`
and `errors.Is(numErr.Err, strconv.ErrRange)` tells out-of-range from bad
syntax. `strings.HasPrefix("true", s)` with the arguments reversed tests
whether `s` is a prefix of `true`. `%q` in `fmt.Sprintf` writes the
double-quoted names in messages. `strings.Builder` with `fmt.Fprintf(&b,
...)` builds the dump, and `strings.ReplaceAll(s, "\n", "\n  ")` indents
a nested one. A `map[int]bool` is a set.

<details><summary><b>Error helper (<code>errAt</code>, <code>noPos</code>, <code>pgTypeName</code>).</b></summary>

Every error goes through `errAt(pos, sentinel, format, args...)`, which
fills an `*Error`; `noPos`, the zero `lexer.Pos`, is what the positionless
errors pass. `pgTypeName` maps a `tuple.TypeID` to `integer`, `bigint`,
`boolean`, `text` for messages; `tuple.TypeID.String` (`int4`, ...) is for
the dump. Positions are never computed: they are a syntax node's `Loc`,
reached through `ast.Expr.Pos()`.
</details>

<details><summary><b>State and dispatch (<code>Analyze</code>).</b></summary>

A private `analyzer` struct holds the `Catalog`; a `scope` struct holds the
range table under construction, a slice of `*query.RangeEntry`, and is what
every expression resolves against. It also carries the index of the first
entry currently visible, which is 0 everywhere except inside a join's `ON`
clause; the entries stay in one flat slice, because `Var.Rel` indexes the
whole range table. `Analyze` type switches on the statement
into one method per kind (`selectStmt`, `insert`, `update`, `delete`,
`createTable`), builds the pass-throughs inline, recurses for `Explain`,
and returns a plain `fmt.Errorf` from the `default`, which is where `nil`
lands.
</details>

<details><summary><b><code>createTable</code>.</b></summary>

One `tuple.Attr` per column, `NotNull` set when either `NOT NULL` or
`PRIMARY KEY` was written; `PrimaryKey` starts at -1, takes the first key's
index, and a second key is `ErrTableDefinition` at `noPos`.
</details>

<details><summary><b>Dump (<code>String</code> methods).</b></summary>

The format is in "The dump format". The solution adds a `String` on
`RangeEntry` (`name (oid)`, with `AS alias` only when the alias differs
from the table name) that `Select`, `Insert`, `Update`, and `Delete` share.
`Select` writes the header, then one line per clause present; `Explain`
indents its statement's dump by two more spaces. `Const` switches on the Go
type of `Value`, `string` going through `ast.QuoteString`; `BoolExpr` joins
its arguments with the operator inside one pair of parentheses, `NOT` being
the one-arg case.
</details>

<details><summary><b><code>addRel(sc, name, alias, pos)</code>.</b></summary>

Looks the table up, turns `catalog.ErrNotFound` into `relation "t" does not
exist` at `pos` and returns any other error as is; defaults the alias to
the table name; rejects an alias already in the scope (at the second
table's position); appends the entry. `INSERT`, `UPDATE`, and `DELETE` call
it with an empty alias, which is why their `Var.Rel` is 0. PostgreSQL:
`addRangeTableEntry` in `parse_relation.c`.
</details>

<details><summary><b><code>fromItem(sc, item, &quals)</code>.</b></summary>

Walks one `FROM` item: a `TableRef` is one `addRel`; a `Join` recurses
left, then right, then analyzes `ON` through `boolClause` and appends it
to the caller's `quals` slice, so quals come out in source order and an
`ON` error precedes any select-list error. The `ON` clause sees only the
relations of its own join: remember `len(sc.entries)` before recursing,
set the scope's visible mark to it around the `boolClause` call, and
restore it after. Otherwise `t, u JOIN v ON t.a = v.a` is accepted, where
PostgreSQL says `invalid reference to FROM-clause entry for table "t"` —
and `columnRef` already builds that message, from the entries outside the
visible window. PostgreSQL: `transformFromClause` in `parse_clause.c`.
</details>

<details><summary><b><code>attrIndex</code>, <code>newVar</code>.</b></summary>

`attrIndex(desc, name)` returns a column's 0-based index or -1 and is
shared with INSERT and UPDATE. `newVar(entry, rel, attr)` builds the `Var`,
copying type, alias, and column name out of the entry, and is also what `*`
expansion in `selectStmt` calls per column per entry.
</details>

<details><summary><b><code>columnRef(sc, ref)</code>.</b></summary>

Applies the rules of "The range table" in this order: qualified, find the
entry whose alias matches, then the column in it (`column t.x does not
exist` if absent); no alias matched, scan again by table name to choose
between `invalid reference` and `missing FROM-clause entry`; unqualified,
scan every entry, keep the first hit, fail on a second. All at `ref.Loc`.
PostgreSQL: `colNameToVar`.
</details>

<details><summary><b><code>intLiteral(text, pos)</code>.</b></summary>

`ParseInt` with 64 bits (`ErrOutOfRange` for bigint on failure), then the
32-bit range check to pick `int4`. The `UnaryExpr` case of `expr` calls it
with `-` prepended to the text when the operand is an `IntLit`, so the fold
happens before any analysis and the out-of-range error sits on the sign.
</details>

<details><summary><b>The unknown type (<code>unknown</code>, <code>resolveUnknown</code>).</b></summary>

The solution declares `unknown` as the zero `tuple.TypeID`; a string
literal or `NULL` becomes a `Const` of that type holding the literal text
or `Null`. It must never reach the output tree, so `resolveUnknown`, which
retypes such a constant to `text`, runs wherever an expression leaves the
analyzer without meeting a typed operand: target list, `ORDER BY` key, `IS
NULL` and unary minus operands, both sides of an operator with two
literals.
</details>

<details><summary><b><code>coerce(e, to, pos)</code>.</b></summary>

The one conversion function: an unknown constant is parsed by
`parseLiteral` (a null just takes the type); a matching type returns `e` as
is; `int4` to `int8` retypes a constant or wraps a `Cast`; anything else
returns `ErrTypeMismatch` together with `e`, so each caller rewords the
mismatch with `e`'s type (`argument of WHERE ...`, `column "a" is of type
...`). PostgreSQL: `coerce_type` in `parse_coerce.c`.
</details>

<details><summary><b><code>parseLiteral</code>, <code>parseBool</code>.</b></summary>

Mirror the input functions: `text` is the string itself; integers use
`ParseInt` on the trimmed text with the target's bit size,
`strconv.ErrRange` becoming `ErrOutOfRange` and any other failure
`ErrInvalidInput`; booleans go through `parseBool`, which trims and
lowercases, then tests prefix-of `true`, `yes`, `false`, `no`, exact
`1`/`0`, `on` from two letters, and `off` from three, so `o` and `of` are
both rejected. `parse_bool_with_len` compares `len > 2 ? len : 3` bytes
against `"off"` for exactly that reason: two letters do not tell `off`
from `on`. The same holds for `yes` and `no`, which is why they are
prefix tests of the full word rather than length-guarded. PostgreSQL:
`parse_bool_with_len` in `bool.c`.
</details>

<details><summary><b><code>expr(sc, e)</code>.</b></summary>

The type switch of `transformExprRecurse`: literals as above, `ColumnRef`
to `columnRef`, `BinaryExpr` to `binary`, `Star` is a syntax error.
`UnaryExpr`: `Neg` folds an `IntLit` (see `intLiteral`), else resolves its
operand and requires an integer type (`operator does not exist: - text` at
the operator); `Not` runs its operand through `boolArg` at the operand's
position and wraps a one-argument `BoolExpr`. `IsNull` resolves its operand
into a `NullTest`.
</details>

<details><summary><b><code>binary(sc, x)</code>.</b></summary>

Analyze both sides first. `AND`/`OR`: each side through `boolArg` at its
own position, then splice in the arguments of a side that is already a
`BoolExpr` of the same operator, which flattens `a AND b AND c` and `a AND
(b AND c)` alike. Other operators, in order: one unknown side is coerced to
the other's type at its own position, two unknown sides both become `text`;
an `int4` side is widened when the other is `int8`; unequal types are
`operator does not exist` at the operator; arithmetic also demands an
integer type and yields it, comparisons yield `bool`. PostgreSQL:
`transformAExprOp`, `transformBoolExpr` in `parse_expr.c`.
</details>

<details><summary><b>Boolean operands (<code>boolArg</code>, <code>boolClause</code>).</b></summary>

Both are `coerce` to `bool` with the mismatch reworded as `argument of X
must be type boolean, not type ...`. `boolArg(e, op, pos)` takes a bound
operand and names `AND`, `OR`, or `NOT`; `boolClause(sc, e, clause)`
analyzes a syntax node first and names `WHERE` or `JOIN/ON`, positioned at
the clause expression. UPDATE and DELETE reuse `boolClause` for `WHERE`.
</details>

<details><summary><b><code>selectStmt</code>, <code>conjunction</code>.</b></summary>

`fromItem` over `FROM`, collecting `ON` quals; the target list (alias, else
a `ColumnRef`'s name, else `?column?`; `*` with an empty scope is the
`ErrSyntax` case); `WHERE` appended after the `ON` quals; `ORDER BY`;
`LIMIT`. `conjunction(quals)` makes the qual: nil for none, the qual itself
for one, else one `And` `BoolExpr` that splices in any qual that is itself
an `And`. PostgreSQL: `transformSelectStmt` in `analyze.c`.
</details>

<details><summary><b><code>sortKey(sc, q, e)</code>.</b></summary>

In order: an `IntLit` as a position (`strconv.Atoi`, `1 <= n <=
len(targets)`, else `ORDER BY position n is not in select list` at the
literal); an unqualified `ColumnRef` against the target names, where two
matches are ambiguous only if their expressions' `String()` differ (`SELECT
a, a ... ORDER BY a` is fine); anything else, a qualified name included,
through `expr` and `resolveUnknown`. PostgreSQL:
`findTargetlistEntrySQL92`.
</details>

<details><summary><b><code>LIMIT</code>, <code>hasVar</code>.</b></summary>

`expr`, then `hasVar`, a recursive walk over the bound tree that reports
any `Var` (`argument of LIMIT must not contain variables`), then `coerce`
to `int8` with the `argument of LIMIT must be type bigint` wording; all
three at the `LIMIT` expression's position. PostgreSQL:
`transformLimitClause`.
</details>

<details><summary><b><code>insert(s)</code>.</b></summary>

First `attrs`, the attribute index for each value position: every column
when the list is omitted, else `attrIndex` per name with a `seen` set,
unknown or repeated names at `ColumnLocs[k]`. Then, before anything is
bound, every row must be as long as the first: `VALUES lists must all be
the same length` at the last expression of the first row that differs.
That check comes first because it is about the `VALUES` list alone, and
because without it a short row is silently padded with nulls — and if the
column it skipped is `NOT NULL`, the user is told about a column they
never wrote. Per row: more values than `attrs` fails at the first surplus
value; with a column list, fewer fails
at `ColumnLocs[len(row)]`; each value goes through `assignValue` with an
empty scope, so a column name does not resolve, into a full-width slice at
`attrs[i]`; slots still nil get a typed null, or `notNull` when the column
is `NOT NULL`. PostgreSQL: `transformInsertStmt`, `checkInsertTargets`.
</details>

<details><summary><b><code>assignValue(sc, e, attr, table)</code>, <code>notNull</code>.</b></summary>

`expr`, then the literal-`NULL` check against `attr.NotNull` (before
coercion, which would give the null a type), then `coerce` to `attr.Type`
with the mismatch reworded as `column "a" is of type ...`. `notNull(column,
table)` builds the not-null error at `noPos`. PostgreSQL:
`transformAssignedExpr` in `parse_target.c`.
</details>

<details><summary><b><code>update(s)</code>, <code>delete(s)</code>.</b></summary>

`update` resolves each `SET` column with `attrIndex` (unknown at
`Assignment.Loc`, repeated at `noPos`) and calls `assignValue` with the
real scope, so `a = a + 1` sees the table; then `WHERE` through
`boolClause`. `delete` is `addRel` plus `boolClause`. PostgreSQL:
`transformUpdateTargetList` in `analyze.c`.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch09` at the end.
Every test not named in that step or an earlier one still panics.

1. Dispatch and dump. `Analyze` and `createTable` per "State and
   dispatch" and "`createTable`" above. In `query.go`, all 18 `String`
   methods per "Dump": `BoolOp.String`, `Select.String`, `Insert.String`,
   `Update.String`, `Delete.String`, `CreateTable.String`, `DropTable.String`,
   `Begin.String`, `Commit.String`, `Rollback.String`, `Explain.String`,
   `Var.String`, `Const.String`, `OpExpr.String`, `BoolExpr.String`,
   `Neg.String`, `NullTest.String`, `Cast.String`. Green:
   `TestCreateTable`. Writing the dump first lets you compare each later
   step against the golden table by eye.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestCreateTable'
   ```
2. Range table. `addRel`, `fromItem`, `attrIndex`, `newVar`, `columnRef`,
   and `*` expansion per the entries above. Green: `TestVars`,
   `TestCatalogErrorsPassThrough`. `Var.Rel` and `Var.Attr` are 0-based.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestVars|TestCatalogErrorsPassThrough'
   ```
3. Expressions and types. `intLiteral`, the unknown type, `coerce`,
   `parseLiteral`, `expr`, `binary`, `boolArg` per the entries above.
   Green: `TestTypes`. Do not fold constants: `1 + 1` stays an `OpExpr`.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestTypes'
   ```
4. SELECT clauses. `selectStmt`, `boolClause`, `conjunction`, `sortKey`,
   `LIMIT` per the entries above. Green: `TestPositionsSpanLines`, which
   wants the position of a column reference in `WHERE` on line 3 carried
   through from the syntax tree.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestPositionsSpanLines'
   ```
5. INSERT, UPDATE, DELETE. `insert`, `assignValue`, `update`, `delete`
   per the entries above. Green: `TestConstValues`, `TestUpdateAttrs`,
   `TestStmtKinds`, and `TestGolden`, which binds every statement kind
   and is the real judge of steps 1 to 5. It stops at the first case that
   fails to bind, so read its cases top to bottom.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestConstValues|TestUpdateAttrs|TestStmtKinds|TestGolden'
   ```
6. Errors. Walk the sentinel table in the README: every error is a
   `*Error` with the sentinel (`errors.Is`), the message verbatim, and
   the position of the offending token; `Pos` is zero for `NOT NULL`
   violations, `multiple assignments to same column`, and
   `multiple primary keys`. Green: `TestErrors`, which checks the column
   of every error on line 1, so a message pointing at the wrong operand
   fails even when the text is right.

   ```sh
   go test -race ./internal/sql/analyzer/... -run 'TestErrors'
   ```

`TestRangeIndex` in the same package belongs to chapter 15 and is not
part of `make test-ch09`.

### When a test fails

- `TestVars` — `Var.Rel` or `Var.Attr` is one too high. Both are 0-based
  here, unlike PostgreSQL's `varno` and `varattno`. The executor indexes
  a slice with them.
- `TestVars` — `select * from t, u` gives three targets instead of five,
  or the columns of `u` come first. Expansion walks the range entries in
  order and every column of each, and duplicate output names are allowed.
- `TestVars` — `FROM t, t` is accepted. Two entries with the same alias
  is `table name "t" specified more than once`; an alias on one side
  fixes it.
- `TestVars` — `select t.a from u` reports `column "a" does not exist`.
  A qualified name that names no range entry is a FROM-clause error, not
  a column error, and the message differs again when a relation of that
  name is present under another alias.
- `TestTypes` — `a = '5'` fails with `operator does not exist`. A string
  literal starts with no type at all; only when it meets no typed operand
  does it become `text`. Give it the other side's type and parse its text
  with that type's input rules.
- `TestTypes` — `'x' + 1` reports `operator does not exist` rather than
  `invalid input syntax for type integer: "x"`. The literal is coerced to
  the other operand's type first, and the coercion is what fails.
- `TestTypes` — `-2147483648` is out of range. Fold the unary minus onto
  the literal before checking the range, as `doNegate` does; the digits
  alone do not fit in an `int4` and the negated value does.
- `TestTypes` — `a AND b AND c` produces nested `BoolExpr` nodes. Flatten
  same-operator arguments into one node with three arguments, so the
  planner can split a qual into conjuncts without a tree walk.
- `TestTypes` — an `int4` column compared with an `int8` one is rejected.
  `int4` widens to `int8`: a constant is retyped in place, anything else
  gets a `Cast` node.
- `TestGolden` — the first case passes and the second dumps with the
  wrong parenthesisation. Every operator application is parenthesised,
  and an `AND` or `OR` puts all its arguments inside one pair.
- `TestGolden` — an `int8` constant prints as its digits. Constants print
  their type where it is not obvious: `1::int8`, `NULL::bool`, `TRUE`,
  quoted text.
- `TestConstValues` — `insert into t values (1)` binds one expression
  rather than three. A bound row always has one expression per column of
  the relation in column order, with typed nulls for the omitted ones.
- `TestUpdateAttrs` — `SET a = 1, a = 2` is accepted. That is
  `multiple assignments to same column "a"`, and PostgreSQL reports it
  with no position, so `Pos.Line` is 0.
- `TestErrors` — the message is right and the column is wrong. Take the
  position from the offending node: an operator error from the
  `BinaryExpr`'s `Loc`, which chapter 07 put on the operator; a column
  error from the `ColumnRef`; a missing relation from the `TableRef`.
- `TestErrors` — a `NOT NULL` violation carries a position. It must not;
  chapter 11 prints no `LINE` when `Pos.Line` is 0.
- `TestPositionsSpanLines` — the column is right and the line is 1. The
  position travels from the lexer through the syntax tree untouched;
  nothing in the analyzer should be recomputing it.
- `TestCatalogErrorsPassThrough` — a catalog I/O error arrives wrapped as
  `relation "t" does not exist`. Only `catalog.ErrNotFound` becomes that
  message; every other error is returned unchanged.

## Why type names are the parser's problem

There is no `pg_type` here, so `int`, `integer`, `int4`, `bigint`, `int8`,
`bool`, `boolean` and `text` are mapped to a `tuple.TypeID` in the parser,
and an unknown type name is a parse error rather than an analysis error
(D19). PostgreSQL resolves type names here, in the analyzer, against a
catalog, which is what lets a user define a type. The consequence to keep in
mind while reading this chapter is that the analyzer never fails on a type
*name*: every type error it raises is about a value or an operator, which is
why the sentinel table has `ErrType` and not `ErrUnknownType`.

## Out of scope

Subqueries, aggregates, `GROUP BY`, `DISTINCT`, functions and casts in
SQL, `unknown`-typed output columns (they become `text` here), the full
type coercion lattice and operator resolution (`func_select_candidate`),
`ORDER BY` on expressions not in the target list with `DISTINCT`, view
expansion, the rewriter, default values, `RETURNING`, and permission
checks.

## Check your understanding

1. `t` is `(a int4, b text)` and `u` is `(a int4, d int8)`. What are the
   `Var` values for `t.b` and `u.d` in
   `select t.b, u.d from t join u on t.a = u.a`, and what is the bound
   `Where`?
   <details><summary>Answer</summary>

   `t.b` is `Var{Rel: 0, Attr: 1}` and `u.d` is `Var{Rel: 1, Attr: 1}`,
   both 0-based. `Where` is the `ON` condition `(t.a = u.a)` on its own:
   the join is flattened into two range entries and its condition becomes
   the qual, with nothing left to mark it as a join condition. Chapter 15
   works out again which quals can drive a join.
   </details>
2. Which of `select 'x'`, `select a = '5' from t` and `select 'a' = 'b'`
   produce a `text` constant, given `a` is `int4`?
   <details><summary>Answer</summary>

   The first and the third. An untyped literal takes the type of the
   typed operand it meets, so `'5'` in the second becomes the `int4`
   constant 5. In the third neither side is typed, so the fallback
   applies to both and they compare as `text`. In the first the literal
   reaches the query's output untyped and becomes `text` there.
   </details>
3. Why does the analyzer flatten `t JOIN u ON p` into a range table plus
   a qual, when PostgreSQL keeps the join tree in the `Query`?
   <details><summary>Answer</summary>

   Because we have only inner joins, and for inner joins the join tree
   carries no information the range table and the conjunction do not:
   `FROM t, u WHERE p` and `FROM t JOIN u ON p` mean the same thing and
   should get the same plan. PostgreSQL must keep the tree because outer
   joins are not flattenable; `deconstruct_jointree` flattens exactly the
   inner ones. Flattening here spares chapters 11 and 15 a tree walk.
   </details>
4. Why does `ORDER BY b` become the expression behind `b` rather than a
   reference to output column `b`?
   <details><summary>Answer</summary>

   So the executor can sort on a value it computes from the input row,
   before or independently of the projection. If the key were a reference
   into the output, the sort would have to run after the projection, and
   `ORDER BY` on a column that is not in the select list at all would
   become impossible. PostgreSQL resolves the same way, trying the
   output-name rule first and falling back to an ordinary expression.
   </details>
5. PostgreSQL's `Query` is one struct with a `commandType` field, and its
   `Var` uses 1-based `varno` and `varattno`. What do those choices buy
   it that we give up?
   <details><summary>Answer</summary>

   One struct means one set of walkers: `query_tree_walker` and the
   rewriter, the view expander and the rule system all operate on any
   statement without a type switch, which matters when there are dozens
   of node types and several passes. 1-based attribute numbers leave 0
   free to mean "whole row" and negative values to mean the system
   columns like `ctid`, which is how `t.*` and `WHERE ctid = ...` are
   represented at all. We have one pass, no views and no system columns,
   so a type switch reads better and 0-based indices go straight into a
   Go slice.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **The coercion lattice.** Replace the fixed rules with a table of casts
   and a search over it (`can_coerce_type`), then add `numeric` and see how
   much of the analyzer changes.
2. **Keep `unknown`.** We resolve a bare string literal to `text` at the
   end; PostgreSQL keeps the type `unknown` all the way to the output column
   and warns. Implement that and find the queries whose result type changes.
3. **Read `transformExpr` and `func_select_candidate` in `parse_expr.c` and
   `parse_func.c`.** Ours picks an operator by an exact type match. Work out
   what PostgreSQL does when two candidates are equally good, and construct
   a query where our rule accepts what PostgreSQL rejects as ambiguous.
