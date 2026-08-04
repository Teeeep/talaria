# Review escalation

The review loop stopped in cycle 2 and is handing back to a human.

**Reason:** CRIT count did not fall between cycles (1 → 2)

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 1 |
| 2 | 2 |

## What needs a decision

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 5: `auth check` reports a malformed `TALARIA_AUTH_BASIC` as satisfied where `call` exits 5
- Finding 6: An unreadable history store is exit 1 — the code AGENT.md tells the agent to retry — and `44c0264` added a third instance rather than classifying it
- Finding 7: `Entry.Replay` ignores `HeadersTruncated`, so a capped entry replays into a different request with nothing on any channel
- Finding 8: `history show` prints a truncated body as if it were whole
- Finding 9: The truncated-body replay refusal names `MaxBody` rather than where the cut actually happened
- Finding 10: `config.(*Profile).ReferencesEnv` is dead code whose comment claims a live security role
- Finding 11: The spec cache read still blocks in `open(2)`, and the branch now contradicts itself about it
- Finding 17: Three tests order themselves with a sleep
- Finding 18: Stale number in a test comment — the lock deadline is a minute, not five seconds

Full detail in `REVIEW_FINDINGS.md`.
