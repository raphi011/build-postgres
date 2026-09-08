# Chapter 07: Parser and AST

**Goal.** A hand-written recursive-descent parser that turns the token
stream into a tree with one Go type per statement and expression kind,
prints that tree back in one canonical form, and parses expressions with
a Pratt loop over PostgreSQL's precedence table.

**You edit.** `internal/sql/ast/ast.go`: 25 functions with
`panic("not implemented")` bodies, `BinOp.String` through `IsNull.String`.
`internal/sql/parser/parser.go`: 2 functions, `Parse` and `ParseExpr`;
the private `parseX` functions behind them are yours to design. Nothing
else changes; `ast_test.go` and `parser_test.go` are the contract.

**Needs from earlier chapters.** `internal/sql/lexer` (chapter 06) for
tokens, positions, and `*lexer.Error`, plus its `IsKeyword`, which this
chapter added and the skeleton already implements; `internal/tuple`
(chapter 02) for the `TypeID` constants that column types map to and
`TypeID.String`, which prints them.

**Done when.** `make test-ch07` passes.

**Effort.** Long, about 6 hours. The Pratt loop in `ParseExpr` (step 3) is
the hard part; the statement parsers after it are repetitive.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer (06) ──► Parser [07] ──► Analyzer (09) ──► Planner (14, 15)
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

The lexer gives us a flat stream of tokens. The parser turns that stream
into a tree that says what the statement means structurally: which table
a `SELECT` reads, which expressions it filters on, and in `a OR b AND c`,
that the `AND` binds first. Everything after this chapter, the analyzer,
the planner, `EXPLAIN`, works on that tree and never sees SQL text again.

PostgreSQL source: `parser/gram.y` (the bison grammar), `parser/parser.c`
(`raw_parser`), `include/nodes/parsenodes.h` (the raw statement nodes:
`SelectStmt`, `InsertStmt`, `CreateStmt`), `include/nodes/primnodes.h` and
`parsenodes.h` again for `A_Expr`, `ColumnRef`, `A_Const`,
`parser/scansup.c`. Our parser is hand-written rather than generated, so
the closest read is `gram.y` for the shape of each rule and the precedence
declarations near its top.

## Two packages

`internal/sql/ast` holds the node types and nothing else: no parsing, no
checking. `internal/sql/parser` builds them. The split keeps later
packages (analyzer, planner) from importing the parser when they only
need the types, and it mirrors PostgreSQL, where `nodes/` is independent
of `parser/`.

## The tree

Three node interfaces, distinguished by unexported marker methods so a
`Stmt` cannot be used where an `Expr` is expected:

```go
type Node interface{ String() string }
type Stmt interface{ Node; stmtNode() }
type Expr interface{ Node; Pos() lexer.Pos; exprNode() }
type TableExpr interface{ Node; tableExpr() }
```

One struct per statement kind: `CreateTable`, `DropTable`, `Insert`,
`Select`, `Update`, `Delete`, `Begin`, `Commit`, `Rollback`, `Explain`.
One per expression kind: `IntLit`, `StrLit`, `BoolLit`, `NullLit`,
`ColumnRef`, `Star`, `BinaryExpr`, `UnaryExpr`, `IsNull`. Two table
expressions: `TableRef` and `Join`. PostgreSQL has a single `A_Expr` node
with an operator name string and a generic `A_Const`; we use one Go type
per case because the analyzer's type switch is easier to read than a
switch on strings, and there are only a dozen cases.

Every expression carries `Loc lexer.Pos` and returns it from `Pos()`. It
is the start of the expression, except for `BinaryExpr` and `IsNull`, where
it is the operator, which is where PostgreSQL points too (`location` in
`A_Expr` is the operator). Chapter 09 reports "column does not exist" and
"operator does not exist" at these positions.

Integer literals keep their digits as text. Whether `3000000000` is an
`int8` or an overflow is decided by the analyzer, and `-2147483648` must
remain representable: it arrives as `UnaryExpr{Neg, IntLit{"2147483648"}}`
and the analyzer folds the sign before checking the range, as
`doNegate` in `gram.y` does.

Worked example. `select name from users where id >= 10` parses to one
`*ast.Select`:

```
Select
  Items: [ SelectItem{ Expr: ColumnRef{Name: "name", Loc: 1:8} } ]
  From:  [ TableRef{Name: "users", Alias: "", Loc: 1:18} ]
  Where: BinaryExpr{ Op: Ge, Loc: 1:33
           Left:  ColumnRef{Name: "id", Loc: 1:30}
           Right: IntLit{Text: "10", Loc: 1:36} }
  OrderBy: nil
  Limit:   nil
```

Read the three positions carefully. `ColumnRef` and `IntLit` sit at their
own first character, columns 30 and 36. The `BinaryExpr` sits at column
33, the `>=`, not at column 30 where the expression it spans begins.
That is why chapter 09 can report "operator does not exist" under the
operator rather than under the left operand. `Alias` is the empty string
and `OrderBy` and `Limit` are nil, not empty slices, because the clauses
were absent.

Optional clauses are `nil` when absent: a `Select` with no `FROM` has
`From == nil`, an `Insert` without a column list has `Columns == nil`.
`ORDER BY x` and `ORDER BY x ASC` both produce `OrderItem{Desc: false}`.

## Printing

`String()` renders a node back to SQL in one canonical form:

