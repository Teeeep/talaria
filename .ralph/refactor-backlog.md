# Refactor backlog

Observations recorded from inside a task, to be drained by a refactor pass. Do not act on
these mid-task.

Task 6 (2026-08-03) drained every entry recorded during tasks 1–5. What is left below is what
that pass deliberately did not do, and why.

- `cmd/talaria/call.go:441` — `callPayload` renders four blocks (request, withheld, response,
  validation) and `callView` has four optional fields. **Not done:** the fifth block that would
  want a builder does not exist yet, and a builder for four `if`s is more machinery than the
  `if`s. Revisit when something adds a fifth.
- `cmd/talaria/call.go:170` and `cmd/talaria/call.go:319` — `newRedactors(cfg)` is called twice
  per invocation: once in `RunE` for history and the view, once inside `buildRequest` for the
  binder. Same config, same result, two constructions of the same firewall — and a future
  change that makes one of them configurable per-surface will silently apply to only one path.
  Build it once in `RunE` and pass it into `buildRequest`.
- `cmd/talaria/call.go:437` — `callPayload` now takes four arguments, three of which every
  caller derives from the same `RunE` locals (`req`, the response view, the validation result,
  the redactor). If a fifth appears, it wants a struct.
- `cmd/talaria/call.go` is 562 lines and now holds three things that are not the `call` command:
  `newRedactors`, `recordCall` and (task 8) `observed`, the `curl.Response` → `corpus.Observed`
  translator. `history_replay.go` reaches into all three. That is the seam — a `record.go` beside
  them holding the recording plumbing, leaving `call.go` the command, the binder wiring and the
  views.
- `cmd/talaria/history_replay.go:60` — `buildReplay` was going to move beside
  `corpus.Entry.Replay`. **Not done:** it needs `selectProfile`, `--base-url`, `--allow-host`
  and `config.Resolve`, so the move would make `internal/corpus` import `internal/config` —
  which the store must not do if it is to stay usable by the twin, which has no profiles. It
  was split into its own file instead; the two rules it holds (an off-set stored host is
  refused, a stored body goes via stdin) are one call each into a package that owns them.
- `internal/curl/config_test.go` is 650 lines against a 375-line `config.go`, and there is no
  `firewall_test.go` — the firewall half's tests (`checkSplit` refusals, `inlinable`, the
  cleanup/zeroing trio task 11 added) all sit in `config_test.go`. CLAUDE.md says tests split the
  same way as the source with the same names; this pair does not. That is the seam.
- `internal/curl/config.go:301` — `tempFile` writes the request body to a 0600 file that
  `cleanup` `os.Remove`s **without overwriting** it first. Task 11 made the in-memory buffers
  zero on every path; this is the same defect in a different medium, and a body can carry a
  credential the user put in it. Not one of the 25 findings, so task 11 left it alone: overwrite
  before remove, or say in the doc comment that the file's bytes are not scrubbed.
- `internal/corpus/store.go` is 434 lines and holds two concerns: the `Store` API (`Append`,
  `Read`, `Recording`, `Path`, id assignment) and the file mechanics underneath it (`readStore`,
  `write`, `trim`, `replace`, `lines`, `lineHead`, `stateDir`). The seam is that split — a
  `file.go` beside `store.go`, matching how `curl` splits `config.go` from `firewall.go`. The
  size bounds added in task 9 all live on the mechanics side.
