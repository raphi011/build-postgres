# Chapter 06: Lexer

**Goal.** Turn SQL text into a stream of tokens: keywords, identifiers,
integer and string literals, and operators, each tagged with the byte
offset, line, and column it came from, with whitespace and comments
dropped.

**You edit.** `internal/sql/lexer/lexer.go`: 2 functions with
`panic("not implemented")` bodies, `New` and `Next`. The token kinds,
the `keywords` table, the error sentinels, `Kind.String`, `Tokenize`, and
`IsKeyword` are already written. Nothing else changes; `lexer_test.go` is
the contract.

**Needs from earlier chapters.** Nothing. The package imports only the
standard library.

**Done when.** `make test-ch06` passes.

**Effort.** Medium, about 3 hours. Two stubs, but `Next` is a scanner with
every token kind in it; the position bookkeeping set up in step 1 is what
the error tests measure.

Where this chapter sits in the whole, from chapter 00. The box in
brackets is this one.

```
  SQL text
    │
    ▼
  Lexer [06] ──► Parser (07) ──► Analyzer (09) ──► Planner (14, 15)
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

Part 2 turns SQL text into something the executor can run. The first step
is the lexer: it reads the query string once, left to right, and produces a
stream of tokens. Keywords, identifiers, literals, and operators come out
the other side with the whitespace and comments gone and each token tagged
with where it came from, so every later error message can point at the
right place in the input.

PostgreSQL source: `parser/scan.l` (the flex scanner), `parser/kwlist.h`
and `common/keywords.c` (the keyword table), `parser/scansup.c`
(`downcase_identifier`), `include/parser/kwlist.h`.

## What a token is

```go
type Token struct {
	Kind Kind
	Text string
	Pos  Pos
}
```

`Kind` says what the token is. `Text` carries the part the parser needs:
the folded name of an identifier, the lower-cased keyword, the digits of
an integer, the unescaped contents of a string, or the operator itself.
`Pos` is where the token starts: a 0-based byte offset into the source
plus a 1-based line and column, with the column counted in characters,
not bytes, because that is what a terminal shows.

The kinds:

| Kind | Text | Example |
|---|---|---|
| `EOF` | `""` | end of input |
| `Ident` | folded name | `users`, `"Users"` |
| `Keyword` | lower-cased keyword | `SELECT`, `select` |
| `Integer` | the digits | `42` |
| `String` | unescaped value | `'it''s'` |
| `Eq` `Ne` `Lt` `Le` `Gt` `Ge` | the operator | `=` `<>` `<` `<=` `>` `>=` |
| `Plus` `Minus` `Star` `Slash` | the operator | `+` `-` `*` `/` |
| `LParen` `RParen` `Comma` `Semicolon` `Dot` | the character | `(` `)` `,` `;` `.` |

Worked example. `SELECT "Name", id FROM users WHERE id >= 10; -- go`
followed by a newline lexes to twelve tokens:

| Kind | Text | Offset | Line | Column |
|---|---|---|---|---|
| `keyword` | `select` | 0 | 1 | 1 |
| `identifier` | `Name` | 7 | 1 | 8 |
| `,` | `,` | 13 | 1 | 14 |
| `identifier` | `id` | 15 | 1 | 16 |
| `keyword` | `from` | 18 | 1 | 19 |
| `identifier` | `users` | 23 | 1 | 24 |
| `keyword` | `where` | 29 | 1 | 30 |
| `identifier` | `id` | 35 | 1 | 36 |
| `>=` | `>=` | 38 | 1 | 39 |
| `integer` | `10` | 41 | 1 | 42 |
| `;` | `;` | 43 | 1 | 44 |
| `end of input` | `` | 51 | 2 | 1 |

`SELECT` arrives as `Text` `select`, folded; `"Name"` arrives as `Name`,
not folded, and as an identifier rather than a keyword. `>=` is one
token. The comment produces nothing, but the newline that ends it is
still consumed, which is why the `EOF` token is on line 2. `Text` is
empty at `EOF` and every position is the *start* of its token.

Keywords share one kind and are told apart by `Text`. Operators get one
kind each. The split is deliberate: the operator set is fixed by the
expression grammar and the parser switches on it structurally, while the
keyword list grows in later chapters (`INDEX` and `UNIQUE` arrive in
chapter 13, `ANALYZE` in chapter 14), and a growing list is easier to
maintain as a table than as an enum. PostgreSQL gives every keyword its
own token number generated from `kwlist.h`; the effect is the same.

## Identifiers and keywords

**Predict.** Of `SeLeCt`, `"select"`, `Straße` and `Über`, which are
keywords, and what is each one's `Text`?

An identifier starts with a letter, an underscore, or any byte outside
ASCII, and continues with those plus digits and `$`. PostgreSQL's
`downcase_identifier` folds only ASCII letters, so `Straße` becomes
`straße` and `Über` stays `Über`. After folding, the word is looked up in
the keyword table; a hit is a `Keyword`, a miss is an `Ident`.

A double-quoted identifier is never a keyword and is not folded: `"Users"`
and `users` are different names, and `"select"` is a column called
`select`. Two double quotes inside stand for one. An empty `""` is an
error, as in PostgreSQL ("zero-length delimited identifier").

Worked example. Six inputs, each lexed on its own:

| Source | Kind | Text |
|---|---|---|
| `SeLeCt` | `keyword` | `select` |
| `"select"` | `identifier` | `select` |
| `Straße` | `identifier` | `straße` |
| `Über` | `identifier` | `Über` |
| `"Users"` | `identifier` | `Users` |
| `a$b` | `identifier` | `a$b` |

`SeLeCt` and `"select"` end with the same `Text` and different kinds,
which is the entire point of quoting. `Straße` folds its `S` and leaves
`ß` alone; `Über` keeps its `Ü` because folding touches only the ASCII
range, so the two words are folded inconsistently on purpose. That is
`downcase_identifier`'s behaviour, not an oversight, and
`TestIdentifierFolding` pins it.

Every keyword in this book is reserved. PostgreSQL distinguishes four
classes so that `key` or `level` can still name a column; we do not, and
the keyword list is kept short to compensate. The list is the `keywords`
table in `lexer.go`.

## Literals

Integers are a run of decimal digits. The lexer does not convert them:
whether `3000000000` is an `int8` or too large is the analyzer's decision
(chapter 09), which is also how PostgreSQL decides between `int4`, `int8`,
and `numeric` for a literal. A digit run followed directly by an
identifier character (`123abc`) is an error, matching the "trailing junk
after numeric literal" check added in PostgreSQL 15. There are no float
literals; `1.5` lexes as `Integer`, `Dot`, `Integer` and the parser rejects
it.

Strings are single-quoted, with `''` standing for one quote. There are no
backslash escapes, no `E'...'` strings, and no dollar quoting. A string that
reaches end of input without its closing quote is an error reported at the
opening quote.

Worked example:

| Source | Result |
|---|---|
| `42` | `Integer` with text `42` |
| `123abc` | `ErrTrailingJunk` at the `1` |
| `1_000` | `ErrTrailingJunk` at the `1`; `_` starts an identifier |
| `1.5` | `Integer` `1`, `Dot`, `Integer` `5` |
| `'it''s'` | `String` with text `it's` |
| `''` | `String` with text `` |
| `'abc` | `ErrUnterminatedString` at the opening quote |

