# Chapter template

How a chapter looks on `main`. Copy the README skeleton below into
`chapters/NN-name/README.md` and fill every section; the rules under each
heading say what goes there and how long it is. Chapter 01 is the
reference for the short form, chapter 11 for a multi-package chapter.

A chapter is three things, committed together on `main`:

- `chapters/NN-name/README.md` in the order below.
- `internal/<pkg>/<pkg>.go`: every exported type, constant, error
  sentinel, and doc comment of the solution, with each exported function
  body replaced by `panic("not implemented")`. Private fields and helpers
  may differ from the solution.
- `internal/<pkg>/<pkg>_test.go`: the full tests. They are the contract.

Then `CHNN` in the Makefile, a "Status" line in the root README, and the
chapter's line in `docs/PLAN.md` marked done.

## README skeleton

````markdown
# Chapter NN: Name

**Goal.** One or two sentences, from the `Goal:` line in docs/PLAN.md.

**You edit.** `internal/<pkg>/<file>.go`: N functions with
`panic("not implemented")` bodies, `First` through `Last`. Nothing
else changes; `<pkg>_test.go` is the contract.

**Needs from earlier chapters.** Packages this chapter calls, and any
function in an earlier package that was stubbed for this chapter.

**Done when.** `make test-chNN` passes.

**Effort.** Short, Medium or Long and a round number of hours, then the
step that is the hard part and one clause saying why. Short is about two
hours, Long is six or more and says whether to split it over two
sittings.

Then the architecture map from chapter 00, reprinted verbatim in a
fenced block under the caption "Where this chapter sits in the whole,
from chapter 00. The box in brackets is this one.", with this chapter's
`(NN)` changed to `[NN]`.

Motivation: two or three paragraphs. What PostgreSQL problem this
subsystem solves and why it comes now.

PostgreSQL source: `dir/file.c` (`func_a`, `func_b`), ... under
`src/backend/` and `src/include/`.

## Design sections

One `##` per concept, in the order the reader needs them. Exact on-disk
layout or algorithm wherever a test depends on it (golden bytes, error
messages, ordering). Tables for byte layouts.

Every section that pins a layout or an algorithm ends with a worked
example on concrete data: an annotated dump for a layout, a table with
one row per step for an algorithm. An algorithm with a shape worth
seeing (the clock sweep, a B-tree split, hash join's two phases, a
snapshot on a line of transaction IDs) also gets one ASCII diagram, on
the same data as the worked example. The hardest design section opens with
a **Predict.** line, one question the reader answers before reading on.

## API

`internal/<pkg>`

```go
// the exported signatures, one block per package
```

Semantics the tests depend on:

- One bullet per rule a test checks that the signature does not say:
  error conditions, aliasing, ordering, what a failed call leaves behind.

## Implementation notes

From chapter 06 on, the section opens with the sentence saying the
sketches are folded, and from chapter 12 with a `**Yours to design.**`
paragraph naming the one area the chapter does not sketch and the
constraints the tests do pin.

**Go you will need.** Standard-library pieces a reader new to Go would
not guess. Only what the chapter uses. Never folded.

<details><summary><b>Function or area.</b></summary>

Three to six lines: what to compute, in what order, which fields to
update, the trap the tests set. No code. Where one exported stub hides a
private design, name the private types and helpers the solution uses and
what each does. Chapters 01 to 05 leave these entries unfolded; from
chapter 06 each one is a `<details>` whose summary is the entry's title
in `<b>` and `<code>`.
</details>

## Suggested order

One step at a time with the line under it; `make test-chNN` at the end.
Every test not named in that step or an earlier one still panics.

1. Area. `Func`, `Func`. Green: `TestA`, `TestB`. One-line hint, or a
   pointer at the Implementation notes entry.

   ```sh
   go test -race ./internal/<pkg>/... -run 'TestA|TestB'
   ```
2. ...

### When a test fails

- `TestName` — the symptom, then the cause in one clause.

## Why not <the rejected alternative>

One paragraph: the design this chapter did not take, why, and the
consequence the reader can see in the code or the tests. Ends with the
decision number, `(D10)`.

## Out of scope

