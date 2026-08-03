# Phase 2b — the enforced boundary

*Design document for one autonomous build. Self-contained. Depends on phase 2a being merged.*

## Why this phase exists

talaria's stated reason to exist is that an agent can call an API **without ever being given
your credentials**. Three distinct claims hide inside that sentence:

| | Claim | Guaranteeable by talaria? |
|---|---|---|
| **A** | talaria never *emits* a credential — stdout, stderr, dry-run, emitted curl, errors, history | **Yes**, mechanically. Shipped |
| **A′** | talaria never *transmits* a credential to a host the spec does not declare | **Yes**, mechanically. Phase 2a |
| **B** | the agent cannot *obtain* the credential by other means | **No** — a property of the OS |

DESIGN.md §1 promises B while the code delivers A. **This phase closes as much of B as a binary
can close, and makes the rest visible instead of implied.**

The deployment that matters is Claude Code sessions running as the same user that owns the
secret. In that setup an agent can read the profile file or the environment directly, and no
architecture inside the binary prevents it.

## The decision already made

**The supported enforced deployment runs the agent as a second uid**, with one sudo rule letting
it invoke talaria as the secret owner:

```
casper-agent ALL=(casper) NOPASSWD: /usr/local/bin/talaria
```

The agent can *invoke* talaria as the owner; it cannot read the owner's config, environment, or
run anything else as them. Enforcement is the kernel's.

**Rejected, with reasons — do not re-propose:**

- *Agent-harness deny rules* (e.g. Claude Code `Deny` on `~/.config/talaria/**`). A policy
  control in another tool's config file: invisible when it rots, absent under any other harness.
- *A credential broker at the same uid.* Buys nothing — a process running as the user connects
  to the socket and asks. Strictly worse than a file, because a path is something a deny rule can
  name and a socket protocol is not. ssh-agent is precedent for typing a passphrase once, not for
  hiding a key from your own shell.
- *Broker plus a setgid binary.* A real boundary, at the price of a setgid binary that parses
  untrusted YAML and OpenAPI documents fetched over HTTP, plus a service user, a group, a unit
  file and install-time root. `go install` stops working.
- *OS keyrings.* libsecret and gnome-keyring are uid-scoped, so anything running as the user
  bypasses them.

## Scope

**In:** `internal/boundary`, `internal/fileguard`, `talaria doctor`, exit code 6, the guard call
sites, and the documented deployment.

**Explicitly out:**

- Creating the second uid, the sudoers entry, or any systemd unit. talaria *documents and
  verifies* the deployment; it does not install it.
- Packaging, release binaries, the Claude Code skill, dogfooding — phase 2c.
- Anything under `internal/twin`.
- The macOS Keychain question (see the end).

## Commands

| Purpose | Command |
|---|---|
| All tests | `go test ./...` |
| One package | `go test ./<dir>/...` |
| Build | `go build ./...` |
| Lint | `test -z "$(gofmt -l .)" && go vet ./...` |

House rules are in `CLAUDE.md`. Note the §5 package boundary rule: `operation` and `validate` may
not import `curl`, `corpus` or `twin`, and `corpus` may not import `curl`. Both new packages are
leaves and may be imported anywhere.

## The work

### 1. `internal/boundary`

Detects the deployment mode from process state alone. Caller uid from `SUDO_UID`/`PKEXEC_UID`,
ours from `os.Getuid()`. Returns one of:

- `enforced` — caller uid ≠ our uid
- `policy-only` — single uid
- `none` — running as root, or the config file is group/world readable

One function, no I/O beyond a stat. The uid lookups must be injectable so tests need no root.

### 2. `internal/fileguard`

> **In `enforced` mode, talaria reads only files owned by the calling uid.**

This is the rule that keeps uid separation from being tunnelled. Without it:

```
sudo -u owner talaria call spec.yaml op --body @/home/owner/.config/talaria/config.yaml --dry-run
```

