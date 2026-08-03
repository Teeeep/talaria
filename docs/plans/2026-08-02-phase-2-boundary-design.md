# talaria — Phase 2 design: the boundary made real

*Status: design · 2026-08-02 · successor to [DESIGN.md](../design/DESIGN.md) v0.3*

> **Do not apply the DESIGN.md amendments in §7 of this document until Task 33 of
> [IMPLEMENTATION_PLAN.md](../../IMPLEMENTATION_PLAN.md) has passed.** A ralph loop is executing
> against DESIGN.md as its source of truth. Editing it mid-flight either changes the ground under
> a running build or invites the loop to drift into work planned here.

---

## 1. What "phase 2" means

DESIGN.md §7 numbers eight roadmap phases. IMPLEMENTATION_PLAN.md covers roadmap Phases 1–4 and
stops at the §7 decision gate. **This document treats all of that planned work as "phase 1" and
designs what comes immediately after it.**

The agreed ordering for what follows is: **install and use it → deepen the CLI → the twin.** This
document covers only the first. The twin (roadmap 5–8) stays gated, as §7 intends.

## 2. The problem this phase exists to solve

When Task 33 passes, talaria has `list`, `describe`, `search`, `uses`, `call`, `history`, `run`,
`auth check`, response validation, the full `SecretRef` credential firewall with a canary leak
suite, `AGENT.md`, and CI. It has no installation story, has never been pointed at a real API, and
no agent has ever driven it.

More importantly, it delivers a weaker guarantee than §1 promises. Two claims are conflated
throughout DESIGN.md:

| | Claim | Guaranteeable by talaria? |
|---|---|---|
| **A** | talaria never *emits* a credential value — not in stdout, stderr, dry-runs, emitted curl, errors, history, or recordings | **Yes.** Mechanically, via `SecretRef`, the canary suite, and the absence of any `--show-secrets` flag. Phases 1–4 deliver this |
| **B** | the agent cannot *obtain* the credential | **No.** This is a property of the operating system, not of the binary |