The text of a `String` is the unescaped value, so the doubled quote is
already gone by the time the parser sees it, and nothing distinguishes
`'it''s'` from a string that contained one quote some other way.

## Operators and punctuation

`= <> < <= > >= + - * / ( ) , ; .` and `!=`, which PostgreSQL's scanner
rewrites to `<>` before the parser sees it; we do the same, so `!=`
produces `Ne` with text `<>`. Matching is greedy: `<=` is one token, never
`<` then `=`. Any other character outside a string or comment is an error.

Worked example. `1 != 2` lexes to `Integer` `1`, then a token of kind
`Ne` whose text is `<>` at column 3, then `Integer` `2`. The text is the
kind's own name rather than the two source bytes, so a parser error
message says `<>` however the user spelled it. `!` on its own is not an
operator, so `!x` reaches the fallthrough and is `ErrBadChar` at the `!`.

## Whitespace and comments

Whitespace is space, tab, newline, carriage return, form feed, and vertical
tab. `--` starts a comment that runs to the end of the line. `/*` starts a
block comment that runs to the matching `*/`, and block comments nest, as
the SQL standard requires and PostgreSQL implements: `/* a /* b */ c */`
is one comment. An unclosed block comment is an error reported at its
opening `/*`.

Worked example. `select\n  x\tfrom t`, where the second line begins with
two spaces and has a tab between `x` and `from`:

