# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** every CRIT is blocked on a design decision

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 1 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 5: `request.body` inlines a `--body @file` / `--body -` body on stdout while `request.curl` beside it references it

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 2: `document.auth` and `resolve` concatenate the resolved credential into Go strings nothing can zero

Full detail in `REVIEW_FINDINGS.md`.