§1 promises B (*"an agent can explore, call, and test that API without ever being given your
credentials"*). Phases 1–4 deliver A. Claim A is already a real differentiator — Bruno, Insomnia
and Postman all hand the value to whoever runs them — but it is not what the vision says.

The first consumer of phase 2 is **Casper's own Claude Code sessions**, which makes the gap
concrete rather than theoretical: a Claude Code skill runs Bash as `casper`, in `casper`'s
environment. The agent can read any env var and any file that `casper` can read. In that
deployment claim B is void.

## 3. Decisions

### 3.1 The boundary is enforced by uid separation, not by the agent harness

**Decision: the supported enforced deployment runs the agent as a second uid, with a single sudo
rule granting it the right to invoke talaria as the secret owner.**

```
casper-agent ALL=(casper) NOPASSWD: /usr/local/bin/talaria
```

The agent can *invoke* talaria as `casper`; it cannot read `casper`'s config, environment, or run
any other command as `casper`. Enforcement is the kernel's.

**Rejected: relying on Claude Code `Deny` permission rules** on `~/.config/talaria/**`. This was
the initial recommendation and it is wrong for a tool whose reason to exist is a guarantee. It is
a policy control living in another tool's configuration file, invisible when it rots, and absent
entirely if the binary is driven by any other agent harness.

**Rejected: a credential broker at the same uid** (ssh-agent shaped, secrets in memory, unix
socket). It buys nothing. A process already running as `casper` connects to the socket and asks.
It is strictly *worse* than a config file, because a file path is something a deny rule can name
and a socket protocol is not. ssh-agent is precedent for ergonomics — typing a passphrase once —
not for hiding a key from your own shell.

**Rejected: broker + setgid binary** (broker under its own uid, socket group-restricted, talaria
setgid so it alone may connect). This is a genuine OS-enforced boundary, and it is the design most
people reach for. The price is a setgid binary that parses untrusted YAML and OpenAPI documents
fetched over HTTP — a large attack surface behind a privilege bit — plus a service user, a group,
a systemd unit, and install-time root. `go install` stops working. The sudo rule achieves the same
separation with no new code and no privilege bit.

**Rejected: OS keyrings.** libsecret/gnome-keyring is uid-scoped and therefore bypassable by
anything running as the user. macOS Keychain ACLs *are* per-binary and would be a genuine
boundary without uid separation — but the sessions that matter run in tmux on the Debian VPS. See
§8 (deferred).

**Consequence:** on Linux there is exactly one mechanism that closes claim B — the agent runs as a
different uid than the one that can read the secret. Everything else is theater.

### 3.2 In enforced mode, talaria reads only files owned by the calling uid

Uid separation is void while talaria has arbitrary-file-read primitives. Today it has at least
two:

```
sudo -u casper talaria call spec.yaml op --body @/home/casper/.config/talaria/config.yaml --dry-run
```

`--body @path` reads a file and `--dry-run` prints the request. `--spec path` is the same shape.
The boundary is tunnelable in one command.

**Decision: when `internal/boundary` reports `enforced`, path-taking flags refuse any file not
owned by the calling uid.**

Considered and rejected: emulating the caller's permissions properly. That requires walking every
parent directory's execute bit plus the caller's supplementary groups, and `faccessat` only
checks real-vs-effective uid, not an arbitrary one. Dropping privileges per-thread in Go is worse
— `setuid` affects only the calling thread and the Go runtime schedules across threads.

The ownership rule is one `stat`, correct by construction, and conservative in the safe direction
(root-owned world-readable files are also refused). The escape hatch already exists and is *more*
correct: `--body -` has the agent's own shell perform the `open()`, at the agent's own uid.

In `policy-only` mode the guard is a no-op, so nothing changes for a single-uid install.

### 3.3 `doctor` returns a verdict, not a checklist

**Decision: `talaria doctor` reports one of three verdicts** — `enforced`, `policy-only`, `none` —
rather than a list of green ticks. A single-uid install reports `policy-only` explicitly, stating
that the secret is reachable by anything running as the user.

The point is that "this needs work in the future" stays visible in the tool's own output instead
of decaying into an unexamined assumption.

### 3.4 This phase does not finish the boundary, and says so

Phase 2 ships talaria's own half of the guarantee — the read-primitive constraint and the verdict
— plus a documented deployment. It does not install the uid separation, does not solve macOS, and
does not close secrets-in-response-bodies. Those are recorded in §8 as tracked future work, not
omitted.

## 4. Deliverables

| # | Deliverable | Size |
|---|---|---|
| 2a | `internal/boundary` + `internal/fileguard`; guard calls at all path-taking flags | ~1–2 days |
| 2b | `talaria doctor [--require enforced]` | ~1 day |
| 2c | Documented uid-separated deployment (sudoers snippet), verified by `doctor` | ~half a day |
| 2d | Packaging — version stamping via `-ldflags -X`, `make install`, `v0.1.0` tag | hours |
| 2e | Claude Code skill at `~/.claude/skills/talaria/`, wrapping `AGENT.md` | ~1 day |
| 2f | Real-spec dogfooding → written findings doc | open-ended; the real work |

**Not in phase 2** — cross-platform release matrices, the curl-able install script, the Homebrew
tap, the pi package. Those serve strangers, and the first consumer is this machine. They wait for
a public v0.1 that has survived contact with a real API.

The repo is already public at `github.com/Teeeep/talaria`, so `go install
github.com/Teeeep/talaria/cmd/talaria@latest` works as soon as a tag exists. Self-distribution is
nearly free.

### 2f is what feeds the §7 twin gate

DESIGN.md §7: *"Do not start [5–8] on faith; start them because using Phases 1–4 made the absence
of a twin painful."* That evidence cannot exist without usage, and usage cannot exist without
2a–2e. The findings doc from 2f — not intuition — is what later answers whether the twin earns its
keep, and what feeds the "deepen the CLI" phase that precedes it.

Dogfooding targets: large public specs (GitHub, Stripe) for `describe` quality, operationId
synthesis collisions, and load time on multi-megabyte documents; and local services (loop-tracker,
Woodpecker CI) for the end-to-end agent loop. Real 3.0 specs also settle the libopenapi-validator
strictness question that IMPLEMENTATION_PLAN.md left open by design.

## 5. Component design

Two new leaf packages. Both sit below everything and neither imports `curl`, `corpus` or `twin`,
so DESIGN.md §5's boundary rule is untouched.

### `internal/boundary`

Detects the deployment mode from process state alone — caller uid from `SUDO_UID`/`PKEXEC_UID`,
ours from `os.Getuid()`. Returns:

- `enforced` — caller uid ≠ our uid
- `policy-only` — single uid
- `none` — running as root, or the config file is group/world readable

One function, no I/O beyond a stat. Both `fileguard` and `doctor` read its result. The uid lookups
are injectable so tests do not need root.

### `internal/fileguard`

Implements §3.2. In `enforced` mode, `Check(path)` stats the file and refuses unless its owner uid
equals the calling uid. In `policy-only` and `none` it returns nil. Refusal is a `clierr.Usage`
(exit 2) whose message names the rule and points at the `-` / stdin alternative.

Call sites:

- `internal/request/body.go` — `--body @path` (Task 18, not yet built)
- `internal/spec/source.go` — `--spec path` and the positional spec argument (Task 6, built)
- `internal/config/config.go` — the profile path

Any future flag accepting a filesystem path is a call site. This should be stated in
`AGENT.md` and in the package doc comment.

### `cmd/talaria/doctor.go`

Composes `boundary`, the curl version preflight from Task 17, file-mode checks on the config and
history stores, and a warning when `TALARIA_AUTH_*` variables are present in the environment under
`enforced` mode (they should live only in the profile). Renders through the Task 2 envelope with a
top-level `verdict` field.

`--require enforced` exits non-zero when the verdict is weaker than the requirement, so CI and
agent preflight can branch on it.

## 6. Testing

Three layers, the last being the one that matters.

1. **`boundary` unit tests** with injected uid pairs covering all three verdicts, including the
   root case and the loose-permissions case.
2. **`fileguard` unit tests** over `t.TempDir()` files. Real `chown` requires root, so the
   ownership lookup is an injected interface; a table covers owned/not-owned × each mode.
3. **Canary suite extension** (builds on Task 24). Place a canary secret in the profile file, then
   attempt to read it back through every path-taking flag — `--body @profile`, `--spec profile`,
   and each with `--dry-run`, `--output json`, and via `history show` afterwards — asserting
   refusal under `enforced` and asserting the canary appears in no output surface. **This is the
   test that would have caught the tunnel in §3.2**, and like the rest of the canary suite it
   gates every release thereafter.

## 7. Pending DESIGN.md amendments

**Apply only after Task 33 passes.** Until then DESIGN.md v0.3 remains the loop's source of truth.

| § | Amendment |
|---|---|
| §1 Vision | Split claims A and B (§2 above). State plainly which one phases 1–4 deliver and which requires the enforced deployment |
| §3 principle 0 | Same split. "Secrets never reach the agent" becomes "talaria never emits a secret; the enforced deployment additionally prevents the agent obtaining one" |
| §4 CLI surface | Add `talaria doctor [--require enforced]` under Meta |
| §4 exit codes | Add **6 — boundary requirement not satisfied**. None of the existing five fit: it is not a usage error, not a spec error, not a validation failure, not a missing credential |
| §5 Architecture | Add `internal/boundary` and `internal/fileguard` to the package list |
| §5a leak channels | New row: `--body @path` / `--spec path` as arbitrary-file-read primitives under privilege separation; countermeasure is the ownership rule |
| §5a threats not covered | Rewrite. Enforced mode moves *out* of "not covered". Record the rejected designs from §3.1 with their reasons so they are not re-proposed |
| §7 Roadmap | Insert this phase between 4 and 5. Note that it absorbs the self-install slice of the distribution line |
| §8 Open questions | Add macOS: Keychain per-binary ACLs are a stronger mechanism with no Linux equivalent — cross-platform story unsolved |

### No rework of Tasks 1–33 is implied

The guard is additive at three call sites and is a no-op in `policy-only` mode. Nothing already
built becomes wrong: `--spec` (Task 6, complete) and `--body @` (Task 18, pending) each gain a
guard call later, and `AGENT.md` (Task 25) gains a section. A future reader should not conclude
the loop shipped something broken.

## 8. Deferred, tracked

Recorded so they stay visible rather than becoming assumptions:

- **macOS.** Keychain ACLs are scoped to a code-signed binary rather than a uid, which would give
  a genuine boundary without uid separation. There is no Linux equivalent. Unsolved.
- **Secrets in response bodies.** DESIGN.md §5a's standing gap — the tool cannot know a field is
  sensitive unless configured. Mitigated by default `Set-Cookie` redaction and configurable body
  paths; not solved.
- **Installing the boundary.** talaria documents and verifies the uid-separated deployment; it
  does not create the user, the sudoers entry, or the systemd units. A `talaria install-boundary`
  that emits or applies them is possible later.
- **Rejected designs** (§3.1) — same-uid broker, broker + setgid, uid-scoped keyrings. Kept with
  reasons.
- **Public distribution** — release matrix, install script, Homebrew tap, pi package.

## 9. Successor phases

Unchanged in order, restated for context:

1. **Deepen the CLI** — driven by the 2f findings doc. Candidates include request chaining via
   OpenAPI `links`, search ranking, spec overlays. DESIGN.md §9 lists several as non-goals; the
   findings decide whether that stance survives contact.
2. **The twin** — roadmap 5–8, still behind the §7 gate.
