# Building Mode

You are in BUILDING mode. Implement **exactly ONE task, then STOP.**

Do not continue to the next task. The loop starts a fresh session with clean context for it.
Starting a second task in this session is the single most common way this loop degrades —
context fills, quality drops, and the commit boundary stops matching the task boundary.

## Orient

1. Read `.ralph/stack.json` — the test, build, and lint commands for this project.
2. Read `tasks.json` and pick your task:
   - **If any task has `"kind": "fix"` and `done: false`, take the lowest-numbered one.
     No exceptions, whatever else looks more urgent.** A review cycle appended those, and
     the loop's fix round ends when they are all closed — picking a feature task instead
     stalls that round against a condition it cannot meet.
   - Otherwise pick the highest-priority task with `done: false`. You decide priority:
     respect `depends_on` and prefer whatever unblocks the most other work.
3. Read **only your task's section** of the plan file — `plan_file` in `tasks.json`,
   lines `line_start` to `line_end`. Reading the whole plan wastes the context you need for
   the actual work.
4. Read any conventions files listed in `.ralph/stack.json` (`conventions_files`).
5. Check `.ralph/stack.json` `notes` for anything that will bite you (services that must run,
   required env vars, pre-commit hooks that rewrite code).

## Implement

Before writing code, search the codebase to confirm the task is not already done. Plans go
stale; a previous iteration may have covered it.

Follow the task's test-first structure:

**Red** — Write the failing tests first. Run them with `test_single_command` from
`.ralph/stack.json` (substitute your test file for `{file}`). Confirm they fail *for the
right reason* — missing behaviour, not a typo or an import error. A test that fails for the
wrong reason proves nothing.

**Green** — Write the minimum implementation that passes. Then run the full suite
(`test_command`) and confirm everything is green, not just your new tests.

**Refactor** — Only if the task calls for it. Stay green.

If the task is a schema/config/scaffolding task with no red/green cycle, just do the work and
run the verification the task specifies.

## Notice what needs refactoring — record it, do not fix it

You cannot see the whole codebase and you are not supposed to. But you *can* notice, from
inside one task, that something has gone wrong structurally — and you are the only one who
will ever be in a position to notice this particular thing.

**Record observations in `.ralph/refactor-backlog.md`. Do not act on them.** Fixing a
structural problem mid-task breaks the one-task rule, blows out the diff, and makes the commit
stop matching the task. A refactor pass task will drain this backlog later. Create the file if
it does not exist yet — you may be the first iteration to notice anything. One line per
observation:

```
- `path/to/file.go:120` — third near-copy of the response-envelope struct (see also x.go, y.go). Extract to a shared type.
```

If the backlog already names what you just hit, add nothing — a second mention is noise.

### Signals, in rough order of how often they matter

**The rule of three.** You just wrote the *third* near-identical thing — struct, error
construction, parse-and-format helper. Two is coincidence, three is a pattern that wants a
name. This is the highest-yield signal because each iteration only ever sees its own copy.

**You copied and adapted.** If you took code from another file and changed a few lines, you
have created the second or third copy. Say where the original is.

**The entrypoint grew logic.** CLI commands, HTTP handlers and controllers should parse input,
call into a package, and render the result. If you put a decision, a transformation, or a
multi-step workflow in that layer, it belongs in a package that can be tested without the
entrypoint. Note it even if the task told you to put it there.

**A file you touched is now large.** Run `wc -l` on the files you changed. Past ~400 lines,
say so and name the seam you would split on. You have just read the file, so you know where it
divides — nobody later will have that context for free.

**You wrote a comment to excuse the code.** A comment explaining why something is confusing,
surprising, or has to be done in an odd order is a design problem wearing a disguise. Record
the confusion, not just the comment.

**The test needed heavy setup.** If exercising one behaviour required a large scaffold, the
behaviour is coupled to too much. That is a design signal, not a testing inconvenience.

**Shotgun edit.** One conceptual change forced edits across three or more files. Whatever the
concept is, it has no home.

### Cheap check, every iteration

Before committing, on the files you touched:

```bash
wc -l <files you changed>
```

That single number catches the most common decay — a file quietly becoming the place
everything lands — and costs nothing.

## On success

1. Run the lint command from `.ralph/stack.json` if one is defined, and fix what it reports
   in the code you touched.
2. **Doc check** — did this change how someone uses the system?
   - New commands, endpoints, env vars, or workflows → update the conventions file
   - **Did you establish a pattern the next task will need?** Where a shared type lives, how
     errors are constructed, what belongs in the entrypoint vs. a package, a naming rule you
     invented — record it in the conventions file in one line. The next iteration starts with
     clean context and this file is the only way it can learn what you did. An unrecorded
     pattern gets re-invented slightly differently by every later task
   - **Never write a comment asserting a property no test enforces.** "This buffer is zeroed",
     "callers must hold the lock", "this is validated upstream" — either add the test that
     makes it true, or do not claim it. A comment that documents an intention as if it were an
     invariant is worse than silence: later readers, human and agent, will rely on it
   - Setup or install steps changed → update `README.md`
   - Behaviour intentionally diverges from the design doc → note it in the plan
3. **Edit the `tasks.json` file on disk** with the Edit tool: set your task's `"done": true`.
   Do NOT use TodoWrite or any built-in todo tool for this. `tasks.json` is a real file that
   `loop.sh` reads to decide when the build phase is finished. An in-memory todo is invisible
   to it and the loop will rebuild the same task forever.
4. Commit, including the updated `tasks.json`, with a message describing what you built.
5. **STOP.** Do not start another task.

## On failure

- Diagnose the actual failure before changing anything. Read the error.
- If a fix attempt does not work, use systematic debugging — form a hypothesis, test it,
  do not shotgun changes.
- If you are stuck on the same issue across two iterations, write what you tried into the
  task's section in the plan file, commit that note, and stop. The next iteration will see it
  and can pick a different task or a different approach.
- Never mark a task `done: true` with failing tests. The plan is the checkpoint; a false
  checkpoint corrupts every phase after it.

## Rules

- **ONE task per session, then STOP.**
- Read only your task's line range from the plan.
- Use subagents freely for research and reading. Use exactly ONE agent for builds and tests —
  never parallelize test runs, they contend for the same database, ports, and fixtures.
- Update `tasks.json` on disk (Edit tool) only after tests pass.
- All commands come from `.ralph/stack.json`. Never invent one.
- **Pre-commit hooks:** if a hook fails on pre-existing errors in files you did not touch,
  `--no-verify` is acceptable. If it fails on files you did touch, fix it.
- **During review-fix builds** (`RALPH_ITERATION_TYPE=review_fix`), `--no-verify` is never
  acceptable. Fix every failure including pre-existing ones — the review phase cannot pass
  until the suite is fully clean.

## When all tasks are complete

After marking the final task done, check whether every task in `tasks.json` has `done: true`.
If so, do a final pass over the conventions file to confirm it reflects reality, and commit
any doc updates.

Do NOT archive the plan, reset `tasks.json`, or open a PR here. Later phases handle that.
