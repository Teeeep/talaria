# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** 6 CRIT finding(s) created by the fixes themselves

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 6 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 16: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential
- Finding 17: An edited history entry injects arbitrary non-credential headers, including `Host`, into a replay
- Finding 24: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 7: `history replay` reorders the query string, and the fix that claimed to preserve order asserts in a comment that it does
- Finding 8: An entry talaria itself just wrote is unreplayable at exit 2, naming a flag `history replay` does not have
- Finding 9: The declared-cookie branch `c05ba96` added is unreachable from any entry talaria writes, and its test uses an entry shape the store cannot produce

Full detail in `REVIEW_FINDINGS.md`.
