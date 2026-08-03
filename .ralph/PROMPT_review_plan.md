# Review Plan Mode

You are in REVIEW PLAN mode. Convert review findings into fix tasks. Do NOT fix anything here.

## Orient

1. Read `REVIEW_FINDINGS.md` — the issues found.
2. Read `.ralph/stack.json` — verification commands for the tasks you write.
3. Read the design document (`spec_file` in `tasks.json`) for context on intended behaviour.

## Plan the fixes

1. Take **only CRIT findings**. Skip WARN and INFO entirely — they are recorded for a human
   and do not block the PR. Creating tasks for them burns fix cycles on non-blocking work.
2. Group related CRIT findings into tasks:
   - Same root cause → one task, even across different files
   - Same file and same kind of fix → one task
   - A finding needing changes across many files → its own task
3. Each task must be completable in a single build session.
4. Write `IMPLEMENTATION_PLAN.md` with these tasks, replacing the previous contents.
5. Regenerate `tasks.json` from that plan.
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
    { "id": 1, "section": "1", "title": "…", "line_start": 0, "line_end": 0, "done": false }
  ]
}
```

All tasks start `done: false` — these are new fixes, none are complete. Carry `spec_file`
across unchanged; losing it blinds the next review phase to the design doc.

`line_start` and `line_end` must accurately bracket each `### Task N:` section. The build
phase reads only that range.

## Commit

```bash
git add IMPLEMENTATION_PLAN.md tasks.json
git commit -m "review-plan: N tasks from M CRIT findings"
```

## Rules

- CRIT findings only. WARN and INFO produce no tasks.
- Reference the specific finding numbers in every task.
- Do NOT modify or delete `REVIEW_FINDINGS.md` — the next review cycle regenerates it, and it
  stays readable in git history.
- Base each task's steps on the finding's suggested fix, but verify the fix makes sense before
  writing it down. A wrong suggested fix becomes a wrong task.
- Task titles are descriptive — they show up in the dashboard during review-fix builds.
