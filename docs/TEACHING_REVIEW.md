# Teaching format review (after v1)

**Status.** R1 to R4, R9 and R10 are implemented across chapters 00 to
17 and the template, on `main` and merged into `solution`. Open decisions
were resolved as recommended: D1 not yet reached (R5 untouched), D2 prose
worked examples and no `Example` tests, D3 questions inside the README.
R4 states "every test not named in this step or an earlier one still
panics" once in each Suggested-order preamble instead of per step. R9's
`**Effort.**` line is last in the header block, after `**Done when.**`.
R10 is a "Using the solution branch" section in chapter 00, replacing the
one-line mention of `git diff ch04 ch05`. R8's map is reprinted after the
header block with this chapter's number in brackets. R6 is a "Why not
<alternative>" section before Out of scope, ending in its D-number, and
R7 a "Challenges" section last in the chapter, three items of which the
last is a reading challenge. R5 is implemented as D1 recommended:
chapters 01 to 05 keep their sketches in the open, chapters 06 to 17 fold
every entry but "Go you will need" into `<details>`, and chapters 12 to
17 open with a `**Yours to design.**` paragraph naming the one area they
do not sketch and the constraints the tests pin. R11 is `pgdb sample`
and `pgdb dump` in `cmd/pgdb`, provided complete rather than stubbed and
gated at chapter 02 rather than 03 or 05, so that the first runnable
thing needs only `page` and `tuple`. Every recommendation is
implemented.

Written September 2026, once chapters 00–17 were complete on `main` and
`solution`. It compares our chapter format with eleven other "build your
own X" resources and the pedagogy literature, lists the gaps, and ranks
the changes. A later conversation implements it: start from "Open
decisions", then "Work order".

## Our format today

Per `docs/CHAPTER_TEMPLATE.md`: header block (Goal / You edit / Needs /
Done when), motivation, PostgreSQL source paths, design sections, API,
"Semantics the tests depend on", Implementation notes (one entry per
function, no code), Suggested order (steps naming every stub and every
test), Out of scope. Skeleton with `panic("not implemented")` bodies and
full tests on `main`; solution on `solution`, tag `chNN` per chapter.

Measured on the READMEs as of commit a1b9ea6:

| chapter | words | code blocks | table rows | stubs |
|---|---|---|---|---|
| 00 | 528 | 4 | 0 | 0 |
| 01 | 1822 | 3 | 15 | 17 |
| 02 | 1609 | 2 | 23 | 16 |
| 03 | 1252 | 2 | 0 | 10 |
| 04 | 1681 | 1 | 0 | 13 |
| 05 | 1650 | 1 | 0 | 14 |
| 06 | 2216 | 3 | 10 | – |
| 07 | 3761 | 6 | 10 | – |
| 08 | 2570 | 1 | 21 | 18 |
| 09 | 4323 | 4 | 15 | – |
| 10 | 2349 | 3 | 0 | – |
| 11 | 4630 | 9 | 10 | 20 |
| 12 | 3655 | 1 | 19 | 25 |
| 13 | 3773 | 11 | 7 | 5 |
| 14 | 2809 | 6 | 19 | – |
| 15 | 4546 | 6 | 10 | – |
| 16 | 2965 | 3 | 10 | 10 |
| 17 | 4505 | 10 | 6 | 7 |

Other counts: "for example" appears in no chapter. One session
transcript (ch11 REPL). ASCII diagrams in ch00 (architecture) and ch01
(slotted page) only. No questions, exercises, or challenges anywhere.
No time or difficulty estimates.

## The field