reads the secret straight back out. `--body @path` reads a file and `--dry-run` prints the
request; `--spec path` is the same shape.

**Rejected: emulating the caller's permissions properly.** That requires walking every parent
directory's execute bit plus the caller's supplementary groups, and `faccessat` checks only
real-vs-effective uid, not an arbitrary one. Per-thread privilege dropping in Go is worse —
`setuid` affects one thread and the runtime schedules across threads.

The ownership rule is one `stat`, correct by construction, and conservative in the safe direction
(root-owned world-readable files are also refused). The escape hatch already exists and is more
correct: `--body -` has the caller's own shell perform the `open()` at its own uid.

In `policy-only` and `none` the guard is a no-op, so single-uid installs are unaffected.

Call sites — every flag that takes a filesystem path:

- `internal/request/body.go` — `--body @path`
- `internal/spec/load.go` and `source.go` — `--spec path`, the positional spec, cache reads
- `internal/config/config.go` — the profile path

Refusal is a `clierr.Usage` (exit 2) naming the rule and pointing at the `-` alternative. Record
in `CLAUDE.md` that any future path-taking flag is a call site.

### 3. `talaria doctor`

Composes `boundary`, the existing curl version preflight, file-mode checks on the config and
history stores, spec-cache age, and a warning when `TALARIA_AUTH_*` variables are present in the
environment under `enforced` mode. Renders through the standard `talaria/v1` envelope.

**It returns a verdict, not a checklist:** `enforced`, `policy-only`, or `none`. A single-uid
install must report `policy-only` explicitly and say that the secret is reachable by anything
running as the user. Stating the weakness in the tool's own output is the point — it is what stops
"we'll do the rest later" from decaying into an unexamined assumption.

`--require enforced` exits non-zero when the verdict is weaker than required, so CI and agent
preflight can branch on it.

### 4. Exit code 6

DESIGN.md §4's table currently ends at 5. Add **6 — boundary requirement not satisfied**, for
`doctor --require`. None of the existing five fit: it is not a usage error, not a spec error, not
a validation failure, not a missing credential. Verified unused as of v0.5.

### 5. Document the deployment

The sudoers snippet, what it does and does not protect, and how to verify it with `doctor`. This
is documentation, not installation.

## Testing

1. **`boundary`** — injected uid pairs across all three verdicts, including root and the
   loose-permissions case.
2. **`fileguard`** — `t.TempDir()` files with an injected ownership lookup, since real `chown`
   needs root. Owned/not-owned × each mode.
3. **Canary suite extension** — place a canary in the profile file, then attempt to read it back
   through every path-taking flag under `--dry-run`, `--output json`, and `history show`,
   asserting refusal under `enforced` and that the canary reaches no output surface. **This is the
   test that would have caught the tunnel**, and like the rest of the canary suite it gates every
   release after it.

## DESIGN.md amendments this phase applies

Six, held back until now:

| § | Amendment |
|---|---|
| §1, §3.0 | Split claims A / A′ / B. State which the code delivers and which needs the enforced deployment |
| §4 | Add `talaria doctor [--require enforced]` and exit code **6** |
| §5 | Add `internal/boundary` and `internal/fileguard` |
| §5a | New leak-channel row: path-taking flags as arbitrary-file-read primitives under privilege separation; countermeasure is the ownership rule |
| §5a | Rewrite "threats explicitly not covered" — enforced mode moves *out*. Record the rejected designs above with their reasons |
| §7, §8 | Insert this phase; add macOS Keychain as the unsolved cross-platform question |

## What this phase does not claim

It closes B **only in the enforced deployment**, which talaria documents and verifies but does not
install. Single-uid installs stay `policy-only` and the tool says so. Secrets arriving in response
bodies remain uncovered — the tool cannot know a field is sensitive unless configured.

**macOS is unsolved.** Keychain ACLs scope to a code-signed binary rather than a uid, which would
be a genuine boundary without uid separation, and there is no Linux equivalent. Out of scope here;
recorded so it is not mistaken for an oversight.
