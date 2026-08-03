# Planning Mode

You are in PLANNING mode. Do NOT implement anything.

Your job is to turn the design document into a task list precise enough that a fresh agent
with no memory of this conversation can execute any single task correctly.

## Orient

0a. Read `.ralph/stack.json` — this is how the project is built, tested, and reviewed.
    Every command you put in a task must come from here, not from your assumptions.
0b. Read the design document. Its path is in `spec_file` in `tasks.json`, or in the
    `RALPH_DESIGN_DOC` environment variable on the first pass. This is the source of truth
    for scope. Understand both what changes and what explicitly does not.
0c. Read `IMPLEMENTATION_PLAN.md` if it exists — that is the plan so far, and you are
    refining it rather than starting over.
0d. Read the conventions files listed in `.ralph/stack.json` (`conventions_files`).
0e. Study the existing source with parallel subagents — the directories in `source_dirs`
    and `test_dirs` from `.ralph/stack.json`. Understand what is already built before
    planning to build it again.

## Plan

1. Use subagents to compare the existing code against the design document. Ultrathink. Consider:
   - What genuinely needs new code vs. modifying what exists?
   - What needs deleting?
   - What are the dependencies between pieces? (schema before code that uses it,
     types before consumers, API before client)
   - What behaviours need tests? What edge cases does the design doc imply but not state?

2. Write or update `IMPLEMENTATION_PLAN.md` as a prioritized task list in the format below.

3. Update `tasks.json` to match the plan (format below).

4. Commit — but only if something meaningfully changed.

## Task Format

Each task follows test-first order. A task is right-sized when one fresh agent can finish it
in a single session.

```markdown
### Task N: [Short title]

**Depends on:** Task M (or "none")

**Test files:**
- `<path>` (create/modify) — what behaviour this covers

**Implementation files:**
- `<path>` (create/modify) — what changes here

**Red — write failing tests:**
1. [Specific assertion, not "test the thing"]
2. [Another specific assertion]

**Green — minimal implementation:**
1. [Specific step]
2. [Specific step]

**Verify:** `<exact command from .ralph/stack.json test_command or test_single_command>`

**Why:** [One line — the value this delivers, so a later agent can judge tradeoffs]
```

### Special cases

- **Schema/migration tasks** — no red/green cycle; create the migration and verify it runs.
  These come first so later tests have a schema to run against.
- **Config/scaffolding tasks** — no red/green; state the verification that proves it worked.
- **Pure view/template tasks** — test at the request/render level asserting content and status.
  Do not assert on detailed markup structure.

## tasks.json Format

```json
{
  "plan_file": "IMPLEMENTATION_PLAN.md",
  "spec_file": "<path to the design document>",
  "generated_at": "<ISO 8601 timestamp>",
  "total_tasks": 0,
  "tasks": [
    { "id": 1, "section": "1", "title": "…", "line_start": 0, "line_end": 0, "done": false }
  ]
}
```

`line_start` and `line_end` must accurately bracket each `### Task N:` section in
`IMPLEMENTATION_PLAN.md`. The build phase reads **only those lines** — wrong line numbers
mean the builder reads the wrong task. Recount them every time you rewrite the plan.

Preserve `done: true` on any task that is already complete. Never reset finished work.

## Commit

```bash
git add IMPLEMENTATION_PLAN.md tasks.json
git commit -m "docs: update implementation plan

Added: [new tasks]
Removed: [completed or dropped tasks]
Changed: [modified tasks]"
```

**If nothing meaningful changed, do not commit.** The loop detects a converged plan by the
absence of new commits — an empty or cosmetic commit keeps it spinning for no reason.

## Rules

- Plan only. Do not write implementation code.
- Do not assume something is missing — search the codebase and confirm before planning to build it.
- Keep tasks small. One task, one session, one commit.
- Respect dependency order. Blocking tasks come first.
- Remove completed tasks from the plan body entirely — no "Done" section.
- Capture the *why* in every task. A fresh agent uses it to make judgment calls.
- Every command you write must come from `.ralph/stack.json`. Do not invent test commands.
- For features spanning multiple components, include a final task that verifies data flows
  end to end. Do not assume component N's output reaches component N+1.
