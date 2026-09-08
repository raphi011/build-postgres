# CLAUDE.md

Book-style repo: "Build Your Own PostgreSQL" in Go, one chapter per subsystem.
`docs/PLAN.md` is the chapter plan; `docs/DECISIONS.md` (D1…Dn) records the
design choices. Read the relevant decision before changing any design.

## Two branches, one contract

- `main`: chapter READMEs, package skeletons (`panic("not implemented")`
  bodies), full tests. Tests here panic by design.
- `solution`: reference implementation, merged from `main`, one annotated tag
  `chNN` per chapter, so `git diff ch05 ch06` shows exactly what a chapter adds.
- Tests are the contract. The solution never bends a test; a wrong test is
  fixed on `main` and merged forward.
- Skeleton keeps every exported type, constant, and doc comment; only
  function bodies are stubbed. Private fields may differ from the solution.

## Per-chapter workflow

```sh
git checkout main
# write chapters/NN-name/README.md, internal/<pkg>/<pkg>.go (skeleton),
# internal/<pkg>/<pkg>_test.go; add CHNN to the Makefile; update README "Status"
gofmt -l . ; go vet ./...
git add -A && git commit -m "Add chapter NN: <name> (skeleton and tests)"

git checkout solution && git merge --no-edit main
# replace the skeleton bodies with the implementation
gofmt -l . ; go vet ./... && go test -race -count=1 ./...
git add -A && git commit -m "Chapter NN solution: <name>"
git tag -a chNN -m "Chapter NN solution: <name>"
git checkout main
```

- Wrong test found while implementing: fix on `main` (amend if the skeleton
  commit is still HEAD, else a small fix commit), merge into `solution`
  again. Amending `main` after `solution` already merged it makes the next
  merge conflict (add/add): abort, `git reset --hard chNN-1` on `solution`,
  merge the amended `main`, reapply the implementation.
- Later chapter needs a new method on an earlier package: add it to the
  skeleton on `main` in that chapter's commit, appended at the end of the
  file so the merge into `solution` applies cleanly.
- Chapter 11+ regression suite: `go test ./internal/regress/... -update`
  regenerates `testdata/regress/expected/`; review every line against
  psql's aligned format and PostgreSQL's messages before committing.
  `make test-chNN` sets `REGRESS_CHAPTER=NN` to run only the files up to
  that chapter, so an earlier chapter's `.sql` must not produce output a
  later chapter changes.

## Conventions

- Standard library only (D1). Every test runs with `-race`.
- Mirror PostgreSQL's design and messages, not its code (D4). Chapter
  READMEs name the `src/backend/` and `src/include/` files they correspond to.
- `docs/CHAPTER_TEMPLATE.md` is the full README and skeleton template.
- Chapter README order: title, then a header block (`**Goal.**`, `**You
  edit.**` with files and stub counts, `**Needs from earlier chapters.**`,
  `**Done when.** \`make test-chNN\` passes`, `**Effort.**` with a
  Short/Medium/Long rating, an hour estimate and the hard step), the
  chapter-00 architecture map with this chapter's box in brackets,
  motivation, PostgreSQL source paths, the design, exact on-disk layout or
  algorithm where tests depend on it, API listing, "Semantics the tests
  depend on" bullets, "Implementation notes" (`**Go you will need.**` line,
  then an algorithm sketch per non-trivial function or private design area,
  no code; from ch06 every sketch is folded into `<details>`, from ch12 one
  named area per chapter is left unsketched), "Suggested order" (numbered
  steps: functions to implement, then the tests that go green; every stub
  and every test named exactly once), "Why not <alternative>" with its
  D-number, "Out of scope", "Check your understanding" (five questions,
  answers in <details>), "Challenges" (two or three, the last a reading
  challenge). Renaming or adding a test means updating that list.
- Skeleton doc comments state the error contract ("Returns ErrX if ...")
  and, where a genuine counterpart exists, end with
  `// PostgreSQL: FuncName in file.c.`
- Tests: table-driven and golden byte tests first, then behaviour, then a
  property or concurrency test where meaningful. Error behaviour is tested
  as strictly as success: `errors.Is` against exported sentinels, message
  text as PostgreSQL. `t.TempDir()` for data directories.
- Style: terse doc comments.
- New keywords go into `lexer.keywords`. New statements get an `ast` struct
  with a `String()` case, a `parseX` in `parser.parseStmt`, and golden cases
  in `parser_test.go` (each golden case is also round-tripped).
- Keep going chapter by chapter without stopping for review; the user
  reviews once all planned chapters are done.
- No push to `origin`, no CI, no LICENSE unless the user says so.

## Gotchas

- `git tag` needs `-a -m` (user config). macOS `sed -i ''`.
- Fuzz runs can show 0 execs/sec near `-fuzztime`; that is input
  minimisation, not a hang.
- `Split` in `internal/session` uses a hand-written scanner, not the lexer
  (a sticky lexer error would hang the REPL).
