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

**Adversarial — what does hostile or malformed input do here?**
1. [What happens with input the code did not expect: oversized, malformed, cyclic,
   attacker-controlled, or crossing a trust boundary. "None — this touches no external
   input" is a valid answer, but it must be stated, not omitted]

**Green — minimal implementation:**
1. [Specific step]
2. [Specific step]

**Verify:** `<exact command from .ralph/stack.json test_command or test_single_command>`

**Why:** [One line — the value this delivers, so a later agent can judge tradeoffs]
```

**Why the adversarial section is mandatory.** Red assertions are written from the plan's
intentions, so a suite built only from them tests what the feature *should* do and never what
an attacker would do. That produces a large green suite that catches nothing — the failure mode
is a codebase with more test lines than source lines and a review that still finds dozens of
live defects. Ask, per task: which of these inputs is not fully under our control? A spec or
schema fetched from a URL, a file another process wrote, a flag value, a stored record read
back later, anything crossing a trust boundary. Then write the test that assumes it is hostile.

### Compaction tasks

**Emit one compaction task every ~5 tasks, and one at the end of each phase.** A loop whose
tasks are all feature tasks can only add: no iteration can see the whole, so duplication
accumulates silently and nothing is ever removed. These tasks are the only counterweight.

```markdown
### Task N: Compaction — tasks P–Q

**Depends on:** Task Q

**Deliverable is negative.** No new behaviour, no new files, no new tests beyond ones that
replace several others. The suite is green before and after, and the diff is net-negative
in lines.

1. Read `.ralph/refactor-backlog.md` first — build iterations record structural problems there
   as they hit them, because each one is visible only from inside the task that caused it.
   That file is the work list; this task drains it. Delete each entry as you resolve it, and
   leave anything you deliberately did not do, with one line on why.
2. Read every file touched since the last compaction task.
3. Consolidate types and helpers that were duplicated because separate iterations could not
   see each other's work — shared view/DTO structs, repeated parsing or formatting, near-identical
   error construction.
4. Move logic that accumulated in the entrypoint or command layer down into the package that
   owns it. A thin entrypoint is the goal; measure it.
5. Delete commented-out code, superseded helpers, and any comment asserting a property that
   no test enforces — either write the test or delete the claim.
6. Record every pattern you consolidated in the conventions file, so later iterations follow it
   instead of re-inventing it.

**Verify:** `<test_command>` green, and report the net line delta in the commit message.
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
