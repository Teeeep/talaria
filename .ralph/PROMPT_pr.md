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

Write `.ralph/pr_body.md` for one human reviewing alone, who did not watch the loop run and has
finite attention. That attention is what this document allocates.

**No filler.** Every line either changes where the reviewer looks or gets cut. No restating the
diff, no "this PR implements the changes described above", no section written to look complete.
An empty section is deleted, not filled — except where noted below, where absence is itself the
information.

**Never claim a check you did not run**, and **volunteer what you are unsure about**. A reviewer
who finds a problem you knew of and did not mention discounts every future PR.

```markdown
## Summary

[2-3 sentences, user-visible terms.]

## Read in this order

[3-6 entries, each a file with one clause on why it comes here. Start with whatever makes the rest
legible — usually a type, not a command.]

## Look hardest at

[Ranked, highest risk first. Each: what, why it is risky, and the question to ask. Cover, when
present: anything touching a credential or what reaches the wire; changes to a published contract;
where you deviated from the plan and why; code you rewrote more than once — that is the best
predictor you have; anywhere tests pass but you are not confident.

If nothing in the branch touches credentials, say that in one line. Silence reads as an omission.]

## Skim

[The mechanical bulk, with line counts, from `git diff --stat`: renames, moved code, generated
files, repeated table cases. "1,800 of 2,400 lines are a test table and a file move" is the single
most useful sentence in the document.]

## Contract changes

[Exit codes, envelope fields, flag names, output shapes, config keys — anything a script breaks on.
Before and after, one line each. "None" if none.]

## Tests and verification

- Suite: [count], [+/-N] on base. Added: [what hostile cases are now covered].
- **Deleted: every removed test, by name, with why.** Highest-suspicion edit in any diff.
- `<build_command>` / `<lint_command>` / `<test_command>` — results, all run after the final commit.
- Race: `.github/workflows/race.yml` runs it on this PR; say whether you also ran it locally.
- **Not verified:** [what rests on argument rather than a test. "Nothing" only if true.]

## Outstanding

[WARN/INFO from REVIEW_FINDINGS.md, file:line, one line each. Anything deliberately not fixed, and
why — the reviewer may overturn it. Tracked debt added: `grep -rn "TRACKED DEBT"`. "None" if none.]

---
🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

Gather the facts, do not recall them:

```bash
BASE=$(jq -r .base_branch .ralph/stack.json)
git diff --stat "$BASE"...HEAD
git log --oneline "$BASE"..HEAD
git diff "$BASE"...HEAD -- '*_test.go' | grep '^-func Test'
grep -rn "TRACKED DEBT" --include='*.go' --include='*.yml' .
```

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