- keywords upper case, identifiers as written by the lexer (folded),
- identifiers double-quoted only when they would not lex back to the same
  name: upper-case ASCII letters, spaces, a leading digit, or a keyword,
- string literals single-quoted with `'` doubled,
- every operator application in parentheses: `((a + 1) = b)`,
  `(-a)`, `(NOT a)`, `(a IS NOT NULL)`,
- `DESC` printed, `ASC` not,
- no trailing semicolon.

Worked example. Six statements and their renderings:

| Source | `String()` |
|---|---|
| `select name from users where id >= 10` | `SELECT name FROM users WHERE (id >= 10)` |
| `select a b from t u` | `SELECT a AS b FROM t AS u` |
| `create table t (a int not null, b text primary key)` | `CREATE TABLE t (a int4 NOT NULL, b text PRIMARY KEY)` |
| `insert into t values (1,'a'),(2,'b')` | `INSERT INTO t VALUES (1, 'a'), (2, 'b')` |
| `select -2147483648` | `SELECT (-2147483648)` |
| `select "Order" from "My Table"` | `SELECT "Order" FROM "My Table"` |

The rendering is not the input. `AS` is inserted where the source omitted
it, `int` is printed as the canonical `int4`, the unary minus is
parenthesised like every other operator, and `"Order"` keeps its quotes
because `Order` would lex back as a different (folded) name while
`"My Table"` keeps them because of the space. Re-parsing any right-hand
column gives the identical string again, which is what `TestRoundTrip`
and `FuzzParse` check.

This is the golden format the tests compare against, and the tests re-parse
every rendering to check it produces the same rendering again. The full
parenthesisation makes precedence visible in test failures, and the
round-trip catches a printer and parser that disagree. `ast.QuoteIdent`
and `ast.QuoteString` are exported because chapter 11's `\d` and chapter
14's `EXPLAIN` need the same quoting.

## Statements

The grammar, in the notation of `gram.y` with `[ ]` for optional and
`{ }` for repetition:

```
stmt: CREATE TABLE ident '(' column_def { ',' column_def } ')'
    | DROP TABLE ident
    | INSERT INTO ident [ '(' ident { ',' ident } ')' ]
          VALUES '(' expr_list ')' { ',' '(' expr_list ')' }
    | SELECT select_item { ',' select_item }
          [ FROM table_expr { ',' table_expr } ]
          [ WHERE expr ] [ ORDER BY order_item { ',' order_item } ]
          [ LIMIT expr ]
    | UPDATE ident SET ident '=' expr { ',' ident '=' expr } [ WHERE expr ]
    | DELETE FROM ident [ WHERE expr ]
    | BEGIN | COMMIT | ROLLBACK
    | EXPLAIN stmt              -- SELECT, INSERT, UPDATE, or DELETE only

column_def: ident type_name { NOT NULL | PRIMARY KEY }
select_item: '*' | expr [ [ AS ] ident ]
table_expr: table_ref { JOIN table_ref ON expr }
table_ref: ident [ [ AS ] ident ]
order_item: expr [ ASC | DESC ]
```

Type names are identifiers, not keywords (D17), and the parser maps them:
`int`, `integer`, `int4` to `tuple.Int4`; `bigint`, `int8` to `tuple.Int8`;
`bool`, `boolean` to `tuple.Bool`; `text` to `tuple.Text`. Anything else is
`ErrUnknownType` at the type name, since there is no `pg_type` to consult
later (D19).

Aliases without `AS` work because every keyword is reserved: in
`SELECT a b FROM t` the `b` can only be an alias, and in `FROM t u` the `u`
can only be an alias. PostgreSQL allows the same and pays for it with its
keyword classes.

Joins nest to the left: `t JOIN u ON p JOIN v ON q` is
`Join{Join{t, u, p}, v, q}`, and a `FROM` list can mix plain tables and
joins. Only inner joins with `ON` exist; `INNER`, `CROSS`, `LEFT`, `USING`,
and `NATURAL` are not keywords.

Worked example. `select * from t join u on t.a = u.a join v on v.b = t.b`
renders as

```
SELECT * FROM t JOIN u ON (t.a = u.a) JOIN v ON (v.b = t.b)
```

and the tree behind it is `Join{Join{TableRef t, TableRef u, t.a = u.a},
TableRef v, v.b = t.b}`: the second `JOIN` takes the whole first join as
its left side, not `u`. `From` is a one-element slice here, because the
two joins are one table expression; `FROM t, u` would give a two-element
slice with no `Join` node at all. Chapter 15 turns both shapes into the
same join search, so the difference is syntax only.

`select 1;;select 2;` returns two statements, `SELECT 1` and `SELECT 2`.

`Parse` accepts several statements separated by `;` and returns them all.
Empty statements are skipped, so `""`, `";"`, and `"select 1;;"` are all
fine. This is what PostgreSQL's `raw_parser` does with a multi-statement
string, and the regression runner in chapter 11 relies on it.

## Expressions

**Predict.** Given the precedence table below, what do `NOT a = b`,
`NOT a AND b` and `-a * b` parse to? Two of the three group the way an
arithmetic reading would not.

A Pratt parser (also called precedence climbing). `parseExpr(minPrec)`
parses one prefix expression, then loops: look at the next token, and if
it is an operator whose precedence is at least `minPrec`, consume it, parse
the right operand with `parseExpr(prec + 1)`, and combine. The recursion
with `prec + 1` is what makes `1 + 2 * 3` come out as `1 + (2 * 3)`: while
parsing the right side of `+`, a `*` is allowed to bind, but another `+`
is not, so `1 + 2 + 3` groups to the left.

