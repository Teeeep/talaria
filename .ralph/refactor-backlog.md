# Refactor backlog

Observations recorded from inside build iterations. Do not act on these mid-task; a
refactor pass drains them later.

Task 6 (compaction of tasks 1–5) drained the duplication entries. What remains is
almost all *file size*, and every fix for it is a file split — which Task 6's brief
("no new files") put out of scope. A later refactor pass without that constraint
should take them.

- `internal/request/build.go:1` — 596 lines and growing; it holds the whole binder. Seam:
  the base-URL resolution (`baseURL`, `firstServer`, the candidate list) is a self-contained
  concern with its own security checks and could move to `internal/request/baseurl.go`.
  Not done in Task 6: new file.
- `internal/request/request_test.go:1` — 1129 lines, one file for every behaviour of the
  package. Splits cleanly along the binder's stages: params/headers, base URL, auth.
  Not done in Task 6: new file.
- `cmd/talaria/history_test.go:1` — 1259 lines, and `internal/e2e/e2e_test.go` 1028. Both split
  along the same line the source now does: listing/filtering vs. replay. The replay half is
  now a unit test of `internal/replay` living in `package main`, so the split has a
  destination: `internal/replay/replay_test.go`. It needs a copy of `cmd/talaria/testdata/call.yaml`
  beside it. Not done in Task 6: new files.
- `cmd/talaria/call_test.go:allowHost` and `internal/canary/canary_test.go:canaryHost` and
  `internal/e2e/e2e_test.go:hostOf` — three near-copies of "a test server's authority, for
  --allow-host", in three packages that cannot share a helper without a testing-support
  package. Task 6 folded the fourth (`cmd/talaria/history_test.go:callHost`) into `allowHost`,
  which is as far as it goes without that package.
- `internal/config/auth.go:1` — 478 lines and it is still three concerns: the
  Credential/Coverage model, the resolution rules (`Resolve`, `Covers`, `Schemes`,
  `credentialFor`), and the env-var/profile naming machinery (`envSuffix`, `envRef`,
  `profileRef`, `checkEnvCollisions`). The naming machinery is the clean seam: it has no
  dependency on `operation` or `spec` and would test standalone as
  `internal/config/envnames.go`. Not done in Task 6: new file.
- `internal/canary/canary_test.go:73` — `readSources` does not do what its comment claims. The
  suite stayed `(cached)` after edits to `internal/curl/render.go` and to `internal/spec/servers.go`
  (probed twice on 2026-08-04, only `-count=1` re-ran it), so the gate *can* go stale exactly the
  way the comment says it cannot. The canary package imports none of the code it tests, so nothing
  but the testlog would invalidate it. Either make the dependency real or drop the claim — the
  comment is currently an invariant no test enforces. Left for Task 10, which owns the canary gate.
- `internal/curl/render.go:bodyArgs` + `internal/e2e/e2e_test.go:TestAPreviewedFileBodyLosesTheNewlinesTheCallSends`
  — a `--body @file` body renders as `--data @path`, and curl strips newlines out of a file read
  that way, while the executed call sends the file's bytes verbatim. So the previewed command and
  the call send different bytes for a pretty-printed or signed payload. DESIGN.md §3.4 names
  `--data`; widening it to `--data-binary` is a design decision, not a bug fix. The test pins the
  divergence so it is known rather than discovered. Left: needs a design-doc change, not a refactor.
- `internal/curl/config.go:1` — 431 lines. Seam: the `document` type and its directive writers
  (`build`, `auth`, `cookies`, `body`, `directive`, `tempFile`, `escapeDirective`) are a distinct
  concern from the `Options`/`Capture`/`BuildConfig` entry points; they would move whole to
  `internal/curl/document.go`. Not done in Task 6: new file.
- `cmd/talaria/call.go:1` — 520 lines after Task 6, and the largest remaining file in the command
  layer. It is two things: the four view structs plus their renderers (`callView`, `requestView`,
  `responseView`, `responseBody`, `callPayload`, `statusLine`, `validationLine`, `pairMap`) and the
  cobra wiring of `call` itself. Seam: `cmd/talaria/callview.go`. Not done in Task 6: new file.
