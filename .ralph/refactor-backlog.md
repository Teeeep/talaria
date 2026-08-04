# Refactor backlog

Observations recorded from inside a task, to be drained by a refactor pass. Do not act on
these mid-task.

Task 12 (2026-08-03) drained every entry recorded during tasks 7–11. Resolved:
`newRedactors` is now built once per invocation and threaded (`cmd/talaria/record.go`);
`call.go`'s recording plumbing moved to `record.go`; `internal/corpus/store.go` split into
`store.go` + `file.go`; `internal/curl/firewall_test.go` now exists. What is left below is what
that pass — and task 6's before it — deliberately did not do, and why.

- `cmd/talaria/call.go:414` — `callPayload` renders four blocks (request, withheld, response,
  validation) and `callView` has four optional fields. **Not done:** the fifth block that would
  want a builder does not exist yet, and a builder for four `if`s is more machinery than the
  `if`s. Revisit when something adds a fifth.
- `cmd/talaria/call.go:414` — `callPayload` takes four arguments, three of which every caller
  derives from the same `RunE` locals (`req`, the response view, the validation result, the
  redactor). **Not done for the same reason:** four positional arguments still read, and both
  call sites pass them from adjacent lines. If a fifth appears, it wants a struct — and that is
  the same trigger as the entry above, so they will fall together.
- `cmd/talaria/history_replay.go:127` — `buildReplay` was going to move beside
  `corpus.Entry.Replay`. **Not done:** it needs `selectProfile`, `--base-url`, `--allow-host`
  and `config.Resolve`, so the move would make `internal/corpus` import `internal/config` —
  which the store must not do if it is to stay usable by the twin, which has no profiles. It
  was split into its own file instead; the two rules it holds (an off-set stored host is
  refused, a stored body goes via stdin) are one call each into a package that owns them.
- `internal/curl/config.go:327` — `tempFile` writes the request body to a 0600 file that
  `cleanup` `os.Remove`s **without overwriting** it first. Task 11 made the in-memory buffers
  zero on every path; this is the same defect in a different medium. **Half done:** task 12 took
  the documentation option — `tempFile`'s doc comment now states that the file is unlinked, not
  scrubbed, so no comment claims otherwise. Overwriting before removal is a behaviour change
  with its own failure modes (a copy-on-write filesystem does not overwrite in place, so the
  overwrite would be theatre there) and it wants a task with a test, not a refactor pass.
- `internal/corpus/store_test.go` is 816 lines and no longer splits the way its source does:
  task 12 divided `store.go` into the `Store` API and `file.go`'s file mechanics, and CLAUDE.md
  says tests split the same way with the same names. The seam is the same one — the bounded-read,
  trim, replace and mode cases belong in a `file_test.go`. Not done here because the pass had
  already moved three files, and a mechanical test split stands alone as its own commit.
- `cmd/talaria/history_test.go` is 710 lines against a 470-line `history.go`, the largest test
  file in the command layer. No seam identified — nobody has read it end to end recently. Worth
  a look during the next refactor pass, not a change on suspicion.

- `internal/canary/canary_test.go` is 1300 lines and holds the whole leak suite in one file. The
  seam the file already implies is three: the harness and its surfaces (`TestMain`,
  `readSources`, `harness`, `recordingServer`, `writeConfig`, `assertNoLeak`) → `harness_test.go`;
  the mechanism sweep and the error-path stages → `mechanism_test.go`; and the individual
  named-threat cases (planted `.curlrc`, injected media type, base-url userinfo, body file,
  redaction controls) → `threats_test.go`. Task 13 added ~140 lines to it and did not split,
  because a table-driven suite whose subject is "every surface" is exactly where a mid-task
  split loses coverage silently.

- Nine call sites in `cmd/talaria` hand-build `[][]string` rows for `output.Table`
  (`list.go:103`, `search.go:66`, `history.go:218`/`263`, `uses.go:56`, `describe.go:138`,
  `call.go:457`, `auth.go:157`, `version.go:31`), each deciding its own column order with no
  header and no shared shape. Task 14 could make the *escaping* uniform because it lives in the
  renderer, but nothing stops two commands disagreeing about column count or order for the same
  data, and `list.go`'s `summaryColumn = 3` is a magic index into a slice built 80 lines away.
  The seam is a per-command `rows()` returning a named row type, or `Table` carrying its columns
  as fields rather than positions. Not attempted mid-task: it touches nine files and every
  golden-output test in `cmd/talaria`.