| Kind | Text | Offset | Line | Column |
|---|---|---|---|---|
| `keyword` | `select` | 0 | 1 | 1 |
| `identifier` | `x` | 9 | 2 | 3 |
| `keyword` | `from` | 11 | 2 | 5 |
| `identifier` | `t` | 16 | 2 | 10 |
| `end of input` | `` | 17 | 2 | 11 |

The offset counts every byte including the newline and the tab; the
column restarts at 1 on the new line and counts the tab as one column, so
`from` is at column 5 and not at a tab stop. Because `/* a /* b */` opens
two comments and closes one, it is `ErrUnterminatedComment` reported at
offset 0, the *outer* `/*`, not the inner one.

## Errors

```go
type Error struct {
	Pos Pos
	Err error
}
```

`Err` is one of the sentinel errors below, and `Error` unwraps to it so
callers use `errors.Is`. `Pos` is where the offending token starts, which
is what the REPL underlines in chapter 11. Once `Next` has returned an
error, every further call returns the same error.

Worked example. Five bad inputs and what `Tokenize` returns for each. In
every case the token slice is `nil`, not a partial list:

| Source | Message | Position |
|---|---|---|
| `SELECT 'it''s ok` | `unterminated quoted string at line 1, column 8` | the opening `'` |
| `SELECT 123abc` | `trailing junk after numeric literal at line 1, column 8` | the `1` |
| `SELECT ""` | `zero-length delimited identifier at line 1, column 8` | the opening `"` |
| `/* a /* b */` | `unterminated /* comment at line 1, column 1` | the outer `/*` |
| `SELECT #` | `unexpected character at line 1, column 8` | the `#` |

The first case is worth reading twice. `'it''s ok` is not a string
containing `it`, followed by junk: the `''` is an escaped quote, so the
string is still open at end of input and the error points at the quote
that opened it, column 8, not at where the scanner gave up.

## API

`internal/sql/lexer`

```go
type Kind int
func (k Kind) String() string

type Pos struct{ Offset, Line, Column int }
type Token struct { Kind Kind; Text string; Pos Pos }

type Error struct { Pos Pos; Err error }
func (e *Error) Error() string
func (e *Error) Unwrap() error

var (
	ErrUnterminatedString  error
	ErrUnterminatedIdent   error
	ErrUnterminatedComment error
	ErrEmptyIdent          error
	ErrTrailingJunk        error
	ErrBadChar             error
)

type Lexer struct{ ... }
func New(src string) *Lexer
func (l *Lexer) Next() (Token, error)
func Tokenize(src string) ([]Token, error)
```

Semantics the tests depend on:

- `Next` skips whitespace and comments, then returns the next token. At end
  of input it returns an `EOF` token whose `Pos` is the end of the source,
  and keeps returning it.
- `Tokenize` calls `Next` until `EOF` and returns every token including
  the final `EOF`. On error it returns `nil` and the error.
- Every error is a `*Error` wrapping one of the sentinels. Positions:
  unterminated string and identifier at the opening quote, unterminated
  comment at the `/*`, empty identifier at its opening quote, trailing junk
  at the first digit, bad character at the character.
- `Pos.Line` and `Pos.Column` start at 1. Column counts characters (Unicode
  code points), a tab is one column, and both `\n` and `\r\n` end a line.
  `Pos.Offset` is the byte offset.
- Unquoted identifiers fold ASCII letters to lower case and nothing else.
  Quoted identifiers are returned as written with `""` collapsed to `"`.
- Keywords are matched after folding, so `SeLeCt` is the keyword `select`.
  A quoted `"select"` is an identifier.
- `!=` is returned as `Ne` with text `<>`.

## Implementation notes

From this chapter on the per-function sketches are folded away. Work from
the design sections, the API and the tests first, and open a sketch when
you want to compare your plan with the reference one or when a test has you
stuck.

**Go you will need.** Indexing a string (`src[i]`) yields a byte, not a
character. `utf8.RuneSelf` (0x80) separates ASCII from the lead byte of a
multi-byte character, and `utf8.DecodeRuneInString` returns the width of
the character at the front of a string (1 for an invalid byte, so `"\xff"`
still advances and cannot loop). Slicing by byte offset (`src[a:b]`) gives
token text without copying; `strings.Builder` collects the unescaped
contents of a quoted literal; `[]byte(s)` copies, so folding in place and
converting back is safe.

<details><summary><b>Scanner state (<code>New</code>).</b></summary>

The `Lexer` fields are the source, the byte offset `off`, the 1-based
`line` and `col`, and the sticky `err`. `New` stores the source and sets
line and column to 1. Everything else in the solution rests on four private
helpers:

