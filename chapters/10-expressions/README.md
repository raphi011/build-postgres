# Chapter 10: Expression evaluation

**Goal.** Evaluate a bound expression against a row: compile the typed
tree the analyzer produced into a closure tree once, then call it per row
with PostgreSQL's three-valued logic and integer overflow semantics.

**You edit.** `internal/executor/expr/expr.go`: 6 functions with
`panic("not implemented")` bodies, `NewLayout` through `Compare`.
`Compile` is most of the chapter: one closure per `query` node kind.
Nothing else changes; `expr_test.go` is the contract.

**Needs from earlier chapters.** `internal/sql/query` (chapter 09) for
the node types `Var`, `Const`, `OpExpr`, `BoolExpr`, `Neg`, `NullTest`,
`Cast`, `internal/sql/ast` (chapter 07) for the `BinOp` constants, and
`internal/tuple` (chapter 02) for `Datum` and the type IDs.
The tests build every expression by running `parser.Parse` and
`analyzer.Analyze` on `SELECT <expr>`, so chapters 07 and 09 must pass
first.

**Done when.** `make test-ch10` passes.

**Effort.** Medium, about 3 hours. Three-valued logic (step 5) is the hard
part: `NULL AND FALSE` is `FALSE`, and the operands are evaluated in the
order that makes it so.

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

The analyzer hands over a tree in which every node is typed and every
column is a range table position. Nothing has computed a value yet. This
chapter is the piece that, given a row, turns `(t.a + 1) > u.d` into
`TRUE`, `FALSE`, or `NULL`, and it is the first code in the book that has
to get SQL's three-valued logic and PostgreSQL's integer semantics exactly
right, because everything the executor prints goes through it.

PostgreSQL source: `executor/execExpr.c` (`ExecInitExpr`,
`ExecInitExprRec`), `executor/execExprInterp.c` (`ExecInterpExpr`,
`ExecEvalBoolAnd` and the `EEOP_BOOL_*` steps, `EEOP_NULLTEST_*`),
`executor/execQual.c` in older releases, `utils/adt/int.c` (`int4pl`,
`int4mi`, `int4mul`, `int4div`, `int4um`), `utils/adt/int8.c`,
`utils/adt/bool.c`, `utils/adt/varlena.c` (`text_cmp`, `varstr_cmp`),
`include/executor/execExpr.h`.

## Compile, then run

PostgreSQL does not walk the expression tree for every row. `ExecInitExpr`
flattens the tree once into an array of steps (`ExprEvalStep`) for a small
interpreter, and JIT-compiles the steps to machine code when a query is
expensive enough. The point is the same either way: pay for the type
switches and node dispatch once per query, not once per row.

Our version of that is a closure tree. `Compile` walks the bound
expression once and returns a `Func`; each node becomes a closure that
holds the compiled closures of its children and does one thing when
called. A `Var` becomes a closure that indexes the row at a slot fixed at
compile time; an integer `+` becomes a closure that calls its two children
and adds with an overflow check; nothing looks at a `query` node again
after `Compile` returns. The executor (chapter 11) compiles a query's
expressions in `Open` and calls the results from `Next`.

Worked example. `select t.a, u.d, t.a + 1 from t, u where t.a > 1`
against `t(a int4, b text, c bool)` and `u(a int4, d int8)`, evaluated on
the row `(7, "hi", TRUE, 7, 100)`:

| Expression | Value | Go type | Null |
|---|---|---|---|
| `t.a` | 7 | `int32` | false |
| `u.d` | 100 | `int64` | false |
| `(t.a + 1)` | 8 | `int32` | false |
| `(t.a > 1)` as a qual | `true` | – | – |

Each of those is a separate `Func` produced by one `Compile` call, and
each was called with the same `Row`. The dynamic type is part of the
contract: `t.a + 1` is an `int4` expression, so it returns `int32`, not
`int64`, even though the addition itself was done in 64 bits.

