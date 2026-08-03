# Review Mode

You are in REVIEW mode. Review the full branch diff and produce structured findings.

You are not fixing anything in this session. You are producing an accurate list of what is
wrong. A later phase turns CRIT findings into tasks and fixes them.

## Orient

1. Read `.ralph/stack.json` — the reviewers to run are in `reviewers`, the base branch is in
   `base_branch`.
2. Read the design document (`spec_file` in `tasks.json`) — you cannot judge spec compliance
   without it.
3. Read the conventions files (`conventions_files`) — violating a documented project
   convention is a real finding.

## Get the diff

Use the base branch from `.ralph/stack.json`, not a hardcoded name:

```bash
BASE=$(jq -r .base_branch .ralph/stack.json)
git diff "$BASE"...HEAD
git diff --stat "$BASE"...HEAD
```

Review the **full branch diff**, not just the latest commit. A bug introduced three commits
ago and never touched since is still a bug on this branch.

## Run reviewers in parallel

Launch one subagent per reviewer listed in `.ralph/stack.json` `reviewers`, **all in
parallel**. Give each one the full diff, the changed-file list, and its checklist.

If the repo defines matching subagents in `.claude/agents/`, use those instead of generic ones —
they encode project-specific knowledge you do not have.

### Checklists by reviewer

**security** (always run)
- Injection: is user input parameterized in queries, shell calls, and template rendering?
- Output escaping: is user content escaped at render? Any unsafe raw/HTML passthrough?
- Path traversal: are file paths validated against an allowed root? Are symlinks resolved?
- AuthN/AuthZ: is every new endpoint access-controlled? Any missing ownership check?
- Mass assignment: are request params filtered before hitting a model?
- Secrets: any credential, key, or token committed in the diff?
- CSRF: are state-changing endpoints protected?

**spec-compliance** (always run)
- Completeness: is every requirement in the design doc implemented?
- Correctness: does behaviour match what the doc describes, not just what it names?
- Edge cases: are the cases the doc calls out actually handled?
- Contracts: do request/response shapes match what the doc specifies?
- Drift: does anything in the diff contradict the doc without a recorded reason?

**rails / orm** (ORM-backed projects)
- N+1 queries: are associations preloaded where iterated?
- Validations and constraints present for required fields?
- `dependent:` set on associations that own children?
- Migrations reversible and safe against existing data?

**performance**
- Queries inside loops; missing indexes on new WHERE/ORDER/JOIN columns
- Whole collections loaded into memory where batching would do
- Expensive repeated computation that should be cached

**concurrency** (Go/Rust/threaded)
- Data races on shared state; unsynchronized map/slice access
- Goroutine/thread leaks — is every spawned unit guaranteed to exit?
- Errors swallowed instead of propagated
- Context cancellation honoured

**integration** (multi-component systems)
- Does data produced by one component actually reach the next? Verify, don't assume.
- Environment parity: does this behave the same in tests as at runtime?
- Event chains: does every event in the sequence fire and land?

**accessibility** (frontend)
- Interactive elements reachable and operable by keyboard
- Labels and accessible names on controls; alt text on meaningful images
- Colour is not the sole carrier of meaning; contrast is adequate

## Findings Format

Write `REVIEW_FINDINGS.md`. Use this exact format — `loop.sh` parses it to decide whether to
keep cycling, and a malformed severity line means a real bug gets silently skipped.

```markdown
## Finding N: <Title>
- **Reviewer:** <reviewer name>
- **Severity:** CRIT
- **File:** path/to/file.ext:42
- **Description:** What is wrong and why it matters.
- **Suggested fix:** Concrete steps or code.
```

Severity:
- **CRIT** — bugs, security vulnerabilities, data loss, broken functionality, unmet design-doc
  requirements. These block the PR and will be fixed automatically.
- **WARN** — missing validation, performance problems, architectural concerns, spec drift.
  Recorded for a human; does not block.
- **INFO** — style, minor cleanup, doc gaps.

Be honest about severity. Inflating a WARN to CRIT burns a fix cycle on something that did not
need one. Downgrading a real bug to WARN ships it.

## Output

**If any CRIT or WARN findings exist:**
```bash
git add REVIEW_FINDINGS.md && git commit -m "review: N findings (M CRIT)"
```

**If all findings are INFO, or there are none:** treat it as a clean pass.
```bash
git rm -f REVIEW_FINDINGS.md 2>/dev/null; git commit --allow-empty -m "review: clean pass"
```

## Rules

- Review the full branch diff against the base branch from `.ralph/stack.json`.
- Run all reviewers in parallel, never sequentially.
- Every finding needs a `file:line` reference. "Somewhere in the auth code" is not a finding.
- One finding per distinct issue. If two reviewers find the same thing, report it once.
- Number findings sequentially from 1.
- Do not report issues already documented as known and accepted in the conventions files.
- Do not report on code outside the diff. Pre-existing problems are not this branch's findings.
