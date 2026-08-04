# Consolidate Mode

You are in CONSOLIDATE mode. The tasks are built and the branch is about to become a pull request
someone has to read. This phase is where the branch is made *true*: the tests earn their place,
the documents describe the binary that exists, and the conventions file describes this code rather
than code that was deleted three commits ago.

**This is not a cleanup nicety. It is the phase that keeps every later iteration's context
accurate.** Every future agent starts by reading CLAUDE.md, README.md and AGENT.md. A stale line
in any of them is a wrong instruction delivered with authority to everyone who comes after, and it
costs more than the bug it describes. CLAUDE.md grew from 93 lines to 663 across one abandoned
phase because every iteration added and none removed.

You may change code. You may not change behaviour. If you find a defect, write it down in the
final report rather than fixing it here — a behaviour change in the consolidation commit is a
change nobody reviewed as a change.

## 1. The test suite earns its place

The suite is 2.4x the production code and caught **none** of the 32 findings in the first review,
because it asserted what each feature should do and never what it must not. Bulk is not coverage.

Delete, in this order:

1. **Tests that assert implementation rather than behaviour.** A test that breaks when a function
   is renamed but no user-visible behaviour changed is a cost with no benefit. If the only way to
   describe what it protects is by naming an internal function, it goes.
2. **Duplicate coverage.** Several tests exercising one path through different doors: keep the one
   whose failure message would tell you the most, delete the rest. Say in the commit which you
   kept and why.
3. **Tests that cannot fail.** Assertions on values the test itself constructed one line earlier;
   table cases whose expectation was copied from the implementation's output. If you cannot state
   the change that would turn it red, it is decoration.
4. **Tests superseded by a mechanical gate.** `golangci-lint` now runs on every iteration; a test
   asserting something `errcheck` or `unused` enforces is redundant.

Keep, and add where missing:

- The hostile case for every behaviour: malformed, oversized, cyclic, attacker-controlled, or
  crossing a trust boundary. This is the half the original suite lacked entirely.
- Any test whose subject is a **contract**: an exit code, an envelope field, a document's claim.
- `internal/canary` in full. It asserts a credential *arrived* before asserting it did not leak,
  so its assertions cannot go vacuous — that property is rare and worth protecting.

**Report the numbers**: test count and lines before and after, and the single test you would most
want to keep if you could keep only one. A suite that shrinks while its hostile-case count rises
is the outcome; a suite that only shrinks is a regression you have to justify.

## 2. The documents describe the binary that exists

Read these against the code, not against each other, and drive each disagreement to zero:

- `README.md` — the human's reference.
- `AGENT.md` — the shipped operating manual for an agent driving the binary.
- `docs/design/DESIGN.md` — the contract. **If the code is right and the doc is wrong, the doc is
  the bug.** If the doc is right and the code is wrong, that is a defect for the report, not a
  silent edit.
- Every flag, exit code and envelope field a command emits appears in all three where the house
  rule requires it. A field an agent branches on and no document names is unusable.

Run the binary. Do not read the code and infer what it prints — the last phase shipped a document
describing behaviour the code no longer had, twice.

## 3. CLAUDE.md describes this code

- **Delete every rule whose subject no longer exists.** A rule naming a function that was renamed
  or removed is worse than no rule: the next agent will reason about absent code.
- **Merge rules that say the same thing.** Three paragraphs on one invariant is one paragraph.
- **A rule with no test is a claim.** Either the test exists and the rule can name it, or the rule
  states an intention and must say so.
- **It should end this phase no longer than it started**, unless the phase genuinely established a
  new invariant. Growth is the failure mode. If it grew, say what earned the lines.

## 4. The debt ledger is honest

- `grep -rn "TRACKED DEBT"` — every marker either still describes a real deferral with a named
  task, or it is stale and goes.
- `grep -rn "nolint"` — every waiver carries a reason, and the reason is still true.
- `.ralph/refactor-backlog.md` — drain what this phase's shape now makes cheap; for anything left,
  say why it is still deferred. An entry with no "why not" rots into "later means never".

## 5. Verify, then commit

Run the three commands from `.ralph/stack.json`. The loop will run them again and roll you back if
they fail, so a red consolidation commit costs an iteration for nothing.

One commit. The message is the report:

- tests deleted and added, with the count before and after
- documents corrected, and the disagreements found
- CLAUDE.md's line count before and after
- **defects found and deliberately not fixed** — this is the most valuable part of the message,
  because it is what the next phase plans from

Then stop. Do not open the PR; the next phase does that, and it will read your report.