Set slot 0 to `NULL` and the qual becomes `false` rather than an error:
`>` is strict, so it yields `NULL`, and `Qual` accepts only `TRUE`. That
one line is where `WHERE` gets its rule that unknown means no.

```go
type Func func(r Row) (v tuple.Datum, null bool, err error)
```

Every result carries a null flag next to the value, which is how
PostgreSQL passes `Datum` plus `isnull` everywhere in the executor. A
function's value is undefined when `null` is set; callers must check the
flag first.

## Rows and layouts

A `Row` is a slice of values and a parallel slice of null flags, one slot
per column. For a query over range entries `t` and `u`, the row is the
columns of `t` followed by the columns of `u`, so a `Var{Rel: 1, Attr: 1}`
is slot `len(t.Desc.Attrs) + 1`. `Layout` holds that prefix sum:
`Layout[rel]` is the first slot of range entry `rel`, and one extra
element at the end holds the total width, so `NewLayout(rng)` returns
`len(rng)+1` integers. `Slot(v)` is `l[v.Rel] + v.Attr`. An `INSERT`
value list has no range table; its layout is `NewLayout(nil)`, which is
`[0]`, and its row is empty.

Worked example. For `FROM t, u` with `t` three columns wide and `u` two,
`NewLayout` returns `[0 3 5]`:

| Slot | 0 | 1 | 2 | 3 | 4 |
|---|---|---|---|---|---|
| Column | `t.a` | `t.b` | `t.c` | `u.a` | `u.d` |
| `Var` | 0,0 | 0,1 | 0,2 | 1,0 | 1,1 |

`Width()` is 5, the last element. `Slot(Var{Rel: 1, Attr: 1})` is
`l[1] + 1 = 4`, so `u.d` reads slot 4. The extra trailing element is what
makes `Width` free and what makes `NewLayout(nil)` come out as `[0]`, a
layout of width zero for an `INSERT` value list that has no columns to
read.

PostgreSQL resolves the same question at plan time: a `Var` is rewritten
to `INNER_VAR`, `OUTER_VAR`, or a scan tuple and an attribute number
(`set_plan_references`), and the interpreter fetches from the matching
slot. We have no join nodes until chapter 15, and even then the row of a
join is the concatenation of its inputs, so one flat layout covers every
node.

## Semantics

**Predict.** `FALSE AND (1/0 = 0)`, `(1/0 = 0) AND FALSE`, and
`NULL AND (1/0 = 0)`. One is `FALSE`, and the other two are not the same
thing as each other. Which is which?

All operators are strict except `AND`, `OR`, and `IS NULL`: a `NULL`
operand yields `NULL` without evaluating the operator. That is one check
at the top of each closure, and it is what makes `NULL = NULL` come out
`NULL` rather than `TRUE`.

Integers. `int4` arithmetic is done in 64 bits and checked against the
`int32` range; `int8` arithmetic uses Go's wrapping operations and detects
overflow the way `int8.c` does, by checking the sign of the result against
the operands (`pg_add_s64_overflow` and friends in `include/common/int.h`).
Overflow is `integer out of range` for `int4` and `bigint out of range`
for `int8`, SQLSTATE 22003. Division truncates toward zero, as in C and
Go. A zero divisor is `division by zero`, 22012, and `MinInt / -1` is out
of range, both checked in that order like `int4div`. Unary minus on the
minimum value is out of range too. Wrapping silently, which is what Go
does by default, is the bug this chapter's property test exists to
catch.

Comparison. All four types compare with `= <> < <= > >=`. Integers
numerically, booleans with `FALSE < TRUE` (`boollt`), text bytewise as
in the `C` collation (`varstr_cmp` with `lc_collate_is_c`), so `'B' <
'a'` and `'ab' < 'abc'`. Go's string comparison is already bytewise, and
UTF-8 byte order agrees with code point order, so there is no collation
code to write; the chapter's job is to say that this is a choice and that
PostgreSQL's default collations do something else. `Compare` is exported
because chapter 11's `Sort` needs the same order and chapter 12's B-tree
needs it for keys.

