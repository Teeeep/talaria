# Refactor backlog

Observations recorded from inside build iterations. Do not act on these mid-task; a
refactor pass drains them later.

- `internal/request/build.go:1` — 549 lines and growing; it holds the whole binder. Seam:
  the base-URL resolution (`baseURL`, `firstServer`, the candidate list) is a self-contained
  concern with its own security checks and could move to `internal/request/baseurl.go`.
- `internal/request/request_test.go:1` — 1129 lines, one file for every behaviour of the
  package. Splits cleanly along the binder's stages: params/headers, base URL, auth.