| resource | unit, words | design given | solution | hints | tests named per step | self-check | exercises |
|---|---|---|---|---|---|---|---|
| CodeCrafters | stage, 150–400 | protocol only | per-stage diff, paid | gated Q→snippet | 1 stage = 1 test | no | extensions |
| Mini-LSM (skyzh) | day, ~2100 | layouts, lock patterns, snippets | always, commit per chapter | inline | per day | 6–9 per day | bonus tasks |
| CMU 15-445 BusTub | project, ~3100 | API fixed, algorithm named | never | failure-keyed, Common Pitfalls | tiered names, roadmap | no | leaderboard |
| MIT 6.5840 Raft | lab part, ~1000 | 4-line API, paper | never | ungated bullets, Go traps | PASS transcript pasted | no | no |
| Talent Plan TinyKV | part, 300–3000 | architecture, papers | never | 8–12 bullets | make target per part | no | no |
| Crafting Interpreters | chapter, ~5300 | every line | is the text | questions in challenges | repo map only | no | 2–4 challenges, design note |
| Ball, Interpreter in Go | chapter | every line, test first | is the text | none | the test just written | no | none |
| Sandler, C Compiler | chapter | pseudocode, ASDL, BNF | optional reference | forward-compat warnings | stage flags | no | extra credit |
| Smith, DB in Go | chapter, 1200–3400 | layouts, partial code | tarball | none | none | no | one-liners |
| cstack db_tutorial | part, ~2300 | every line, full diff | is the text | none | rspec inline | no | no (unfinished) |
| Sciore, SimpleDB | chapter, ~9500 | API figure, narrated client, code | is the text | none | none | 10 conceptual | 8 programming |
| **ours** | chapter, 1250–4630 | layouts, API, algorithm in prose | branch + tag | traps inside notes | every test per step | no | no |

Our structure is the one Mini-LSM v2, 15-445 and MIT converged on, and
the one reviewers rate highest. Naming every test per step is rarer
than it looks; only MIT and Nand2Tetris do it.

## What readers reward and punish

1. Guidance beats a bare spec, but only if it explains why. Mini-LSM v1
   "proved too open-ended ... students found it difficult to understand
   why a design decision was made"; v2 added layouts, invariants and an
   overview per day. CodeCrafters gets the opposite complaint: "doesn't
   teach you how to build the software or even an architecture".
2. Worked examples with real data are the most requested addition.
   Mini-LSM issue #11: "you give great schemas of the format, but I would
   also have loved seeing a concrete example ... with actual example
   data." Sciore narrates every client program's output. toydb re-shows
   one query's plan after each optimizer pass.
3. Copy-along is the failure mode. On Nystrom: "easy to fall to the trap
   of just copying the code". On Ball: "I wish it had something in the
   way of exercises. It's a lot of copying." Renkl and Atkinson: fading
   worked steps beats keeping them; retention is highest on faded steps.
4. Self-explanation questions are cheap and effective (Chi 1989).
   Mini-LSM: "Test Your Understanding" and "Predict before coding".
5. Stuck readers want a symptom, not a lecture. MIT: "If you fail a test,
   look at raft_test.go and trace the test code"; "Don't use time.Ticker,
   difficult to use correctly." Exercism keys Go hints by error text.
6. Difficulty cliffs are remembered. CodeCrafters SQLite: "from 'uncomment
   this line' to 'read a btree from the db file, glhf'". Their authoring
   rule: split a Medium stage into three Easy ones; first stages easy.
7. Terse reads as unfinished. Smith (closest to our voice): "so tersely
   written, it feels like only half done ... not guiding, just
   condescending". Nystrom is praised for "nothing left as an exercise"
   and a runnable program at the end of every chapter.
8. Debug tooling is pedagogy. skyzh on 15-445: making students "dump the
   version chain to stdout" found bugs at once. cstack tests the output
   of its print-constants and print-tree commands.

## Gaps

Most consequential first.

- F1 No worked examples. Byte-layout tables without an annotated dump of
  a real page or tuple; clock sweep, visibility rule and join order are
  rules, never traced on a concrete state. Golden-byte failures are
  unreadable without one.
- F2 No self-check or prediction questions.
- F3 No exercises beyond the tests. Out of scope already lists the raw
  material.