Three-valued logic. `AND` and `OR` take any number of arguments (the
analyzer flattened them) and follow Kleene's tables: `AND` is `FALSE` if
any argument is `FALSE`, else `NULL` if any is `NULL`, else `TRUE`; `OR`
is the mirror image; `NOT NULL` is `NULL`. Arguments are evaluated left
to right and evaluation stops at the first `FALSE` for `AND` and the
first `TRUE` for `OR`, since nothing later can change the result. That
is what `ExecEvalBoolAnd` does, and it is observable: `FALSE AND (1/0 =
0)` is `FALSE`, while `(1/0 = 0) AND FALSE` is an error, and so is `NULL
AND (1/0 = 0)`, because a `NULL` does not settle the answer.

`Qual` wraps a `Func` for use as a filter: the row passes only if the
result is `TRUE`. This is `ExecQual`, and it is where `WHERE` acquires
its rule that `NULL` means "no". Chapter 11's `Filter` node and chapter
15's join conditions call it.

Worked example of the arithmetic edges, all four of which are errors:

| Expression | Message |
|---|---|
| `2147483647 + 1` | `integer out of range` |
| `9223372036854775807 + 1` | `bigint out of range` |
| `1 / 0` | `division by zero` |
| `-2147483648 / -1` | `integer out of range` |

The first would silently wrap to `-2147483648` in Go, which is exactly
what the property test exists to catch. The last is the reason the two
division checks are ordered: a zero divisor is reported first, and only
then is the quotient checked for the one case where a division overflows.

And of the comparison order, where all four results are `-1`:

| Call | Why |
|---|---|
| `Compare("B", "a")` | `'B'` is 0x42 and `'a'` is 0x61: bytewise, not alphabetical |
| `Compare("ab", "abc")` | a prefix sorts first |
| `Compare(false, true)` | `FALSE < TRUE` |
| `Compare(int32(1), int32(2))` | numerically |

The first row is the one that surprises. In PostgreSQL's usual `en_US`
collation `'a' < 'B'`; in the `C` collation, which is what we implement,
it is not. Sorting output in chapter 11 will show it.

`Cast` is `int4` to `int8` only, and `NULL` stays `NULL`.

## Errors

```go
type Error struct {
	Err error  // ErrOutOfRange or ErrDivisionByZero
	Msg string // PostgreSQL's message text
}
```

The shape matches `analyzer.Error` minus the position, since PostgreSQL
reports these without one. Chapter 11 prints `Msg` after `ERROR:  `.

## API

`internal/executor/expr`

```go
var ErrOutOfRange, ErrDivisionByZero error
type Error struct { Err error; Msg string }

type Row struct { Values []tuple.Datum; Nulls []bool }

type Layout []int
func NewLayout(rng []*query.RangeEntry) Layout
func (l Layout) Slot(v *query.Var) int
func (l Layout) Width() int

type Func func(r Row) (v tuple.Datum, null bool, err error)
func Compile(e query.Expr, l Layout) Func
func (f Func) Qual(r Row) (bool, error)

func Compare(a, b tuple.Datum) int
```

Semantics the tests depend on:

- Values have the Go types of `tuple.Datum`: `int32`, `int64`, `bool`,
  `string`. An `int4` expression yields `int32` and an `int8` one
  `int64`; the tests compare with `!=` on the interface, so the dynamic
  type matters.
- `NewLayout` lays out range entries in order and appends the total
  width; `NewLayout(nil)` is `Layout{0}`.
- Every error returned by a `Func` is a `*Error` unwrapping to one of the
  two sentinels, with `Msg` one of `integer out of range`,
  `bigint out of range`, `division by zero`, and `Error()` returning
  `Msg`.
