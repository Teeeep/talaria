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
3. **Project conventions** — `CLAUDE.md`, `AGENTS.md`, `README.md`, `CONTRIBUTING.md`
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