What PostgreSQL does here that this chapter does not, with the chapter
or version that adds it.

## Check your understanding

Five questions, answers in `<details>`.

1. Question.
   <details><summary>Answer</summary>

   Answer.
   </details>

## Challenges

Optional and untested. Later chapters expect the implementation the tests
describe, so do these on a branch and come back.

1. **Short title.** Two or three sentences of direction, promoted from
   Out of scope. The third is a reading challenge: a PostgreSQL source
   file and a question to answer from it.
````

## Rules per section

- Header block: five `**Bold.**` lines, then the chapter-00 map, then
  the motivation. Stub count is `grep -c 'not implemented'` per file. Extra
  commands (`-update`, fuzzing, `go run`) go in a fenced block at the end
  of Suggested order, not here.
- Design sections: prose explains why; tables and numbered lists give
  what the tests pin. Mirror PostgreSQL's design and messages, not its
  code (D4).
- Worked examples: one per section that pins a layout or an algorithm,
  on data small enough to check by hand and taken from a test where a
  test uses one. A layout gets a labelled dump (offset, bytes, field);
  an algorithm gets a table with one row per step and the state after
  it. Prose, not Go `Example` tests (D2), so the reader can follow it
  before any code compiles.
- Predict lines: one per chapter, a `**Predict.**` paragraph at the top
  of the hardest design section. One question, no answer; the section
  answers it.
- Semantics bullets: error behaviour as strictly as success. Name the
  sentinel, the message, and the position where there is one.
- Implementation notes: entries follow the Suggested order steps. Trivial
  accessors get no entry. Chapter 01 alone shows two accessors as code,
  to establish the pattern; later chapters show none. Chapters 01 to 05
  are fully worked in the open; from chapter 06 every entry but "Go you
  will need" is folded into `<details>`; from chapter 12 one named area
  per chapter is left unsketched.
- Suggested order: three to eight steps. Every stubbed function and
  every `TestX`/`FuzzX` in the chapter's packages appears exactly once. A
  test goes in the earliest step at which every function it calls
  exists; read the test body, do not guess from the name. Renaming or
  adding a test means updating the list.
- Every step is followed by a fenced `sh` block holding the exact
  `go test -race <pkg> -run '<TestA|TestB>'` line for that step's tests,
  alternation in the order they are named. Chapter 01 alone also shows
  one panic-to-PASS transcript, to establish what the loop looks like.
  `-run` patterns are unanchored, so a name that is a prefix of a later
  chapter's test (`TestDelete` and `TestDeleteSnapshot`) needs a `$`.
  Run the line against `solution` and check what actually ran.
- When a test fails: an `###` block closing Suggested order, one bullet
  per test that has a non-obvious failure, keyed by the symptom a reader
  sees ("fails on the third eviction", "off by 8 bytes"), then the cause.
  Sourced from the traps in Implementation notes and from what the
  solution got wrong first. Ungated: no `<details>`.
- Check your understanding: five questions after Out of scope, each
  answer in `<details><summary>Answer</summary>`. Order: two concrete
  ones the reader computes from the chapter's numbers, two on why the
  design is what it is, one on PostgreSQL's real trade-off. Blank line
  after the `<summary>` line and before `</details>`, or the Markdown
  inside does not render.
- Length: wrap at 76 columns. Table rows and the `-run` lines are
  exempt; they cannot be wrapped. Terse; no filler, no "simply".

## Skeleton doc comments

- First sentence as in the solution. Then the error contract where the
  function returns an error or panics: "Returns ErrX if ...; ErrY if
  ...". One or two lines.
- Where a genuine PostgreSQL counterpart exists, last line
  `// PostgreSQL: FuncName in file.c.` Skip it for `String` methods,
  trivial accessors, and anything without a real match; omit rather than
  guess.
- Doc comments are identical on `main` and `solution`. Change them on
  `main` and merge forward.

## Checks before committing

```sh
gofmt -l . ; go vet ./...
git diff main solution -- internal/<pkg>/   # once the solution exists: bodies only
```

And by hand: every stub and every test name in Suggested order; every
sentinel in the semantics bullets has a test; the header block's counts
match the file.