- Arithmetic overflow and division checks as described above, in that
  order. The property test compares every int4 and int8 operation on
  random operands, weighted toward the range edges, against `math/big`.
- Comparison of every type per the order above; `NULL` on either side is
  `NULL`.
- The complete truth tables in `TestThreeValuedLogic`, and the
  evaluation order in `TestShortCircuit`.
- `Qual` is `TRUE` only, and returns the evaluation error if any.
- `Compare` panics or misbehaves only on mixed types, which the analyzer
  rules out; the tests never call it that way.
- `Compile` is called once per expression by the tests and the returned
  `Func` many times; a `Func` must not keep state between calls.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** A type switch, `switch e := e.(type)`, binds `e`
to the concrete node type in each case. A closure captures the variables
around it, so whatever is computed before `return func(r Row) ...` (a
slot, a constant, the compiled children) is fixed once and shared by
every call. `math.MinInt32` and friends. Integer arithmetic wraps
silently, and `math.MinInt64 / -1` is `MinInt64`, not a panic.
`int64(v.(int32))` widens; converting back with `int32(res)` is what
gives a result its dynamic type. A generic
`func cmp[T int32 | int64 | string](a, b T) int` saves three copies of
one three-way comparison; `<` on strings is bytewise.

<details><summary><b>Private helpers.</b></summary>

The solution's `Compile` is a type switch that handles `Var`, `Const`,
`NullTest`, and `Cast` inline and delegates to `compileOp` (arithmetic and
comparison), `compileBool`, and `compileNeg`. Two constructors,
`outOfRange(t tuple.TypeID)` and `divisionByZero()`, build the `*Error`
values so the message text lives in one place; `outOfRange` picks `integer`
or `bigint` from the type.
</details>

<details><summary><b><code>NewLayout</code>, <code>Slot</code>, <code>Width</code>.</b></summary>

A prefix sum: allocate `len(rng)+1` ints, then `l[i+1] = l[i] +
rng[i].Rel.Desc.Len()`. `Slot` is `l[v.Rel] + v.Attr` and `Width` the last
element; both work unchanged on the `[0]` layout of an `INSERT`.
</details>

<details><summary><b><code>Var</code> and <code>Const</code>.</b></summary>

Resolve `l.Slot(e)` before building the closure; the closure returns
`r.Values[slot], r.Nulls[slot], nil` and does nothing else. A `Const`
closure ignores the row and returns the captured `Value` and `Null`. The
property test compiles with a nil layout, so a `Const` must never touch
`l`.
</details>

<details><summary><b><code>compileOp</code>.</b></summary>

Compile both children, then pick at compile time a `func(a, b tuple.Datum)
(tuple.Datum, error)` for the operator: `int4Op` or `int8Op` for `Add`
through `Div`, chosen by `e.Left.Type()` (`e.Typ` is `Bool` for a
comparison, and the analyzer has already cast both operands to one type),
`compareOp` for the rest. The returned closure is the strict wrapper:
evaluate the left child and return on error or NULL, the same for the
right, then call the operator. Every strict node in the chapter has this
shape.
</details>

<details><summary><b><code>int4Op</code>.</b></summary>

Widen both operands to `int64`, compute, and check the result once against
`MinInt32..MaxInt32`; that one check covers `+`, `-`, `*`, and `MinInt32 /
-1`, since no product of two `int32` values overflows `int64`. Test the
zero divisor before dividing. Convert the result back to `int32`, or the
tests' `!=` on the interface fails.
</details>

<details><summary><b><code>int8Op</code>.</b></summary>

Wrapping is the bug, so detect it after the fact as `int.h` does. `+`
overflowed iff the result moved the wrong way, `(res > x) != (y > 0)`; `-`
iff `(res < x) != (y > 0)`; `*` iff `x != 0 && res/x != y`, plus the one
case Go's division hides, `x == -1 && y == MinInt64`; `/` checks the zero
divisor first, then `x == MinInt64 && y == -1`. Nothing else can overflow a
division.
</details>