- `pos()` packs `off`, `line`, `col` into a `Pos`. Call it before
  consuming a token so the token's `Pos` is its start.
- `peek(n)` returns the byte at `off+n`, or 0 past the end, so two-byte
  lookahead never bounds-checks. Because a real NUL byte also reads as 0,
  test `off >= len(src)` for end of input rather than `peek(0) == 0`;
  `"\x00"` in the source must reach the `ErrBadChar` branch.
- `advance(n)` consumes n bytes and is the only code that touches `line`
  and `col`: `\n` bumps the line and resets the column to 1, any other
  ASCII byte bumps the column by one, a non-ASCII lead byte decodes the
  whole character and bumps the column once. `\r` is counted like any
  other byte and the `\n` after it resets the column, which is all `\r\n`
  needs. Callers only pass an n that ends on a character boundary.
- `fail(at, sentinel)` wraps the sentinel in an `*Error` at `at`, stores
  it in `err`, and returns it, so one call both reports the error and arms
  the sticky return.
</details>

<details><summary><b><code>Next</code> dispatch.</b></summary>

In order: return `err` if set; skip whitespace and comments (this can fail
too); take `start := pos()`; at end of input return `EOF` at `start`, and
since nothing advanced, every later call returns the same token; otherwise
switch on the first byte: digit, `'`, `"`, identifier start, then the
operator table, then `ErrBadChar` at `start`. Each branch is a private
method taking `start` and returning `(Token, error)`. Check digits before
identifier starts.
</details>

<details><summary><b>Operators.</b></summary>

A pure function `operator(c, c2) (Kind, int)` returns the kind and byte
length of the longest operator at the two lookahead bytes, or `EOF, 0` for
none. Try the two-byte forms `<=`, `<>`, `>=`, `!=` before the one-byte
ones. `!` alone is nothing, so `!x` falls through to `ErrBadChar`. The
token text is `Kind.String()`, which is what turns `!=` into `<>`.
</details>

<details><summary><b>Integers.</b></summary>

Advance over digits, then look at the next byte without consuming it: an
identifier start means `ErrTrailingJunk` at `start`. `_` is an identifier
start, so `1_000` is junk; `$` is not. The text is `src[start.Offset:off]`.
</details>

<details><summary><b>Identifiers and keywords.</b></summary>

`ident` advances while `isIdentChar`, by whole characters (decode the width
when the byte is non-ASCII). Slice the source, fold it with `downcase`,
which copies to a `[]byte` and rewrites only `A` to `Z`, and look the
result up in `keywords`: `Keyword` on a hit, `Ident` otherwise.
`isIdentStart` accepts `_`, ASCII letters, and any byte at or above
`utf8.RuneSelf`; `isIdentChar` adds digits and `$`.
</details>

<details><summary><b>Quoted literals.</b></summary>

One helper `quoted(q)` serves both `'` and `"`: consume the opening quote,
then loop copying whole characters into a `strings.Builder` until a `q`;
`q` followed by another `q` writes one `q` and consumes both; a lone `q`
closes and returns the contents. Running off the end returns not-ok and the
caller reports `ErrUnterminatedString` or `ErrUnterminatedIdent` at
`start`, the opening quote. `quotedIdent` also fails with `ErrEmptyIdent`
when the contents are empty; `str` returns them as they are, so `''` is an
empty `String`. The escape check comes first, so `'a''` is unterminated.
</details>

<details><summary><b>Whitespace and comments.</b></summary>

`skipSpace` loops until a byte that is none of these: whitespace advances
one byte; `--` advances up to, not past, the `\n`, so the newline still
goes through `advance` and bumps the line; `/*` records `pos()`, consumes
two bytes, and runs a depth counter where `/*` adds one, `*/` subtracts
one, anything else advances one byte, and end of input with depth above
zero is `ErrUnterminatedComment` at the recorded position, which is the
outermost `/*`. Arm the error with `fail` here as well so the sticky rule
holds for comment errors.
</details>

## Suggested order

One step at a time with the line under it; `make test-ch06` at the end.
Every test not named in that step or an earlier one still panics. Only
two functions are stubbed, so steps 2 to 4 add branches to the switch you
write in step 1 rather than new functions.

