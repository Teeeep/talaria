# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** every CRIT is blocked on a design decision

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 2 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 5: The boundary guard has no entry for `corpus → config`, the rule this branch wrote down
- Finding 7: A profile's own `base-url` is not in the allowed host set, so the documented profile workflow withholds its own credential

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 1: A spec path key moves the URL authority past the host check
- Finding 2: A `--body` literal prints unredacted in `request.curl` while `request.body` beside it is redacted
- Finding 3: `document.b`'s abandoned backing arrays keep the resolved credential readable
- Finding 4: `document.cookies` accumulates a resolved credential into a `strings.Builder`
- Finding 5: The boundary guard has no entry for `corpus → config`, the rule this branch wrote down
- Finding 7: A profile's own `base-url` is not in the allowed host set, so the documented profile workflow withholds its own credential

Full detail in `REVIEW_FINDINGS.md`.
