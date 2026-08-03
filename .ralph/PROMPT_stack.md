# Stack Detection Mode

You are in STACK DETECTION mode. Do NOT implement anything and do NOT plan anything.

Your only job: figure out how this project is built, tested, and reviewed, then write `.ralph/stack.json`.

Every later phase reads that file instead of guessing. If you get it wrong, every downstream
iteration runs the wrong test command. Take the time to verify rather than assume.

## Orient

Use parallel subagents to inspect:

1. **Dependency manifests** — `Gemfile`, `package.json`, `go.mod`, `pyproject.toml`,
   `requirements.txt`, `Cargo.toml`, `pom.xml`, `mix.exs`, `composer.json`
2. **Test setup** — test/spec directories, test config files, existing CI workflows
   (`.github/workflows/*`, `.woodpecker.yml`, `Makefile`) — CI is the most reliable source
   of the real test and lint commands
3. **Project conventions** — `CLAUDE.md`, `AGENTS.md`, `CONTRIBUTING.md`. These describe *how
   this codebase is written*. A design doc, spec, or README describing *what to build* is *not*
   a conventions file — see the rule under `conventions_files` below.
4. **Source layout** — which top-level directories hold source vs. tests vs. generated code

## Verify, don't assume

Before writing the file, actually run the test command you intend to record:

```bash
<candidate test command>
```

It must execute — a passing suite, or a failing suite for real reasons. If it errors with
"command not found", missing dependencies, or a config error, that is not the right command.
Try the next candidate. For a greenfield project with no tests yet, record the command the
project *will* use per its conventions and note this in `notes`.

## Determine the base branch

```bash
git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|^origin/||'
git branch -r
```

This is the branch PRs will target and the branch reviews diff against. Usually `main` or
`master` — confirm which one actually exists on the remote.

## Choose reviewers

Pick the review lenses that fit this stack. Always include `security` and `spec-compliance`.
Add whatever else is warranted, e.g.:

- Rails/Django/Laravel → `rails` / `orm` (N+1 queries, mass assignment, migrations)
- Go/Rust → `concurrency` (data races, goroutine leaks, error handling)
- Frontend → `accessibility`, `performance`
- Anything with a database → `performance`
- Anything with multiple processes/services → `integration`

If the repo has custom reviewer definitions (subagents in `.claude/agents/`, or reviewer
sections in `CLAUDE.md`), prefer those names so the review phase can invoke them.

## Output

Write `.ralph/stack.json` exactly in this shape. Use `null` for anything genuinely absent —
never invent a command you have not verified.

```json
{
  "language": "Ruby",
  "framework": "Rails 8",
  "package_manager": "bundler",
  "test_command": "bundle exec rspec",
  "test_single_command": "bundle exec rspec {file}",
  "build_command": null,
  "lint_command": "bundle exec rubocop",
  "base_branch": "main",
  "source_dirs": ["app", "lib"],
  "test_dirs": ["spec"],
  "conventions_files": ["CLAUDE.md"],
  "reviewers": ["security", "spec-compliance", "rails", "performance"],
  "notes": "Anything a later phase must know: monorepo layout, required services, env vars, pre-commit hooks."
}
```

`test_single_command` must contain the literal token `{file}` where a path goes — the build
phase substitutes it to run one test file during the red step.

### `conventions_files` is the loop's only long-term memory — get it right

Every build iteration starts with clean context. `conventions_files` is the *sole* channel
through which one iteration tells the next how this codebase is written. Iteration 20 has no
other way to learn that iteration 3 already established a pattern.

Two hard rules:

1. **Never list a specification document.** A design doc, a README, or a research write-up
   answers *what to build*. Listing one here spends every iteration's context on the spec while
   teaching it nothing about the code — and worse, the build phase is instructed to *write* to
   the conventions file, so it will edit your design doc as if it were house style.
   The plan phase reads the spec; build iterations read their task's slice of the plan.
2. **If no conventions file exists, create one.** Do not fall back to whatever documentation
   happens to be present. Greenfield projects have nothing to discover — that is exactly when
   this matters most, because the whole codebase is about to be written by iterations that
   cannot see each other's work.

For a greenfield project, write a starter `CLAUDE.md` at the repo root (Claude Code loads it
automatically, so it cannot be skipped or budgeted away) containing what you already know: the
language, the layout, the test/build/lint commands, and a short "House rules" section for
iterations to extend. Then set `"conventions_files": ["CLAUDE.md"]`.

Seed the House rules with whatever the project's own docs already commit to — package boundary
rules, error-handling conventions, where shared types live. Leave a standing instruction that
patterns get recorded here as they are established.

Use `notes` for anything that would otherwise bite a later phase: a database that must be
running, a `.env` that must exist, a pre-commit hook that reformats code, a monorepo where
commands must run from a subdirectory.

## Commit

```bash
git add .ralph/stack.json && git commit -m "chore: detect project stack for ralph"
```

## Rules

- Detect only. Do not plan, do not implement, do not modify source files.
- Verify the test command by running it. An unverified command is a guess.
- Prefer commands from CI config over commands you infer from the manifest.
- If the project is a monorepo, record commands that work from the repo root, or put the
  required working directory in `notes`.
- Write the file even if detection is partial — record what you know, put gaps in `notes`.
