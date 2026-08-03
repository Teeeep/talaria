# Building Mode

You are in BUILDING mode. Implement **exactly ONE task, then STOP.**

Do not continue to the next task. The loop starts a fresh session with clean context for it.
Starting a second task in this session is the single most common way this loop degrades —
context fills, quality drops, and the commit boundary stops matching the task boundary.

## Orient

1. Read `.ralph/stack.json` — the test, build, and lint commands for this project.
2. Read `tasks.json`. Pick the highest-priority task with `done: false`. You decide priority:
   respect `depends_on`, prefer whatever unblocks the most other work, and prefer CRIT-derived
   fix tasks over everything else.
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

## On success

1. Run the lint command from `.ralph/stack.json` if one is defined, and fix what it reports
   in the code you touched.
2. **Doc check** — did this change how someone uses the system?
   - New commands, endpoints, env vars, or workflows → update the conventions file
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