1. Skeleton and positions. `New`, `Next`: the scanner state, position
   tracking, and sticky error are sketched under "Scanner state" and
   "`Next` dispatch" above; at this step skip whitespace, return `EOF` at
   the end of input on every call, and turn any other character into
   `ErrBadChar`. Green: `TestKindString`, which only exercises the given
   `Kind.String` and is green before you start.

   ```sh
   go test -race ./internal/sql/lexer/... -run 'TestKindString'
   ```
2. Operators and integers. Greedy two-character operators (`<=` before
   `<`), `!=` returned as `Ne` with text `<>`, digit runs with
   `ErrTrailingJunk` when an identifier character follows. Green:
   `TestNextAfterEOF`, `TestErrorIsSticky`.

   ```sh
   go test -race ./internal/sql/lexer/... -run 'TestNextAfterEOF|TestErrorIsSticky'
   ```
3. Identifiers and keywords. Unquoted names start with a letter, `_`, or
   any non-ASCII byte and fold only ASCII letters; look the folded word up
   in `keywords`. Quoted names collapse `""` to `"`, are never keywords,
   and raise `ErrEmptyIdent` or `ErrUnterminatedIdent`. Green:
   `TestAllKeywords`, `TestIdentifierFolding`.

   ```sh
   go test -race ./internal/sql/lexer/... -run 'TestAllKeywords|TestIdentifierFolding'
   ```
4. Strings and comments. Single-quoted strings with `''` for one quote and
   `ErrUnterminatedString` at the opening quote; `--` to end of line;
   nested `/* */` with `ErrUnterminatedComment` at the outermost `/*`.
   Green: `TestTokens`, `TestPositions`, `TestEOFPosition`, `TestErrors`,
   `FuzzTokenize`. The error table also checks that `Tokenize` returns a
   nil slice on error and that the message contains the sentinel text; the
   fuzz test runs its seed corpus under plain `go test` and must not panic
   on invalid UTF-8 such as `"\xff"`.

   ```sh
   go test -race ./internal/sql/lexer/... -run 'TestTokens|TestPositions|TestEOFPosition|TestErrors|FuzzTokenize'
   ```

To fuzz for longer than the seed corpus:

```sh
go test ./internal/sql/lexer/... -run '^$' -fuzz FuzzTokenize -fuzztime 60s
```

### When a test fails

- `TestPositions` — columns are right on line 1 and wrong afterwards, or
  a token's position is the character *after* it. Take `pos()` before
  consuming anything, and make `advance` the only code that touches
  `line` and `col`, so no branch can forget to update them.
- `TestPositions` — a multi-byte character counts as two or three
  columns. Columns count characters, so a non-ASCII lead byte has to be
  decoded and the whole character consumed as one column, while `Offset`
  still counts bytes.
- `TestEOFPosition` — the `EOF` position is the start of the trailing
  comment rather than the end of the input. Whitespace and comments are
  skipped *before* `start := pos()` is taken, so the newline that ends a
  `--` comment has already advanced the line.
- `TestNextAfterEOF` — the second call past the end returns a different
  token or an error. At end of input nothing advances, so returning
  `EOF` at the current position is naturally idempotent; do not consume
  a sentinel byte.
- `TestErrorIsSticky` — the call after an error returns a real token.
  One `fail` helper both builds the `*Error` and stores it, and `Next`
  returns the stored error before doing anything else. Comment errors go
  through the same helper.
- `TestErrors` — `errors.Is` fails against the sentinel although the
  message is right. `*Error` must implement `Unwrap` returning `Err`, and
  the sentinel has to be stored there rather than formatted into a
  string.
- `TestErrors` — `"\x00"` in the source is treated as end of input. A NUL
  byte reads the same as the past-the-end value from `peek`, so test
  `off >= len(src)` for the end and let a real NUL reach `ErrBadChar`.
- `TestErrors` — the unterminated comment is reported at the inner `/*`.
  Record the position once, when the depth goes from zero to one, and
  report that.
- `TestErrors` — `'a''` is reported as a complete string rather than an
  unterminated one. Check for the doubled quote before treating a quote
  as the closer.
- `TestTokens` — `!=` comes back with text `!=`. The token text for an
  operator is `Kind.String()`, which is what performs the rewrite.
- `TestTokens` — `1.5` is one token or an error. There are no float
  literals; the digit run stops at the `.`, which is a `Dot`.
- `TestAllKeywords` — a keyword is returned as an identifier. Fold first,
  then look up: the table holds lower-case words only.
- `TestIdentifierFolding` — `Über` is folded to `über`.
  `strings.ToLower` folds Unicode; `downcase_identifier` rewrites only
  `A`–`Z`. Copy to a `[]byte` and rewrite that range alone.