<details><summary><b><code>compileNeg</code> and <code>Cast</code>.</b></summary>

Negation is strict and fails only on the minimum value, so test for
`MinInt32` or `MinInt64` (by `e.X.Type()`) before negating. `Cast` is
`int64(v.(int32))` with NULL passed through; `Typ` is always `Int8` here.
</details>

<details><summary><b><code>Compare</code> and <code>compareOp</code>.</b></summary>

`Compare` type-switches on `a` and asserts `b` to the same type: `int32`,
`int64`, and `string` through the generic `cmp`; `bool` by hand, equal is
0, otherwise the `false` side is smaller. `compareOp` calls `Compare` once
and maps its sign to the six operators (`Eq` is `c == 0`, `Lt` is `c < 0`,
and so on). Text needs no collation code: Go's `<` on strings is the order
the tests want.
</details>

<details><summary><b><code>compileBool</code>.</b></summary>

Compile all arguments first. `Not` has one argument and is strict. `And`
and `Or` share one loop parameterised by the deciding value, `false` for
`And` and `true` for `Or`: 1. Evaluate the next argument; return an error
at once. 2. NULL: remember it and continue. Returning NULL here is the trap
`TestShortCircuit` sets with `null and (1 / 0 = 0)`. 3. Equal to the
deciding value: return it without evaluating the rest. 4. After the loop:
NULL if any argument was, else the other value.
</details>

<details><summary><b><code>NullTest</code>.</b></summary>

Evaluate the operand, return its error, then produce `null != e.Not` as a
non-NULL boolean. This is the one closure that never sets the null flag.
</details>

<details><summary><b><code>Qual</code>.</b></summary>

Call `f`; an error or a NULL result is `false`, otherwise the value
asserted to `bool`.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch10` at the end.
Every test not named in that step or an earlier one still panics.

1. Layout and constants. `NewLayout`, `Slot`, `Width`, and `Compile` with
   the `*query.Const` case (a `Null` const yields `null` set). Green:
   `TestLayout`, `TestConstants`. Every test compiles a whole tree, even
   the layout one (`select 1 from t, u`), so a temporary panic on the
   node kinds you have not reached yet is fine.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestLayout$|TestConstants'
   ```
2. Vars. The `*query.Var` closure: resolve the slot once at compile
   time, then index `Values` and `Nulls`. Green: `TestVars`.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestVars'
   ```
3. Arithmetic. `*query.OpExpr` with `ast.Add`, `Sub`, `Mul`, `Div` for
   `int32` and `int64`, `*query.Neg`, `*query.Cast` (int4 to int8), and
   the `*Error` values for `integer out of range`, `bigint out of range`,
   `division by zero`. Green: `TestArithmetic`, `TestArithmeticVars`, and
   `TestArithmeticProperty`, which checks 40,000 random operations
   against `math/big` and compiles an `OpExpr` directly with a nil
   layout. See `int4Op` and `int8Op` above.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestArithmetic|TestArithmeticVars|TestArithmeticProperty'
   ```
4. Comparison. `Compare`, then `*query.OpExpr` with `ast.Eq` through
   `ast.Ge` on top of it; see `compileOp` above for the operand type.
   Green: `TestCompare`, `TestComparison`, `TestComparisonVars`.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestCompare|TestComparison|TestComparisonVars'
   ```
5. Three-valued logic. `*query.BoolExpr` with `And`, `Or`, `Not` over any
   number of arguments; see `compileBool` above. Green:
   `TestThreeValuedLogic`, `TestShortCircuit`.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestThreeValuedLogic|TestShortCircuit'
   ```
