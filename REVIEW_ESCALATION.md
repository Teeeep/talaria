# Review escalation

The review loop stopped in cycle 1 and is handing back to a human.

**Reason:** 4 CRIT finding(s) repeat after a failed fix

## CRIT history

| Cycle | CRIT |
|---|---|
| 1 | 5 |

## What needs a decision

These findings are marked `Blocked-by: design` — fixing them means choosing a
policy the design doc does not state. Decide the policy, amend the design doc,
then re-run with `--from review`.

- Finding 10: The allowed host set ignores the URL scheme, so an agent can downgrade TLS and keep the credential
- Finding 11: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them
- Finding 13: A `--body @file` credential still reaches the agent on stdout, defeating §3.4's reason for referencing the file
- Finding 14: An edited history entry injects arbitrary non-credential headers into a replay
- Finding 23: `--allow-host host:443` also admits `http://host:80`

These findings are repeats — a previous cycle's fix did not hold. Re-applying the
same approach will fail again; they need a different one.

- Finding 1: A stored history header supplies the credential a replayed request authenticates with
- Finding 2: A credential talaria marks sensitive but did not resolve from a scheme is sent to any host
- Finding 4: A spec's `securitySchemes.name` reaches the emitted curl unchecked — `--dry-run` exits 0 with a CRLF-injected reproduction
- Finding 5: `auth check` reports a scheme satisfied for a `--base-url` that `call` refuses outright
- Finding 6: A server variable with no `default` substitutes to the empty string instead of omitting the server
- Finding 11: Server-variable `enum` alternatives are not in the allowed host set, and the message denies the spec declares them
- Finding 12: Path- and operation-level `servers[]` are not in the allowed host set
- Finding 13: A `--body @file` credential still reaches the agent on stdout, defeating §3.4's reason for referencing the file

Full detail in `REVIEW_FINDINGS.md`.
