# Pull Request Mode

You are in PR mode. The work is built and has passed review with no CRIT findings.
Open the pull request. Do NOT write any more code.

## Orient

1. `.ralph/stack.json` — `base_branch` is the PR target.
2. The design document (`spec_file` in `tasks.json`) — what this was supposed to deliver.
3. `IMPLEMENTATION_PLAN.md` and `tasks.json` — what was actually built.
4. `REVIEW_FINDINGS.md` if it exists — the non-CRIT findings a human still needs to see.

```bash
BASE=$(jq -r .base_branch .ralph/stack.json)
git log --oneline "$BASE"..HEAD
git diff --stat "$BASE"...HEAD
```

## Write the PR body

Write `.ralph/pr_body.md`. Write it for a reviewer who has not read the design doc and was
not watching the loop run.

```markdown
## Summary

[2–4 sentences: what this delivers and why. Lead with user-visible impact, not file counts.]

## What changed

- [Substantive change, grouped by area — not a file listing]
- [Another]

## Design doc

`<path to design doc>`

## Verification

- Test command: `<test_command from stack.json>`
- Status: [what the final suite run reported]

## Review

Automated review ran <N> cycle(s), no CRIT findings outstanding.

[If REVIEW_FINDINGS.md has WARN/INFO entries, list them here under
"Outstanding non-blocking findings" with file:line and one line each.
If there are none, say "No outstanding findings."]

## Notes for the reviewer

[Anything genuinely worth a human's attention: deliberate tradeoffs, deferred work,
assumptions the loop made, places where implementation diverged from the design doc and why.
If there is nothing, omit this section entirely.]

---
🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

Be accurate about verification status. If the last suite run had failures, say so plainly —
do not write "all tests pass" unless you confirmed it. An inaccurate PR body is worse than a
sparse one, because it costs the reviewer their trust in every other line.

## Create the PR

```bash
BASE=$(jq -r .base_branch .ralph/stack.json)
BRANCH=$(git branch --show-current)

git push -u origin "$BRANCH"

gh pr create \
  --base "$BASE" \
  --head "$BRANCH" \
  --title "<concise imperative title>" \
  --body-file .ralph/pr_body.md
```

## Record the URL

`loop.sh` verifies the PR was actually created by reading this file. If it is missing or
empty, the run is reported as failed even though the PR may exist.

```bash
gh pr view --json url -q .url > .ralph/pr_url.txt
cat .ralph/pr_url.txt
```

Confirm the file is non-empty before finishing.

## If PR creation fails

Do not retry blindly. Diagnose:

- **"no commits between base and head"** — nothing was actually built. Report this; do not
  force a PR.
- **"a pull request already exists"** — fine. Record the existing URL with
  `gh pr view --json url -q .url > .ralph/pr_url.txt` and finish.
- **auth failure** — `gh auth status`. Report it; do not attempt to re-authenticate.
- **base branch does not exist** — re-check `base_branch` in `.ralph/stack.json` against
  `git branch -r`.

## Rules

- Do not write, fix, or refactor code in this phase.
- Do not amend, squash, or rebase existing commits.
- Title is imperative and specific: "Add session replay to dashboard", not "Updates".
- Never claim tests pass without having confirmed it.
- Always write `.ralph/pr_url.txt`.