6. Null test. `*query.NullTest`, the one node that evaluates its operand
   and does not propagate `NULL`. Green: `TestNullTest`, `TestErrors`
   (it mixes overflow and division cases with comparison and `IS NULL`).

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestNullTest|TestErrors'
   ```
7. Filter. `Qual`. Green: `TestQual`.

   ```sh
   go test -race ./internal/executor/expr/... -run 'TestQual'
   ```

`TestLayoutJoinOrder` in the same package belongs to chapter 15 and is
not part of `make test-ch10`.

### When a test fails

- `TestLayout` — `Width()` panics or the last range entry's columns land
  one slot short. `NewLayout` returns `len(rng)+1` integers: the starting
  slot of each entry and the total width at the end.
- `TestConstants` — a `NULL` constant returns a zero value with `null`
  clear. The value is undefined when the flag is set, but the flag has to
  be set; every caller checks it first.
- `TestVars` — the row index is recomputed on every call. Resolve the
  slot in `Compile`, before the closure is returned, and capture it. That
  is the entire point of compiling.
- `TestArithmetic` — `2147483647 + 1` returns `-2147483648`. Go wraps
  silently. Do `int4` arithmetic in `int64` and check the result against
  the `int32` range before converting back.
- `TestArithmetic` — the `int4` result comes back as `int64`. The
  dynamic type is part of the contract: convert back with `int32(res)`.
- `TestArithmetic` — `-2147483648 / -1` returns `-2147483648` instead of
  an error, or `1 / 0` panics. Check the zero divisor first, then the
  overflow case, in that order, exactly as `int4div` does.
- `TestArithmeticProperty` — passes thousands of cases and fails on an
  `int8` multiplication near the limit. 64-bit overflow cannot be
  detected by widening; check the sign of the result against the signs of
  the operands, as `pg_mul_s64_overflow` does, or divide back.
- `TestCompare` — `Compare("B", "a")` is positive. Text compares
  bytewise, in the `C` collation, and Go's `<` on strings already does
  that. Do not fold case.
- `TestComparison` — `NULL = NULL` is `TRUE`. Every operator except
  `AND`, `OR` and `IS NULL` is strict: check both operands' null flags
  before doing anything else.
- `TestThreeValuedLogic` — `NULL AND FALSE` is `NULL`. `AND` is `FALSE`
  as soon as any argument is `FALSE`, whatever came before; only if none
  is `FALSE` and one is `NULL` is the answer `NULL`.
- `TestShortCircuit` — `FALSE AND (1/0 = 0)` returns an error. Stop at
  the first `FALSE` for `AND` and the first `TRUE` for `OR` and return
  immediately, without evaluating the remaining arguments. Conversely
  `NULL AND (1/0 = 0)` must still evaluate the second argument and
  therefore must error.
- `TestNullTest` — `NULL IS NULL` returns `NULL`. `IS NULL` is the one
  node that evaluates its operand and does not propagate: it always
  returns a non-null boolean.
- `TestErrors` — `errors.Is` fails, or the message has a prefix. Every
  error is an `*Error` wrapping one of the two sentinels, with `Error()`
  returning `Msg` exactly.
- `TestQual` — a row whose qual is `NULL` passes. Only `TRUE` passes.
- Any test after a `Compile` is reused — results differ between calls on
  the same row. A `Func` must keep no state between calls; anything
  mutable captured by the closure is shared by every row.

## Why closures and not a step machine

PostgreSQL compiles an expression into a flat array of steps (`ExprState`,
run by `ExecInterpExpr`) and, with LLVM, compiles that array to machine
code. The flat form exists to give the JIT something to compile and to avoid
a function call per node on a hot loop; it costs an opcode per operation and
a hand-written dispatch loop. We build a tree of Go closures instead: the
same evaluation order and the same null handling in a tenth of the code,
with one indirect call per node. It is a deliberate departure from mirroring
PostgreSQL's design (D4), and the place to remember it is `Compile`, which
does the work PostgreSQL's `ExecInitExpr` does.

## Out of scope

Functions and operators beyond the fixed set (no `%`, no string
concatenation), collations other than byte order, `float`/`numeric`
arithmetic, `CASE`, `COALESCE`, `IN`, `BETWEEN`, `LIKE`, subquery
expressions, aggregate evaluation, expression steps and the JIT, and
constant folding (`eval_const_expressions`), which would belong in the
planner.

## Check your understanding

1. A query is `FROM a, b, c` where `a` has 2 columns, `b` has 4 and `c`
   has 1. What does `NewLayout` return, and which slot is
   `Var{Rel: 2, Attr: 0}`?
   <details><summary>Answer</summary>

   `[0 2 6 7]`: the first slot of each entry, then the total width. `c`
   starts at slot 6, so `Var{Rel: 2, Attr: 0}` is slot 6 and `Width()` is
   7. The trailing element is not a range entry; a layout for *n*
   entries has *n*+1 integers.
   </details>
2. Evaluate `FALSE AND (1/0 = 0)`, `(1/0 = 0) AND FALSE` and
   `NULL AND (1/0 = 0)`.
   <details><summary>Answer</summary>

   `FALSE`, then an error, then an error. `AND` evaluates left to right
   and stops as soon as an argument is `FALSE`, because nothing later can
   change the answer; in the second the division happens before that
   `FALSE` is ever seen. In the third the `NULL` does not settle
   anything, so evaluation continues and the division raises. The order
   is observable, which is why it is specified rather than left to the
   implementation.
   </details>
3. Why does every `Func` return a null flag alongside the value instead
   of using a nil `tuple.Datum` to mean `NULL`?
   <details><summary>Answer</summary>

   Because the value and its nullness are independent, and packing them
   into one interface costs an allocation and a type check on the hot
   path. PostgreSQL passes `Datum` plus `isnull` for the same reason: a
   `Datum` is a machine word with no room for a null bit. It also makes
   strictness a single flag test at the top of each closure rather than a
   nil comparison per operand type.
   </details>
4. `Compile` returns a closure tree rather than an interpreter over a
   flat array of steps, which is what PostgreSQL builds. What do the two
   share, and where do they differ?
   <details><summary>Answer</summary>

   Both pay for the node dispatch once per query instead of once per row,
   which is the point. PostgreSQL's flat array can additionally be
   JIT-compiled to machine code, and it keeps the whole expression in one
   cache-friendly block; a closure tree is a chain of indirect calls
   through heap-allocated frames. In Go the closure version is idiomatic
   and about as fast without a JIT to compare against, and it is far
   shorter, which is the trade this book makes (D4).
   </details>
5. Text compares bytewise here, in what PostgreSQL calls the `C`
   collation, so `'B' < 'a'`. What does PostgreSQL's default collation do
   instead, and what does it cost?
   <details><summary>Answer</summary>

   It sorts by the operating system's locale rules, so `'a' < 'B'` and
   accented letters sort next to their unaccented forms. The cost is
   large: comparisons go through `strcoll`, which is far slower than
   `memcmp`, so PostgreSQL keeps an abbreviated-key optimisation to avoid
   it; and the collation is defined outside the database, so a libc
   upgrade can change the sort order and silently corrupt every index
   built under the old one. That last problem is why `C` collation is a
   real choice in production and not only a simplification here.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Constant folding.** Fold `1 + 2` and `NULL AND FALSE` at compile time
   and leave a constant closure behind. PostgreSQL does this in the planner
   (`eval_const_expressions`); decide whether it belongs there for us too,
   and what happens to division by zero in a folded branch.
2. **Conditional expressions.** Add `CASE` and `COALESCE` to the evaluator,
   which is the first time an expression does not evaluate all of its
   operands. Build the `query` nodes by hand in a test, since the parser and
   analyzer do not produce them.
3. **Read `execExprInterp.c`.** Follow one `EEOP_FUNCEXPR_STRICT` through
   the dispatch loop and find where the null check our closures do inline
   lives there. Then find the computed-goto version and what it needs from
   the compiler.