The precedence table is PostgreSQL's since 9.5, from the `%left` and
`%nonassoc` lines in `gram.y`, lowest first:

| Level | Operators | Notes |
|---|---|---|
| 1 | `OR` | left-associative |
| 2 | `AND` | left-associative |
| 3 | `NOT` | prefix |
| 4 | `IS [NOT] NULL` | postfix |
| 5 | `= <> < <= > >=` | do not chain: `a = b = c` is an error |
| 6 | `+ -` | left-associative |
| 7 | `* /` | left-associative |
| 8 | `-` | prefix (unary minus) |

Prefix operators parse their operand at their own level: `NOT` calls
`parseExpr(3)`, so `NOT a = b` is `NOT (a = b)` and `NOT a AND b` is
`(NOT a) AND b`; unary minus calls `parseExpr(8)`, so `-a * b` is
`(-a) * b`. `IS NULL` is a postfix operator one level below comparison,
so `a = b IS NULL` is `(a = b) IS NULL`, and it chains, because after
`a IS NULL` is complete there is no conflict in absorbing another `IS`.
Comparisons are `%nonassoc` in the grammar: after building `a = b`, the
parser checks whether another comparison follows and reports a syntax
error if so, which is what bison does with a non-associative conflict.

Worked example. `1 + 2 * 3 - 4` through `parseExpr(0)`. Each row is one
turn of a loop or one entry to a call; `left` is what the active call
holds after combining whatever the previous row returned:

| # | Active call | `left` | Next token | Test | Action |
|---|---|---|---|---|---|
| 1 | `parseExpr(0)` | – | `1` | – | prefix form gives `1` |
| 2 | `parseExpr(0)` | `1` | `+` (6) | 6 ≥ 0 | consume, call `parseExpr(7)` |
| 3 | `parseExpr(7)` | – | `2` | – | prefix form gives `2` |
| 4 | `parseExpr(7)` | `2` | `*` (7) | 7 ≥ 7 | consume, call `parseExpr(8)` |
| 5 | `parseExpr(8)` | – | `3` | – | prefix form gives `3` |
| 6 | `parseExpr(8)` | `3` | `-` (6) | 6 < 8 | return `3` |
| 7 | `parseExpr(7)` | `(2 * 3)` | `-` (6) | 6 < 7 | return `(2 * 3)` |
| 8 | `parseExpr(0)` | `(1 + (2 * 3))` | `-` (6) | 6 ≥ 0 | consume, call `parseExpr(7)` |
| 9 | `parseExpr(7)` | – | `4` | – | prefix form gives `4` |
| 10 | `parseExpr(7)` | `4` | end | – | return `4` |
| 11 | `parseExpr(0)` | `((1 + (2 * 3)) - 4)` | end | – | return |

Rows 6 and 7 are the whole trick. `*` binds inside the right operand of
`+` because it was called at 7 and `*` is 7; `-` does not, because 6 is
below 7, so it is left for the outer call and the tree groups to the
left. Six more expressions, with their renderings:

| Source | Parses as |
|---|---|
| `1 + 2 + 3` | `((1 + 2) + 3)` |
| `NOT a = b` | `(NOT (a = b))` |
| `NOT a AND b` | `((NOT a) AND b)` |
| `-a * b` | `((-a) * b)` |
| `a = b IS NULL` | `((a = b) IS NULL)` |
| `a IS NOT NULL IS NULL` | `((a IS NOT NULL) IS NULL)` |

`NOT` parses its operand at level 3, so it swallows the comparison at 5
but not the `AND` at 2. Unary minus parses at level 8, above `*`, so it
binds tighter. `IS NULL` at level 4 takes a whole comparison and chains
with itself.

Prefix forms: integer, string, `TRUE`, `FALSE`, `NULL`, `ident`,
`ident . ident`, `( expr )`, `- expr`, `NOT expr`. Anything else where an
expression should start is a syntax error with `Expected == "expression"`.
`*` is not an expression: the select-list rule handles it before calling
`parseExpr`, so `SELECT * AS x` and `count(*)` fail.

## Errors

```go
type Error struct {
	Pos      lexer.Pos
	Err      error   // ErrSyntax, ErrUnknownType, or ErrStackDepth
	Expected string  // what the parser needed: "FROM", ")", "expression", ...
	Found    string  // the token it got: `"x"`, `'abc'`, "end of input"
}
```

`Expected` is upper case for keywords (`BY`, `NULL`), the character for
punctuation (`)`, `=`), and a lower-case category otherwise: `statement`,
`expression`, `identifier`, `type name`, `end of statement`,
`end of expression`, `end of input`. `Found` is the token text in double
quotes, a string literal in single quotes, or `end of input`. `Pos` is the
position of the offending token, which is what the REPL will underline.
PostgreSQL says only `syntax error at or near "x"`; naming the expected
token costs one string per call site and makes the tests precise.

Lexical errors are returned as `*lexer.Error` unchanged, so a caller does
one `errors.As` per error kind and `errors.Is` against either package's
sentinels.

Worked example. Seven bad inputs, with the fields the tests pin:

