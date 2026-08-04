# Refactor backlog

Observations recorded from inside build iterations. Do not act on these mid-task; a
refactor pass drains them later.

- `internal/request/build.go:1` — 549 lines and growing; it holds the whole binder. Seam:
  the base-URL resolution (`baseURL`, `firstServer`, the candidate list) is a self-contained
  concern with its own security checks and could move to `internal/request/baseurl.go`.
- `internal/request/request_test.go:1` — 1129 lines, one file for every behaviour of the
  package. Splits cleanly along the binder's stages: params/headers, base URL, auth.
- `cmd/talaria/history.go:1` — 953 lines. The `replay` struct and its eight helpers
  (`request`, `operation`, `storedURL`, `target`, `body`, `query`, `headers`, `replayParams`)
  are a whole transformation living in the command layer against CLAUDE.md's "cmd/talaria is
  wiring" rule. They are cobra-free by construction, so the seam is a package: `internal/replay`
  importing corpus, operation, request and config. The plan for task 2 prescribed history.go, so
  it went there.
- `cmd/talaria/history_test.go:1` — 1266 lines, and `internal/e2e/e2e_test.go` 1028. Both split
  along the same line the source does: listing/filtering vs. replay.
- `cmd/talaria/call_test.go:allowHost` and `cmd/talaria/history_test.go:callHost` and
  `internal/canary/canary_test.go:canaryHost` and `internal/e2e/e2e_test.go:hostOf` — four
  near-copies of "a test server's authority, for --allow-host", in three packages that cannot
  share a helper without a testing-support package. Third occurrence is the rule of three.
- `internal/config/auth.go:1` — 513 lines and it is now three concerns: the Credential/Coverage
  model, the resolution rules (`Resolve`, `Covers`, `Schemes`, `credentialFor`), and the
  env-var/profile naming machinery (`envSuffix`, `envRef`, `profileRef`, `ReferencesEnv`,
  `checkEnvCollisions`). The naming machinery is the clean seam: it has no dependency on
  `operation` or `spec` and would test standalone as `internal/config/envnames.go`.
- `cmd/talaria/call.go:170` — `newRedactors(cfg)` is built twice on one call path: once here and
  again at `:312` inside `buildRequest`. Two objects, one config, and nothing makes them agree
  beyond both calling the same constructor. Task 5 threaded the existing one into `callPayload`
  rather than adding a third; the fix is to build it once in `RunE` and pass it down.
- `internal/canary/canary_test.go:73` — `readSources` does not do what its comment claims. The
  suite stayed `(cached)` after edits to `internal/curl/render.go` and to `internal/spec/servers.go`
  (probed twice on 2026-08-04, only `-count=1` re-ran it), so the gate *can* go stale exactly the
  way the comment says it cannot. The canary package imports none of the code it tests, so nothing
  but the testlog would invalidate it. Either make the dependency real or drop the claim — the
  comment is currently an invariant no test enforces. Relevant to Task 10.
- `internal/curl/render.go:bodyArgs` + `internal/e2e/e2e_test.go:TestAPreviewedFileBodyLosesTheNewlinesTheCallSends`
  — a `--body @file` body renders as `--data @path`, and curl strips newlines out of a file read
  that way, while the executed call sends the file's bytes verbatim. So the previewed command and
  the call send different bytes for a pretty-printed or signed payload. DESIGN.md §3.4 names
  `--data`; widening it to `--data-binary` is a design decision, not a bug fix. The test pins the
  divergence so it is known rather than discovered.
- `internal/curl/config.go:1` — 431 lines after task 4 pushed it past 400. Seam: the `document`
  type and its directive writers (`build`, `auth`, `cookies`, `body`, `directive`, `tempFile`,
  `escapeDirective`) are a distinct concern from the `Options`/`Capture`/`BuildConfig` entry
  points; they would move whole to `internal/curl/document.go`.
