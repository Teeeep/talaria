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