| Source | Column | `Expected` | `Found` |
|---|---|---|---|
| `SELEC 1` | 1 | `statement` | `"SELEC"` |
| `select from t` | 8 | `expression` | `"from"` |
| `select a = b = c` | 14 | `end of expression` | `"="` |
| `select * from` | 14 | `identifier` | `end of input` |
| `select a b c from t` | 12 | `end of statement` | `"c"` |
| `select * as x` | 10 | `end of statement` | `"as"` |
| `create table t (a foo)` | 19 | `type name` | `"foo"` |

Rendered, the first and the last are

```
syntax error at line 1, column 1: expected statement, found "SELEC"
type does not exist at line 1, column 19: expected type name, found "foo"
```

`Found` is `"SELEC"` as the user typed it, not the folded `"selec"` the
lexer produced, so the caret in the REPL lines up with something the
user recognises. The last row is the only one wrapping `ErrUnknownType`
rather than `ErrSyntax`; everything else here is a syntax error. And
`select a b c from t` fails at `c`, not at `b`: `b` was a perfectly good
alias, and only the token after it has nowhere to go.

The parser stops at the first error. There is no recovery, no
synchronisation on `;`, and `Parse` returns `nil` statements with the
error even if earlier statements in the input were fine.

## API

`internal/sql/ast`

```go
type Node interface{ String() string }
type Stmt interface{ Node; stmtNode() }
type Expr interface{ Node; Pos() lexer.Pos; exprNode() }
type TableExpr interface{ Node; tableExpr() }

type CreateTable struct { Name string; Columns []ColumnDef }
type ColumnDef struct { Name string; Type tuple.TypeID; NotNull, PrimaryKey bool }
type DropTable struct { Name string }
type Insert struct { Table string; Columns []string; Rows [][]Expr }
type Select struct { Items []SelectItem; From []TableExpr; Where Expr; OrderBy []OrderItem; Limit Expr }
type SelectItem struct { Expr Expr; Alias string }
type OrderItem struct { Expr Expr; Desc bool }
type TableRef struct { Loc lexer.Pos; Name, Alias string }
type Join struct { Left, Right TableExpr; On Expr }
type Update struct { Table string; Set []Assignment; Where Expr }
type Assignment struct { Column string; Value Expr }
type Delete struct { Table string; Where Expr }
type Begin struct{}
type Commit struct{}
type Rollback struct{}
type Explain struct { Stmt Stmt }

type BinOp int  // Eq Ne Lt Le Gt Ge Add Sub Mul Div And Or
type UnOp int   // Neg Not
func (o BinOp) String() string
func (o UnOp) String() string

type IntLit struct { Loc lexer.Pos; Text string }
type StrLit struct { Loc lexer.Pos; Value string }
type BoolLit struct { Loc lexer.Pos; Value bool }
type NullLit struct { Loc lexer.Pos }
type ColumnRef struct { Loc lexer.Pos; Table, Name string }
type Star struct { Loc lexer.Pos }
type BinaryExpr struct { Loc lexer.Pos; Op BinOp; Left, Right Expr }
type UnaryExpr struct { Loc lexer.Pos; Op UnOp; X Expr }
type IsNull struct { Loc lexer.Pos; X Expr; Not bool }

func QuoteIdent(name string) string
func QuoteString(value string) string
```

`internal/sql/parser`

```go
var ErrSyntax, ErrUnknownType, ErrStackDepth error

const MaxExprDepth = 1000

type Error struct { Pos lexer.Pos; Err error; Expected, Found string }
func (e *Error) Error() string
func (e *Error) Unwrap() error

func Parse(src string) ([]ast.Stmt, error)
func ParseExpr(src string) (ast.Expr, error)
```

`internal/sql/lexer` gains `IsKeyword(word string) bool`, which
`QuoteIdent` uses.

Semantics the tests depend on:

- `String()` output is exactly the canonical form above; the golden tables
  in `parser_test.go` are the reference. Parsing a rendering and rendering
  again gives the same text.
- Precedence and associativity follow the table above, including
  `(a = b) IS NULL`, `((a IS NULL) IS NULL)`, `(NOT (a = b))`,
  `((-a) * b)`, and the error for `a < b < c`.
- `Parse("")` and `Parse(";")` return an empty list and no error. A
  trailing `;` is optional. Leftover tokens after a statement are a syntax
  error with `Expected == "end of statement"` at the first leftover token.
- `ParseExpr` fails with `Expected == "end of input"` if tokens remain.
- Every parse error is a `*Error` unwrapping to `ErrSyntax`,
  `ErrUnknownType`, or `ErrStackDepth`, with `Pos`, `Expected`, and
  `Found` as described. `Error()` contains `expected <Expected>` and
  `found <Found>`, except for `ErrStackDepth`, whose `Expected` and
  `Found` are empty and whose message is `stack depth limit exceeded at
  line L, column C`. Lexical errors are `*lexer.Error`, never wrapped in
  `*Error`.
- An expression nested more than `MaxExprDepth` levels deep is
  `ErrStackDepth` at the token that would exceed it. Nesting is what
  costs a stack frame: a parenthesis, a unary `-` or `NOT`, and the
  right operand of an infix operator. `MaxExprDepth - 2` levels parse.
- `EXPLAIN` accepts only `SELECT`, `INSERT`, `UPDATE`, `DELETE`;
  `Expected` is `SELECT, INSERT, UPDATE, or DELETE` otherwise.
- `Insert.Columns` is `nil` when the list is omitted and non-nil (possibly
  of length one) when present. `Select.From`, `Where`, `OrderBy`, `Limit`
  and the `Where` of `Update` and `Delete` are `nil` when absent.