- F4 No fading. Ch04's `Pin` entry is a four-step algorithm and ch17's
  entries are the same density; a reader who follows the notes never
  designs anything. Ch11 ("the node types behind them are private and
  yours to design") is the only open area in the book.
- F5 Chapters say what, DECISIONS.md says why, rarely linked. D9, D10,
  D13, D20–D24 carry the rejected alternatives.
- F6 Pitfalls are buried inside per-function prose; no symptom-keyed
  list a reader with a failing test name can scan.
- F7 Suggested order names tests but not a runnable `-run` line.
- F8 One diagram after ch01. The ch00 map is never re-shown with the
  current chapter marked. Clock sweep, B-tree split, hash join, snapshot
  visibility have no picture.
- F9 No time or difficulty signal; chapters span 1250–4630 words and
  1–25 stubs.
- F10 Solution policy unstated: when to look, how to look at one
  function, how to swap in an earlier chapter.
- F11 Nothing runnable before ch11.
- F12 Voice: our register is the one Smith is punished for. Density is
  fine when every reference resolves; it reads as omission where the
  example (F1) is missing.

## Recommendations

Ordered by value per hour. R1–R4 are additive. R5 is the only one that
changes what a chapter withholds.

### R1 One worked example per pinned design section

Where a test depends on a layout or algorithm, show one concrete
instance, in prose plus a table or fenced block:

- ch01: hex dump of a page holding three items, every field labelled.
- ch02: one tuple with a null bitmap and one without, byte by byte.
- ch04: clock sweep over four frames, one table row per step showing
  hand, usage counts, victim.
- ch12: a leaf before and after a split, and the parent entry it adds.
- ch14, ch15: one query's plan re-shown after each planner decision.
- ch17: one snapshot (xmin, xmax, running set) and three tuples with the
  visibility verdict and the rule that decided each.

Go `Example` tests with `// Output:` blocks are an option where output
is small (ch01, ch02, ch14 `EXPLAIN`); they are checkable and appear in
godoc but add test names to Suggested order and panic on `main` like any
other test. See D2.

### R2 "Check your understanding", five questions per chapter

New section after Out of scope. Order: concrete ("A page has lower = 40
and upper = 8100. How many 30-byte items fit?"), then design ("Why does
`Compact` not renumber line pointers?"), then one on PostgreSQL's real
trade-off. Answers in `<details>`. Add one "predict before coding"
prompt at the top of the hardest design section (clock sweep, visibility
rule, hash join build side).

### R3 Symptom-keyed pitfalls

New block at the end of Suggested order: "`TestClockSweepEvictsUsageZero`
fails on the third eviction: you advanced the hand after inspecting the
frame, not before." Source from the traps already in Implementation
notes and from what the solution got wrong on first pass. Ungated.

### R4 Runnable line per step

Each Suggested-order step gets its exact
`go test -race ./internal/<pkg> -run 'TestA|TestB'` line and which
tests still panic after it. One panic-to-PASS transcript in ch01 only.

### R5 Fade the Implementation notes

- ch01–05: keep full sketches. Say in ch00 that early chapters are
  fully worked on purpose and the hand-holding thins out.
- ch06 onward: wrap each function sketch in `<details>` so the default
  reading is design, API, semantics, order, pitfalls.
- ch12 onward: leave one named area per chapter open (as ch11 does for
  executor nodes) and say which.

Mirrors skyzh's ladder for 15-445: follow pseudocode, then debug, then
read existing code, then make design choices.

### R6 "Why not" paragraph per chapter

One paragraph naming the rejected alternative and its consequence,
linking the D-number: LRU vs clock (ch04), B+tree with deletion (ch12,
D10), undo log vs xmin/xmax (ch17), default selectivities vs statistics
(ch14, D21). Most text exists in DECISIONS.md already.

### R7 Two or three challenges per chapter

Promote Out of scope items with one sentence of direction and a note
that later chapters expect the pristine implementation (do them on a
branch). Research challenges ("read HOT chain handling in heapam.c; why
does PostgreSQL need it and we do not?") are free to write.

### R8 Map plus four mechanism diagrams

Reprint the ch00 ASCII map at the top of each chapter with this
chapter's box marked. One ASCII diagram each: clock sweep (ch04),
B-tree split (ch12), hash join build/probe (ch15), snapshot timeline
(ch17).

### R9 Effort line in the header block

Sixth `**Bold.**` line: "**Effort.** Medium, about 4 hours; the sweep
(step 1) is the hard part." Update the template's header rule.

### R10 Solution policy in ch00

When to look (a test red for an hour), how to look at one function
(`git show ch04:internal/bufmgr/bufmgr.go`), how to swap in an earlier
chapter (`git checkout ch03 -- internal/smgr`) with the warning that
tests then pass without your code.

### R11 Something runnable before ch11

A `pgdb dump <file>` subcommand printing page headers and line pointers:
observable result for ch01–05, a debugging tool for every later chapter,
and one golden test of its own. New code, so it needs a home in the
chapter plan (ch03 or ch05) and a solution commit.

## Open decisions

- D1 Fade (R5) or keep the full notes and rely on R2/R7 to force
  generation? Resolved: fade with `<details>`, not deletion; the ramp is
  documented in ch00 and the template.
- D2 Worked examples as prose or as Go `Example` tests? Recommended:
  prose plus dump tables; `Example` tests only for ch01, ch02, ch14.
- D3 Questions and challenges inside the README or a separate file?
  Recommended: inside, after Out of scope, answers in `<details>`.

## Work order

Assuming D1 = fade, D2 = prose, D3 = inside:

1. Template first: add the Effort line, the Pitfalls block, the
   `-run` line rule, and the two new sections (Check your understanding,
   Challenges) to `docs/CHAPTER_TEMPLATE.md`. Add the fading rule and
   solution policy to ch00.
2. R2, R3, R4, R9, R10 across all chapters. Mechanical; about a day.
3. R1 and R8 on ch01, 02, 04, 12, 17 (where readers stall), then the
   rest.
4. R6 and R7 across all chapters.
5. R5: wrap sketches from ch06, pick the open area per chapter from
   ch12.
6. R11 last; it touches code on both branches.

Every README edit is on `main` and merged into `solution` (D14). Adding
`Example` tests or `pgdb dump` follows the per-chapter workflow in
CLAUDE.md, including a new tag only if a chapter's solution changes.

## Sources

- Mini-LSM: skyzh.github.io/mini-lsm (preface, week 1); issue #11 on
  github.com/skyzh/mini-lsm; skyzh.dev, "The final semester in BusTub".
- CodeCrafters: docs.codecrafters.io/contributors (authoring guides);
  github.com/codecrafters-io/build-your-own-redis; HN 32342334;
  crossroad.dev and knutwalker.codes write-ups.
- CMU 15-445 Fall 2024 project 1 and 2 specs; cmu-db/bustub tests.
- MIT 6.5840 lab-raft1, lab-kvraft1, guidance; Gjengset, "Students'
  Guide to Raft".
- Talent Plan tinykv and tinysql docs; HN 20161214.
- Crafting Interpreters ch. 4, 14, 26; tool/bin/test.dart; Nystrom,
  "Crafting Crafting Interpreters"; HN 31835818, 43727118, 44541565.
- Ball, interpreterbook.com sample; Goodreads; HN 21626972.
- Sandler, norasandler.com/book, writing-a-c-compiler-tests;
  jollygoodsw.com.
- Smith, build-your-own.org/database ch. 1, 4, 5; Goodreads; HN 35212654.
- cstack db_tutorial parts 4, 5, 8, 15; HN 19581721.
- Sciore, Database Design and Implementation ch. 4; jessepav/simpledb.
- toydb docs/architecture; howqueryengineswork.com ch. 5; HN 37415494.
- os.phil-opp.com; snaptoken kilo; nand2tetris.org projects 1 and 7;
  rustlings info.toml; Exercism concept-exercise docs.
- Kirschner, Sweller and Clark 2006; Renkl and Atkinson on fading;
  Margulieux, Morrison and Guzdial on subgoal labels; Chi et al. 1989;
  Roediger and Karpicke 2006; Wilson, Teaching Tech Together; Brown and
  Wilson, "Ten quick tips for teaching programming"; Julia Evans on
  filling knowledge gaps.
