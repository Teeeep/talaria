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
- `cmd/talaria/history_replay.go:60` — `buildReplay` was going to move beside
  `corpus.Entry.Replay`. **Not done:** it needs `selectProfile`, `--base-url`, `--allow-host`
  and `config.Resolve`, so the move would make `internal/corpus` import `internal/config` —
  which the store must not do if it is to stay usable by the twin, which has no profiles. It
  was split into its own file instead; the two rules it holds (an off-set stored host is
  refused, a stored body goes via stdin) are one call each into a package that owns them.