- Positions: `ColumnRef.Loc` is the first identifier of a qualified name;
  `BinaryExpr.Loc` and `IsNull.Loc` are the operator; `UnaryExpr.Loc` is
  the operator; `TableRef.Loc` is the table name.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** `for i, r := range name` walks a string by
character and gives the byte index of each, which is how `QuoteIdent`
tells a leading digit from a later one. `strings.Builder` assembles the
printed forms; `strings.ReplaceAll` doubles quotes; `strings.ToUpper`
turns a keyword into its `Expected` spelling; `strings.IndexByte` finds
the closing quote when reporting a quoted identifier. A map keyed by
`lexer.Kind` and the comma-ok form `op, ok := table[kind]` make the
operator tables. In `return &ast.IntLit{Loc: tok.Pos}, p.advance()` copy
`p.tok` into `tok` first: Go does not fix the order of a field read and
a call in the same statement.

<details><summary><b><code>QuoteIdent</code>, <code>QuoteString</code>.</b></summary>

A private `isBareIdent` decides: empty or `lexer.IsKeyword` means quoted;
then per character, `a` to `z`, `_`, and any character at or above 0x80 are
always fine, a digit or `$` is fine except at index 0, and anything else
(upper-case ASCII, a space, a quote) forces quoting. Non-ASCII letters pass
because the lexer folds only ASCII, so `Über` lexes back to itself. Quoting
doubles `"` inside; `QuoteString` does the same with `'` and touches
nothing else.
</details>

<details><summary><b>Printer.</b></summary>

`BinOp.String` indexes a `[...]string` table written with `Eq: "=", ...`
keys, with a fallback for values outside it; `UnOp.String` is a switch.
Statement printers write to a `strings.Builder`, join lists with `", "`,
and emit a clause only when its field is non-nil, so a nil `From` prints no
`FROM` and `Insert` prints `(cols)` only when `Columns != nil`. Column
types print via `tuple.TypeID.String()`. `TableRef` prints ` AS alias`
whenever an alias is set, `Join` prints `left JOIN right ON on` with no
parentheses of its own, `UnaryExpr` prints `(NOT x)` with a space and
`(-x)` without.
</details>

<details><summary><b>Parser state and lookahead.</b></summary>

`parser` holds `src`, the `lex`, and `tok`, one token of lookahead.
`newParser` builds the lexer and primes `tok` with one `advance`, so a
lexical error at the first token surfaces before any rule runs. Every
helper returns the lexer's error untouched, which is the whole of the
`*lexer.Error` plumbing: `advance` reads the next token into `tok`; `next`
returns the current token and advances; `isKeyword(word)` tests kind and
lower-cased text; `accept(word)` consumes a keyword if present and reports
whether it did; `expect(word)` fails with `Expected` set to
`strings.ToUpper(word)`; `expectKind(kind)` returns the consumed token or
fails with `Expected` set to `kind.String()`, which is why an identifier is
reported as `identifier` and a missing `)` as `)`; `ident` is
`expectKind(lexer.Ident)` reduced to the text.
</details>

<details><summary><b>Error helper.</b></summary>

`syntaxError(expected)` builds the `*Error` at `p.tok.Pos` with `Found`
from `describe`. `describe` returns `end of input` for EOF,
`ast.QuoteString` of the text for a string, and for a keyword or identifier
re-slices `src` at the token's offset to show the raw spelling: a bare word
is exactly `len(tok.Text)` bytes because folding is ASCII-only, and a
quoted one is scanned from its opening `"` to the closing one, stepping
over `""` pairs, quotes included. Everything else is the text in double
quotes. `ErrUnknownType` is built directly, with `Found` set to the folded
name in double quotes (`"INT4"` for `"INT4"`), not `describe`'s raw form.
</details>

<details><summary><b><code>Parse</code> and <code>parseStmt</code>.</b></summary>

`Parse` loops: skip any run of `;`, return at EOF, call `parseStmt`, then
demand `;` or EOF and otherwise report `end of statement` at the current
token. Return `nil, err` on every error path so no partial list escapes.
`parseStmt` reports `statement` if the token is not a keyword, switches on
its text to one `parseX` per statement, handles `BEGIN`, `COMMIT`, and
`ROLLBACK` inline as an advance plus an empty struct, and reports
`statement` for any other keyword. Each `parseX` starts by `expect`ing its
own first keyword.
</details>

<details><summary><b><code>parseSelect</code>.</b></summary>

Select list: `parseSelectItem` handles a `Star` token before calling
`parseExpr(0)`, then `parseAlias`, which takes `AS` plus a required
identifier, or a bare identifier if one is next, or nothing. `*` never
calls `parseAlias`, which is how `SELECT * AS x` ends at the `as`. Then
`accept("from")` and a comma list of `parseTableExpr`, which parses a
`parseTableRef` (an `expectKind(lexer.Ident)` token for `Loc` and `Name`,
then `parseAlias`) and folds each `JOIN ref ON expr` into a new `Join`
whose `Left` is the tree so far. `parseWhere` (`accept` then
`parseExpr(0)`, nil otherwise) is shared with `UPDATE` and `DELETE`. `ORDER
BY` is `accept("order")`, `expect("by")`, then per item `parseExpr(0)`
followed by `accept("asc")` or, failing that, `accept("desc")` into `Desc`.
`LIMIT` is `accept` then `parseExpr(0)`.
</details>