- `internal/curl/exec_test.go` is 789 lines and now holds three subjects, against the
  file-is-one-concern rule the package otherwise follows (`config_test.go` / `firewall_test.go`
  split exactly that way). `TestCheckVersionEnforcesTheFloor` and the seven `TestPreflight*`
  cases task 16 added are `version.go`'s, not the executor's, and they bring their own fixtures
  (`fakeCurl`, `fakeBanner`). The seam is a new `internal/curl/version_test.go` holding those
  eight plus the two helpers; nothing else moves. Task 16 left them where its plan said to put
  them.

- `cmd/talaria/auth_test.go` (580 lines) holds two subjects: what `auth check` *reports* about a
  scheme (source, present, supported, the agreement matrix with `call`) and what the *host set*
  does to that report (four `Withheld` cases). Task 27 added the fifth withholding case and put
  the `call` half in the new `cmd/talaria/hosts_test.go`, which is where the whole host-set half
  belongs — the seam is `Withheld` versus everything else, and after the move `auth_test.go` is
  back under 450 lines.

- `writeRedactConfig` (`cmd/talaria/call_redact_test.go:21`) is now the config-file helper for
  four test files (redaction, auth, history, hosts) and only one of them is about redaction. It
  writes a 0600 `config.yaml` into an isolated `XDG_CONFIG_HOME`; the name is left over from its
  first caller. Rename to `writeConfig` and move it beside the other cross-file helpers when the
  command layer's tests are next reorganised — it is one rename plus four call sites, but it
  collides with the `call_redact_test.go` split already recorded below, so they want one pass.

## Examined and deliberately not consolidated

- The bounded-read idiom in `internal/corpus/file.go` (`readStore`) and `internal/spec/source.go`
  (`fetch`). Task 12's plan named these as one helper waiting to happen. They are not: the
  shared part is four lines of stdlib composition, and what differs is the error contract —
  `spec` wraps in `clierr.SpecLoad` so the bound becomes an exit code, `corpus` needs a
  different message for over-bound than for an I/O failure ("move it aside" is wrong advice for
  a permission denial). A shared helper needs a sentinel error and an `errors.Is` branch at each
  call site, which is the same line count plus a package and an import edge. Recorded in
  CLAUDE.md so the next pass does not re-derive it.

- `internal/request/body.go:118` and `:170` — neither `stdinBody` nor `fileBody` bounds what it
  reads: `--body -` from a 10 GB pipe and `--body @/path/to/anything` both allocate whatever
  arrives, and the bytes then live in memory through binding, the config document and the
  capture. Task 15 made the stdin read *cancellable* but deliberately did not add a size bound:
  the house rule for a bound is that the number is a design decision (the spec and the store
  both have one written down in DESIGN.md, a body does not), and a limit that silently truncates
  a request body would send a request the user did not write. It wants a task with a number, an
  error message, and the hostile test that fails without it — not a constant picked in a
  refactor pass. `fileBody` is also unbounded in *time*: `os.ReadFile` on a FIFO blocks forever
  and no context reaches it, which is precisely the wait the second Ctrl-C now exists for.

- `internal/request/request_test.go` (1506 lines) — it is the whole package's test file plus the
  shared helpers (`fixture`, `inputs`, `specHosts`, `build`, `buildErr`, `find`), while the
  package itself is split five ways and `hosts_test.go`/`body_test.go` already sit beside their
  subjects. CLAUDE.md's "tests split the same way, with the same names" is not true here. The
  seam is `build_test.go` — parameter binding, path templating, method, credentials/withholding —
  leaving the helpers and the Request/Value/render cases in `request_test.go`. Task 25 added
  ~110 lines to it and had nowhere else to put them.

- `cmd/talaria/call_redact_test.go` (469 lines) — two concerns in one file: what `call` redacts
  out of the *response* it received (Set-Cookie, configured body paths) and what it redacts out
  of the *request* it is about to send (the body, on both the `request.body` and `request.curl`
  surfaces). Task 26 added ~150 lines to the second half and `writeRedactConfig` is the only
  shared helper. The seam is `call_body_redact_test.go` for the request-body cases, leaving the
  response cases and the helper where they are.