- `FuzzTokenize` — a hang or an out-of-range panic on invalid UTF-8.
  `utf8.DecodeRuneInString` returns width 1 for an invalid byte, so
  advancing by the decoded width always makes progress; advancing by
  zero on a decode failure is the infinite loop.

## Why not unreserved keywords

PostgreSQL sorts keywords into four classes so that `key`, `level`, `name`
and hundreds of others can still be column names; the classes exist because
the standard's keyword list grew faster than anyone's tables were renamed.
We have one class, every keyword is reserved, and the list stays short
instead (D17). The consequence is visible in this chapter's tests: `create
table t (key int4)` is a syntax error here and legal in PostgreSQL, and
every later chapter that adds a keyword (`INDEX`, `UNIQUE`, `ANALYZE`) takes
a name away from users.

## Out of scope

Unreserved keywords, `E'...'` and dollar-quoted strings, Unicode escapes,
float and bit-string literals, `::` casts, user-defined operators, the
63-byte identifier truncation (`NAMEDATALEN`), parameter placeholders
(`$1`), and the psql backslash commands, which chapter 11 handles before
the text reaches the lexer.

## Check your understanding

1. What does `select "a""b" from t` lex to, token by token?
   <details><summary>Answer</summary>

   `Keyword` `select`, `Ident` `a"b`, `Keyword` `from`, `Ident` `t`,
   `EOF`. The doubled quote inside a delimited identifier stands for one
   quote and does not close it, so the name has three characters. The
   identifier is not folded and could never be a keyword.
   </details>
2. Where does the lexer report the error in `select 'a' 'b`, and why is
   that not the position where it ran out of input?
   <details><summary>Answer</summary>

   At the opening quote of `'b`, which is column 12. `'a'` is a complete
   string. The scanner then opens a second string, records that quote as
   the token start, and runs to the end of the input without a closer;
   the error is reported at the token's start because that is what the
   REPL underlines. Pointing at the end of input would tell the user
   nothing about which quote is unbalanced.
   </details>
3. Why do keywords share one `Kind` and get told apart by `Text`, while
   every operator gets its own `Kind`?
   <details><summary>Answer</summary>

   The operator set is closed: the expression grammar in chapter 07
   switches on it structurally and will never grow, so an enum costs
   nothing and gives the parser exhaustive cases. The keyword list grows
   with every chapter that adds a statement, and a table is the cheap
   place to grow it: adding `UNIQUE` in chapter 13 is one entry, not a
   new constant, a new `String` case, and a new parser case.
   </details>
4. Why does the lexer leave `3000000000` as digits instead of deciding it
   is an `int8`?
   <details><summary>Answer</summary>

   Because the type of a literal is a semantic question and the lexer has
   no types. PostgreSQL does the same: the scanner produces an `ICONST`
   or an `FCONST` and the analyzer picks `int4`, `int8` or `numeric` by
   magnitude. Deciding here would also mean the lexer owns the overflow
   error and its message, which belongs with the rest of the type errors
   in chapter 09.
   </details>
5. PostgreSQL's scanner is generated by flex from `scan.l` and gives
   every keyword its own token number from `kwlist.h`. What does a
   generated scanner buy, and why is a hand-written one fine here?
   <details><summary>Answer</summary>

   Flex compiles the rules into a table-driven automaton that matches the
   longest rule in one pass, which stays fast and stays correct as the
   rule set grows to include dollar quoting, Unicode escapes, operator
   precedence hacks and the several start conditions PostgreSQL needs.
   The cost is a build-time dependency and a scanner nobody reads. Our
   token set is small enough that the longest-match logic is two lines of
   lookahead, and the standard library only rule (D1) rules out a
   generator anyway. The place it would hurt first is operators: a
   user-definable operator set is exactly what a hand-written greedy
   matcher gets wrong.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **A second class.** Add an unreserved class: keywords the parser accepts
   as identifiers where an identifier is expected. The lexer keeps returning
   `Keyword`; the parser decides. Then make `key` a legal column name.
2. **Richer literals.** Add `E'...'` escape strings and dollar quoting
   (`$tag$...$tag$`), both of which change where a string ends, and check
   that your error positions still point at the opening quote.
3. **Read `scan.l`.** Find the states it enters for comments and dollar
   quotes, and the lookahead hack for `..` and `:=`. Which of them does a
   hand-written scanner get for free, and which would we need too?
