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
