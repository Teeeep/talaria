# Review Plan Mode

You are in REVIEW PLAN mode. Convert review findings into fix tasks. Do NOT fix anything here.

## Orient

1. Read `REVIEW_FINDINGS.md` — the issues found.
2. Read `.ralph/stack.json` — verification commands for the tasks you write.
3. Read the design document (`spec_file` in `tasks.json`) for context on intended behaviour.

## Plan the fixes

1. Take **every CRIT, and every WARN**. Skip INFO unless it is one line beside a task you are
   already writing.

   This used to say CRIT only, on the reasoning that WARNs are "recorded for a human and do not
   block the PR". They were not recorded for anybody: each cycle re-found them and appended
   `Repeat-of: … (re-confirmed live; no fix attempted)`, and by cycle 4 of phase 2a there were
   nine, polluting the repeat count the guardrails read. Nothing was draining them. A WARN that
   survives three review cycles is not non-blocking, it is unowned — and a remediation phase that
   ships with ten live findings is the failure the phase exists to correct.

   Group aggressively: several WARNs with one root cause are one task, and four surfaces of one
   behaviour are one task, not four. Splitting those is what put the same logic through three
   rewrites in four hours.
1a. **Skip any CRIT whose `Blocked-by` is `design`, and any whose `Repeat-of` is not `none`.**
   These are escalated to a human, not fixed here.
   - `Blocked-by: design` means the fix must invent a policy the design doc does not state.
     Writing that code guesses at the answer and hides the question.
   - `Repeat-of: <something>` means a previous cycle already tried and the defect survived.
     Re-applying a failed approach wastes the cycle.
   If **every** CRIT is skipped for these reasons, create no tasks, write no plan, and commit
   nothing — `loop.sh` detects this and stops the run for human input. Say so plainly in your
   final message rather than inventing work to look productive.
2. Group the remaining findings into tasks:
   - Same root cause → one task, even across different files
   - Same file and same kind of fix → one task
   - A finding needing changes across many files → its own task
3. Each task must be completable in a single build session.
4. **Append** these tasks to `IMPLEMENTATION_PLAN.md` under a new
   `## Review fixes — cycle <N>` heading. Do **not** replace the file or remove earlier
   sections: the build tasks that came before are the record of what this run has already
   done, and a fix task is a continuation of that work, not a replacement for it.
5. **Append** the new tasks to `tasks.json`, preserving every existing entry exactly as it
   is — including `done: true`. Continue `id` and `### Task N` numbering from the highest
   number already present; never restart at 1 and never reuse an id. `total_tasks` becomes
   the count of *all* tasks, old and new, so progress stays truthful across the whole run
   instead of resetting each time a review lands.
5a. **Every task you append carries `"kind": "fix"`.** This is not cosmetic and not optional.
   `loop.sh` counts these entries to size the fix round and to know when it is finished, and
   the build prompt takes them ahead of any feature task. Omit the field and the fix round
   falls back to building the entire remaining backlog inside the review phase, which is what
   stops later review checkpoints from ever running. Never add `kind` to a task that was
   already in the file — a pre-existing entry without it is a feature task and must stay one.
6. Commit.

## Task Format

```markdown
### Task N: [Short title]

**Fixes findings:** #3, #7

**Files:**
- `path/to/file.ext` (modify)

**Steps:**
1. [Concrete action, based on the finding's suggested fix]
2. [Concrete action]

**Verify:** `<test_command or test_single_command from .ralph/stack.json>`
- [ ] The specific behaviour that proves the finding is resolved
```

## tasks.json Format

```json
{
  "plan_file": "IMPLEMENTATION_PLAN.md",
  "spec_file": "<carry over unchanged from the current tasks.json>",
  "generated_at": "<ISO 8601 timestamp>",
  "total_tasks": 0,
  "tasks": [
    { "id": 1, "section": "1", "title": "…", "line_start": 0, "line_end": 0, "done": false },
    { "id": 9, "section": "9", "title": "…", "line_start": 0, "line_end": 0, "done": false, "kind": "fix" }
  ]
}
```

The tasks **you add** start `done: false` and carry `"kind": "fix"`. Every task already in the
file keeps its existing state untouched, `kind` included — absent means feature task — rewriting a `done: true` back to `false` re-runs finished work and makes the
loop look like it is going backwards. Carry `spec_file` across unchanged; losing it blinds the
next review phase to the design doc.

`line_start` and `line_end` must accurately bracket each `### Task N:` section. The build
phase reads only that range.

## Commit

```bash
git add IMPLEMENTATION_PLAN.md tasks.json
git commit -m "review-plan: N tasks from M findings (cycle <N>)"
```

## Rules

- CRIT and WARN findings produce tasks. INFO only when it rides along with one.
- Append, never replace. Earlier tasks and their `done` state survive every review cycle, so
  `total_tasks` only ever grows and the completed count never resets.
- Every appended task carries `"kind": "fix"`. The loop reads it; without it the fix round
  cannot tell your tasks from the feature backlog.
- Reference the specific finding numbers in every task.
- Do NOT modify or delete `REVIEW_FINDINGS.md` — the next review cycle regenerates it, and it
  stays readable in git history.
- Base each task's steps on the finding's suggested fix, but verify the fix makes sense before
  writing it down. A wrong suggested fix becomes a wrong task.
- Task titles are descriptive — they show up in the dashboard during review-fix builds.
