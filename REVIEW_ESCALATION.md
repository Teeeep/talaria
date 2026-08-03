# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** 1 finding(s) repeat after a failed fix

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 8 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 18: Replay's resolvable env-var set extends past the `TALARIA_AUTH_*` namespace
- Finding 25: The remote spec cache never expires and cannot be bypassed

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 2: `history replay` takes its target host from the stored entry

Full detail in `REVIEW_FINDINGS.md`.