<details><summary><b><code>parseInsert</code>, <code>parseUpdate</code>, <code>parseDelete</code>.</b></summary>

Take the table name with `expectKind(lexer.Ident)` so its `Pos` fills
`Loc`. In `INSERT`, on a `(` set `Columns` to an empty non-nil slice, then
loop: record `p.tok.Pos` before calling `ident` (afterwards `tok` is
already the comma), append name and position, continue on `,`, finish with
`)`. `VALUES` rows are `(`, `parseExprList` (comma-separated
`parseExpr(0)`), `)`, repeated on `,`. `SET` assignments are the same
record-then-`ident` pattern, then `expectKind(lexer.Eq)` and
`parseExpr(0)`, and `UPDATE` ends with `parseWhere`. `DELETE` is
`expect("from")`, the table token, `parseWhere`.
</details>

<details><summary><b><code>parseCreateTable</code>, <code>parseColumnDef</code>.</b></summary>

After `CREATE TABLE ident (`, loop `parseColumnDef` on `,` and finish with
`expectKind(lexer.RParen)`, which is what reports `)` for `unique`. A
column def is an `ident`, then a token that must be of kind `Ident` (`type
name` otherwise), looked up in `typeNames`; a miss builds the
`ErrUnknownType` error at that token. Then loop: `not` demands `null` and
sets `NotNull`, `primary` demands `key` and sets `PrimaryKey`, anything
else ends the definition, so the two constraints come in either order. A
table-level `primary key (a)` fails as `identifier` because after a comma a
column name is expected.
</details>

<details><summary><b><code>parseExplain</code>.</b></summary>

`expect("explain")`, then check that the current token is one of the four
allowed keywords and report the combined `Expected` string otherwise, so
`EXPLAIN EXPLAIN` and `EXPLAIN CREATE` fail at the second word; then
recurse into `parseStmt` and wrap.
</details>

<details><summary><b>Operator tables.</b></summary>

A `binOp{op, prec}` pair, a `binOpTokens map[lexer.Kind]binOp` for the
punctuation operators, a `binOpKeywords map[string]binOp` for `AND` and
`OR`, and the `precX` constants from the skeleton. `infixOp()` consults the
keyword map when `tok` is a keyword and the kind map otherwise, returning
the pair and whether the current token is an operator at all.
</details>

<details><summary><b><code>parseExpr</code>, <code>parseIsNull</code>, <code>parsePrefix</code>.</b></summary>

