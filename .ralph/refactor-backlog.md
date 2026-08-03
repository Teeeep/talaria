# Refactor backlog

Observations recorded from inside a task, to be drained by a refactor pass. Do not act on
these mid-task.

- `internal/request/build.go:1` — 721 lines after Task 1 added server-variable substitution.
  Two clean seams: (a) base-URL and server resolution (`baseURL`, `absoluteBase`, `ServerURLs`,
  `firstServer`, `serverURL`, `serverVariable`, `maxServerURL`, `authorityChars`, `hasControl`)
  → `internal/request/server.go`; (b) the wire-safety charset helpers (`SplitsRequest`,
  `IsMethod`, `isFieldName`, `fieldNameChars`, `crlfProblem`, `elided`, `nameRule`) →
  `internal/request/wire.go`. What is left is the binder itself.
- `internal/request/build.go:134` — `binder.baseURL`'s doc comment used to claim a relative
  server URL "counts as no server"; the code has always failed it with *"is not an absolute
  http(s) URL"* instead. The claim was dropped in Task 1 rather than the behaviour changed —
  if the comment described the intended behaviour, that is a real (small) bug to fix.
- `internal/request/build.go:345` — correction to the seam above: `hasControl` now has a second
  caller, `parseHostEntry` in `hosts.go`, so it belongs in the shared `wire.go` half, not in
  `server.go` with the substitution code.
- `internal/corpus/replay.go:26` — the parameter locations `query`/`header`/`cookie` are now
  spelled out for the third time: `config.InQuery` etc. (auth.go:44), `internal/request`'s
  private `inQuery` aliases (build.go:24), and here, because corpus deliberately does not import
  config. `operation.Param.In` is the field they all describe, so `internal/operation` is the one
  package all three may import — move them there and alias from the other two.
- `cmd/talaria/auth.go:109` — `destinationWithholds` re-derives the base-URL precedence
  (`--base-url`, then the profile, then the spec) that `binder.baseURL` (request/build.go:137)
  already owns. Two copies of "where would this call go" is exactly how `auth check` and `call`
  came to disagree before. Export a `request.Destination(Inputs) string` — or have `auth check`
  build the same `Inputs` — and delete the copy.
- `cmd/talaria/history.go:1` — 657 lines, and the file now holds three unrelated things: the
  list/show view structs and their filters, the replay workflow (`buildReplay`), and the shared
  store plumbing (`openHistory`, `loadHistory`, `selectEntry`). `buildReplay` is the seam:
  it is 70 lines of decision-making in the command layer that could live beside
  `corpus.Entry.Replay`, leaving history.go as views + wiring.
- `cmd/talaria/history_test.go:1` — 1207 lines. Splits cleanly along the same seam as the source:
  list/show/filter cases vs. the replay contract.
- `cmd/talaria/auth.go:180` — `satisfied` and `unsatisfied` are the "can this spec be called at
  all" verdict, decided in the command layer, while `call` decides the same thing in
  `config.Resolve`. Task 4 had to change both in lockstep to keep them agreeing (DESIGN.md:329),
  which is the shotgun-edit signal: the rule has no home. Move both into `internal/config` beside
  `Covers` as `config.Unsatisfied(ops, creds) error`; `auth check` then wires spec → `Schemes` →
  `Unsatisfied`, and the agreement is structural instead of asserted.
- `cmd/talaria/call.go:453` — `callPayload` now renders four blocks (request, withheld, response,
  validation) and `callView` has grown a fourth optional field. It is still one function per
  block, but the next addition wants a builder rather than a fifth `if`.
