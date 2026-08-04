# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** 1 CRIT finding(s) created by the fixes themselves

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 9 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 19: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential
- Finding 20: The `Host` header is settable from `--header` and from a stored entry, and it decides who receives the credential without passing the host check
- Finding 31: DESIGN.md §5a's allowed host set omits the active profile's own `base-url`, so §6's worked example silently withholds every credential
- Finding 32: A broken spec's security requirement maps to exit 2, which DESIGN.md §4 defines as a usage error

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 10: `history replay` reorders the query string, and the fix that claimed to preserve order asserts in a comment that it does
- Finding 11: An entry talaria itself just wrote is unreplayable at exit 2, naming a flag `history replay` does not have
- Finding 12: The declared-cookie branch `c05ba96` added is unreachable from any entry talaria writes, and its test uses an entry shape the store cannot produce

Full detail in `REVIEW_FINDINGS.md`.