The loop in the Expressions section, with these details: check `IS` before
`infixOp`, returning if `precIs < minPrec`, else `parseIsNull(left)`, which
takes the `IS` token with `next` (its `Pos` is the `IsNull.Loc`),
`accept("not")`, `expect("null")`. For an infix operator take its token
with `next` so the position lands in `BinaryExpr.Loc`, and after building a
comparison report `end of expression` if `infixOp` shows another one.
Callers pass `minPrec` 0, below `precOr`. `parseExpr` is also where the
depth limit goes: a counter on the parser, incremented on entry and
decremented on the way out, and `ErrStackDepth` at the current token once
it passes `MaxExprDepth`. Every recursive descent into an expression goes
through `parseExpr`, so one counter covers parentheses, unary operators,
and right operands alike; and since the analyzer and `ast.String` recurse
over the same tree with no limit of their own, capping the tree here is
what keeps them off the stack's edge too. Without it a long enough run of
`(` ends the process: a Go stack overflow is a fatal error, not a panic,
and no `recover` sees it. `parsePrefix` switches on the
token kind: `Integer`, `String`, `Ident` (then a `.` plus `ident` makes a
qualified `ColumnRef` located at the first name, which is where `a.1`
reports `identifier`), `LParen` (`parseExpr(0)`,
`expectKind(lexer.RParen)`, return the inner node; there is no paren node),
`Minus`, and the keywords `true`, `false`, `null`, `not`; anything else is
`expression`. `1.5` needs no special case: `.` is not an infix operator, so
the loop returns and `Parse` reports `end of statement` at the dot.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch07` at the end.
Every test not named in that step or an earlier one still panics. Steps 4
to 7 all sit behind the same statement dispatcher; each adds one
statement area.

1. Quoting. `QuoteIdent`, `QuoteString`. Green: `TestQuoteIdent`,
   `TestQuoteString`. The per-character rule is under Implementation
   notes.

   ```sh
   go test -race ./internal/sql/ast/... -run 'TestQuoteIdent|TestQuoteString'
   ```
2. Printer. `String` on `BinOp` and `UnOp`, then on the nine expression
   nodes (`IntLit` through `IsNull`) and the twelve statement and table
   nodes (`CreateTable` through `Explain`). Green: `TestOpStrings`,
   `TestString`. Every operator application is parenthesised, `DESC` is
   printed and `ASC` is not, no trailing semicolon; the tests build trees
   by hand, so the printer is judged before the parser exists.

   ```sh
   go test -race ./internal/sql/ast/... -run 'TestOpStrings|TestString'
   ```
3. Expressions. `ParseExpr` and the Pratt loop behind it: token
   lookahead, the `*Error` and `*lexer.Error` plumbing, prefix forms
   (literals, `ident`, `ident . ident`, parentheses, `-`, `NOT`), the
   binary levels, postfix `IS [NOT] NULL`, and the "end of expression"
   error for a chained comparison. Green: `TestExprGolden`,
   `TestParseExprRequiresWholeInput`. The private structure is under
   Implementation notes.

   ```sh
   go test -race ./internal/sql/parser/... -run 'TestExprGolden|TestParseExprRequiresWholeInput'
   ```
4. Statement loop and SELECT. `Parse` with the `;` loop that skips empty
   statements, the dispatch on the first keyword, `SELECT` (select list
   with `*` and aliases, `FROM` with aliases and left-nested `JOIN ... ON`,
   `WHERE`, `ORDER BY`, `LIMIT`), and `BEGIN`, `COMMIT`, `ROLLBACK`.
   Green: `TestTree`, `TestPositions`, `TestLexerErrorsPassThrough`.
   How `SELECT * AS x` and `count(*)` end up as "end of statement" is
   under Implementation notes.

   ```sh
   go test -race ./internal/sql/parser/... -run 'TestTree|TestPositions|TestLexerErrorsPassThrough'
   ```
5. INSERT, UPDATE, DELETE. Multi-row `VALUES`, the optional column list
   (`nil` when omitted, with `ColumnLocs` alongside), `SET` assignments
   with their positions, optional `WHERE`. Green: `TestInsertColumns`,
   `TestOptionalClausesAreNil`, `TestTableNamePositions`,
   `TestColumnPositions`, `TestMultipleStatements`.

   ```sh
   go test -race ./internal/sql/parser/... -run 'TestInsertColumns|TestOptionalClausesAreNil|TestTableNamePositions|TestColumnPositions|TestMultipleStatements'
   ```
6. CREATE TABLE, DROP TABLE. Column definitions with `NOT NULL` and
   `PRIMARY KEY` in either order, type names looked up in `typeNames`
   with `ErrUnknownType` at the type name otherwise. Green:
   `TestUnknownType`.

   ```sh
   go test -race ./internal/sql/parser/... -run 'TestUnknownType'
   ```
7. EXPLAIN and the whole contract. `EXPLAIN` accepts only `SELECT`,
   `INSERT`, `UPDATE`, `DELETE`. Green: `TestGolden`, `TestRoundTrip`,
   `TestSyntaxErrors`, `TestExprDepthLimit`, and `FuzzParse`, which
   re-parses every rendering and checks error positions stay inside the
   input; it is the real judge of steps 2 to 7. The syntax-error table
   pins `Pos`, `Expected`, and `Found` for 70 inputs, and `Found` is the
   token as written (`"SELEC"`, not `"selec"`).

   ```sh
   go test -race ./internal/sql/parser/... -run 'TestGolden|TestRoundTrip|TestSyntaxErrors|TestExprDepthLimit|FuzzParse'
   ```

To fuzz for longer than the seed corpus:

```sh
go test -run=NONE -fuzz=FuzzParse -fuzztime=30s ./internal/sql/parser/
```

### When a test fails

- `TestString` — `SELECT a + 1` comes back without parentheses, or with
  one pair too many. Every operator application is parenthesised and
  nothing else is, so `((a + 1) = b)` has exactly two pairs and a bare
  `ColumnRef` has none. The rule exists so a precedence bug shows up as a
  wrong tree rather than as an identical string.
- `TestString` — `ORDER BY x ASC` prints `ASC`. `ASC` is the default and
  is not printed; only `DESC` is. Otherwise the re-parse of the rendering
  produces a tree the original comparison does not match.
- `TestQuoteIdent` — `"Users"` loses its quotes, or `users` gains them.
  The test is whether the bare name would lex back to the same string:
  any upper-case ASCII letter, space, leading digit, or keyword needs
  quoting, and a non-ASCII letter does not, because the lexer does not
  fold it.
- `TestExprGolden` — `1 + 2 + 3` comes out as `(1 + (2 + 3))`. The right
  operand is parsed at `prec + 1`, not at `prec`; parsing at the same
  level makes every left-associative operator right-associative.
- `TestExprGolden` — `NOT a AND b` comes out as `(NOT (a AND b))`. A
  prefix operator parses its operand at its *own* level, so `NOT` calls
  `parseExpr(3)` and `AND` at 2 cannot bind inside it.
- `TestExprGolden` — `-a * b` comes out as `(-(a * b))`. Unary minus is
  level 8, above `*`; it is not the same level as binary minus.
- `TestExprGolden` — `a = b = c` builds a tree instead of failing. After
  a comparison is built, look at the next token and reject another
  comparison with `Expected` "end of expression". That is what bison
  does with `%nonassoc`.
- `TestParseExprRequiresWholeInput` — `1 + 2 junk` returns `1 + 2` and no
  error. `ParseExpr` must reach end of input, not merely stop making
  progress.
- `TestPositions` — a `BinaryExpr` reports the position of its left
  operand. `Loc` for a binary expression and for `IS NULL` is the
  operator, not the start of the expression, because that is where the
  analyzer's "operator does not exist" belongs.
- `TestSyntaxErrors` — `Found` is `"selec"` rather than `"SELEC"`. The
  token's `Text` is folded; the source slice at the token's offset is
  not. Report what the user typed.
- `TestSyntaxErrors` — `select a b c from t` is reported at `b`. The
  alias is legal; the error is the token after the item, so the select
  list has to finish accepting the alias before it complains.
- `TestSyntaxErrors` — an error inside `EXPLAIN CREATE TABLE ...` names
  the wrong token. `EXPLAIN` accepts only the four data statements, and
  the check belongs where the inner statement is dispatched.
- `TestExprDepthLimit` — the test process dies with `fatal error: stack
  overflow` instead of reporting an error. The counter has to be checked
  on the way *in* to `parseExpr`, before it recurses; a check after the
  recursive call never runs.
- `TestUnknownType` — the error is `ErrSyntax`. An unrecognised type name
  is `ErrUnknownType` at the type name, and the type name is an
  identifier, not a keyword (D17), so the token itself lexes fine.
- `TestMultipleStatements` — `select 1;;select 2;` returns three
  statements or fails on the empty one. Skip empty statements in the `;`
  loop; a trailing `;` is not a statement either.
- `TestLexerErrorsPassThrough` — a lexical error arrives wrapped in a
  `*parser.Error`. Return `*lexer.Error` unchanged, so a caller does one
  `errors.As` per package.
- `FuzzParse` — a rendering re-parses to a different rendering. The
  printer and the parser disagree about precedence somewhere; print the
  two strings and the first differing operator to find which level.
- `FuzzParse` — an error position outside the input. Every `*Error` is
  built at a token's own position, so the only way out of range is
  reporting at an offset computed by hand rather than taken from a
  token.

## Why not compare parse trees

The golden tests compare the SQL that `String()` prints, and every golden
case is parsed again from that text. The obvious alternative, comparing the
tree against a composite literal, makes a precedence case three nested
struct literals instead of `(a OR (b AND c))`, and leaves the printer
untested, which matters because chapter 11's `EXPLAIN` and chapter 14's cost
output are built on it (D18). The price is that a printer bug and a parser
bug can cancel out; the round trip is what catches the pair, since a wrong
rendering that parses back to the same tree still has to render the same
text twice.

## Out of scope

Subqueries, aggregates and function calls, `GROUP BY`, `HAVING`,
`DISTINCT`, `OFFSET`, `LIMIT ALL`, outer and cross joins, `USING`,
table-level constraints (`PRIMARY KEY (a, b)`), `DEFAULT`, `IF EXISTS`,
`INSERT ... SELECT`, `RETURNING`, `CASE`, `BETWEEN`, `IN`, `LIKE`,
`IS TRUE`, casts, `t.*`, positional `ORDER BY 1` (parsed as an integer
literal; the analyzer decides), `CREATE INDEX` (chapter 13), `ANALYZE`
(chapter 14), and error recovery.

## Check your understanding

1. What does `a + b * c = d - e` parse to, fully parenthesised?
   <details><summary>Answer</summary>

   `((a + (b * c)) = (d - e))`. `*` at 7 binds tighter than `+` at 6,
   and both bind tighter than `=` at 5, so the comparison is built last
   and each side is complete before it is.
   </details>
2. Where does `select a, from t` fail, with what `Expected` and `Found`?
   <details><summary>Answer</summary>

   At `from`, column 11, with `Expected` `expression` and `Found`
   `"from"`. The comma commits the select list to another item, and
   `from` is a reserved keyword, so it can be neither an expression nor
   an alias. Note `Found` shows the source spelling.
   </details>
3. Why does `Expr` carry `Loc` at the operator for `BinaryExpr` and at
   the first token for everything else?
   <details><summary>Answer</summary>

   Because the position is a place to point an error at, and the errors
   that a binary expression can raise are about the operator: "operator
   does not exist: text >= integer" in chapter 09 is a statement about
   the `>=`, not about the left operand. PostgreSQL stores the same thing
   in `A_Expr.location`. For a `ColumnRef` the interesting error is
   "column does not exist", which belongs at the name.
   </details>
4. Why do the AST node types live in their own package rather than in the
   parser?
   <details><summary>Answer</summary>

   So the analyzer, the planner and chapter 11's `\d` can depend on the
   tree without depending on the parser. In Go that is not just tidiness:
   an import cycle is a compile error, and the parser will eventually
   want to consult things the analyzer defines. PostgreSQL splits
   `nodes/` from `parser/` for the same reason.
   </details>
5. PostgreSQL's grammar is one bison file with a single `A_Expr` node
   carrying an operator name string, where we hand-write a Pratt parser
   with one Go type per expression kind. What does each choice buy?
   <details><summary>Answer</summary>

   Bison checks the grammar for ambiguity at build time and reports
   conflicts, which is worth a great deal when the grammar has hundreds
   of productions and dozens of `%prec` declarations; a hand-written
   parser silently resolves ambiguity in whatever way the code happens
   to. A generic `A_Expr` is what lets PostgreSQL support user-defined
   operators at all, since the operator is not known until the analyzer
   looks it up in `pg_operator`. We have a closed operator set and about
   a dozen node types, so a type switch in the analyzer is clearer than a
   string switch, and the standard-library-only rule (D1) rules out a
   parser generator anyway.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **More expression syntax.** Add `BETWEEN`, `IN (list)` and `CASE`. Each
   one is a new node, a `String()` case, a golden test and a round trip;
   `BETWEEN` is the interesting one, because PostgreSQL rewrites it and the
   printer has to decide whether to show the rewrite.
2. **Error recovery.** Report more than one syntax error per statement by
   skipping to the next `,` or `;` and carrying on. Decide first what a
   second error is worth when the first one already made the tree wrong.
3. **Read the precedence declarations at the top of `gram.y`.** Map each
   `%left` and `%nonassoc` line to a binding power in our Pratt loop, and
   find the one place where bison's table says something our two numbers
   cannot.
