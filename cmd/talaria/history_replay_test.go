package main

// `history replay`'s contract: what a stored entry may contribute to a
// re-issued call and what it may not. The list, show and filter cases are in
// history_test.go, whose helpers this file shares.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/spec"
)

// replayFlags is what every replay now needs. The positional argument names the
// entry, so the spec can only come from --spec; the target host comes from the
// flags rather than the stored URL; and the test server's host is off-spec, so
// --allow-host is what lets a credential reach it.
func replayFlags(srv *callServer) []string {
	return []string{
		"--spec", "testdata/call.yaml", "--base-url", srv.URL,
		"--allow-host", "127.0.0.1", "--output", "json",
	}
}

// runReplay runs `history replay <handle>` with the flags above plus extra.
func runReplay(t *testing.T, srv *callServer, handle string, extra ...string) (int, string, string) {
	t.Helper()

	args := append([]string{"replay", handle}, replayFlags(srv)...)

	return runHistory(t, append(args, extra...)...)
}

func TestHistoryReplayReissuesTheCallAndRecordsANewEntry(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42", "--query", "verbose=true",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("history replay 1 = %d, want 0; stderr: %s", code, stderr)
	}

	// Replay renders the same envelope a call does, so an agent parses one shape.
	replayed := decodeCall(t, stdout)
	if replayed.Response == nil || replayed.Response.Status != http.StatusOK {
		t.Fatalf("replay produced no successful response:\n%s", stdout)
	}

	rec := srv.received()
	if rec.Path != "/pets/42" {
		t.Errorf("server saw path %q on replay, want /pets/42", rec.Path)
	}
	if rec.Query != "verbose=true" {
		t.Errorf("server saw query %q on replay, want verbose=true", rec.Query)
	}
	// The credential is re-resolved from the environment: it was never written to
	// history, so this is the only place it could have come from (§5a).
	if got, want := rec.Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q on replay, want %q", got, want)
	}

	entries := readHistory(t)
	if len(entries) != 2 {
		t.Fatalf("history holds %d entries after a replay, want 2", len(entries))
	}
	if entries[0].Source != corpus.SourceCall {
		t.Errorf("the original entry became %q; replay must leave it intact", entries[0].Source)
	}
	if entries[1].Source != corpus.SourceReplay {
		t.Errorf("the new entry's source = %q, want %q", entries[1].Source, corpus.SourceReplay)
	}
	if entries[1].OperationID != "getPet" {
		t.Errorf("the new entry's operation_id = %q, want getPet", entries[1].OperationID)
	}
}

// A replay is re-derived from the spec, so there is a contract to check the
// response against — and an agent that reads `call`'s validation block must be
// able to read replay's too.
func TestHistoryReplayEmitsAValidationBlock(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}
	if decodeCall(t, stdout).Validation == nil {
		t.Errorf("replay emitted no validation block:\n%s", stdout)
	}
}

// Two replays in a row is ordinary agent behaviour, and it is where the
// positional index breaks: replay appends, so every index printed by the
// listing the agent is reading has already shifted by the time it issues the
// second command. The ids are the same two handles before and after.
func TestReplayingTwoIDsInARowReissuesTwoDifferentEntries(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "getPet", "--param", "petId=42",
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call getPet = %d, want 0; stderr: %s", code, stderr)
	}
	code, _, stderr = runCall(t,
		"testdata/call.yaml", "getPublic", "--base-url", srv.URL, "--output", "json")
	if code != 0 {
		t.Fatalf("call getPublic = %d, want 0; stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHistory(t, "--output", "json")
	if code != 0 {
		t.Fatalf("history = %d, want 0; stderr: %s", code, stderr)
	}
	listed := decodeHistoryList(t, stdout)
	if len(listed.Entries) != 2 {
		t.Fatalf("history listed %d entries, want 2:\n%s", len(listed.Entries), stdout)
	}
	public, pet := listed.Entries[0], listed.Entries[1]
	if public.OperationID != "getPublic" || pet.OperationID != "getPet" {
		t.Fatalf("history listed %q then %q, want getPublic then getPet", public.OperationID, pet.OperationID)
	}

	code, _, stderr = runReplay(t, srv, public.ID)
	if code != 0 {
		t.Fatalf("replay of the getPublic id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/public" {
		t.Fatalf("the first replay sent %q, want /public", got)
	}

	code, _, stderr = runReplay(t, srv, pet.ID)
	if code != 0 {
		t.Fatalf("replay of the getPet id = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Path; got != "/pets/42" {
		t.Errorf("the second replay sent %q, want /pets/42: the handle resolved to the wrong entry", got)
	}

	entries := readHistory(t)
	if len(entries) != 4 {
		t.Fatalf("history holds %d entries after two replays, want 4", len(entries))
	}
	if entries[0].ID != pet.ID || entries[1].ID != public.ID {
		t.Errorf("the original ids became %q and %q; replaying must not rewrite them",
			entries[0].ID, entries[1].ID)
	}
}

func TestHistoryReplayStillRequiresAllowMutations(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	code, _, stderr := runCall(t,
		"testdata/call.yaml", "createPet", "--allow-mutations", "--body", `{"name":"Rex"}`,
		"--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json")
	if code != 0 {
		t.Fatalf("call createPet = %d, want 0; stderr: %s", code, stderr)
	}

	code, _, stderr = runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("history replay of a POST = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-mutations") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if entries := readHistory(t); len(entries) != 1 {
		t.Errorf("a gated replay wrote %d entries, want the original 1", len(entries))
	}

	code, _, stderr = runReplay(t, srv, "1", "--allow-mutations")
	if code != 0 {
		t.Fatalf("history replay --allow-mutations = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Body; got != `{"name":"Rex"}` {
		t.Errorf("server saw body %q on replay, want the recorded one", got)
	}
}

// The mutation gate is decided from the spec, not from the entry. A one-word
// edit to a JSONL line turned a DELETE replay into an ungated one, which is the
// one place here where believing the file costs a write against a live API.
func TestHistoryReplayGatesOnTheSpecsMethodNotTheStoredOne(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// Stored as GET; deleteAllPets is a DELETE in the spec.
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "deleteAllPets", "GET", srv.URL+"/pets", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a spec DELETE stored as GET = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the gated replay still reached the server: %+v", rec)
	}

	// And the inverse: the hostile file must not get to make replay *harder*
	// either, or a stored "DELETE" turns every GET into a flag-gated call.
	isolateHistory(t)
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPublic", "DELETE", srv.URL+"/public", 0))

	code, _, stderr = runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("replay of a spec GET stored as DELETE = %d, want 0; stderr: %s", code, stderr)
	}
	if got := srv.received().Method; got != "GET" {
		t.Errorf("the replay sent %q, want the spec's GET", got)
	}
}

// Finding 2, at the wire. A hand-edited `url` must not decide where the request
// goes: the destination is the spec and the flags, and the planted listener
// must see nothing at all.
func TestHistoryReplayIgnoresAHandEditedStoredURL(t *testing.T) {
	isolateHistory(t)
	real := newCallServer(t, jsonPet)
	planted := newCallServer(t, jsonPet)

	// Both are on 127.0.0.1, so --allow-host admits the planted host too: the
	// point is that even an *allowed* stored URL does not choose the target.
	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", planted.URL+"/pets/42", 0))

	code, _, stderr := runReplay(t, real, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}

	if got := real.received().Path; got != "/pets/42" {
		t.Errorf("the replay sent %q to the host the flags name, want /pets/42", got)
	}
	if rec := planted.received(); rec.Method != "" {
		t.Errorf("the replay reached the planted listener: %+v", rec)
	}
	assertNoCanaryOnTheWire(t, planted.received())
}

// §5a's replay table, row "Target host": a stored host outside the currently
// allowed set is refused rather than silently retargeted. This is the one place
// replay and `call` deliberately differ — `call` withholds and runs.
func TestHistoryReplayRefusesAStoredHostOutsideTheAllowedSet(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runHistory(t, "replay", "1",
		"--spec", "testdata/call.yaml", "--base-url", srv.URL, "--output", "json")
	if code != 2 {
		t.Fatalf("replay of an off-set stored host = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "--allow-host") {
		t.Errorf("error message %q does not name the flag that permits it", msg)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the refused replay still reached the server: %+v", rec)
	}
}

// An entry whose operationId no longer names anything in the spec fails that
// entry, with the exit code an agent branches on — not the process, and not a
// request built from the stored fields.
func TestHistoryReplayFailsWhenTheOperationIsGoneFromTheSpec(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "retiredOperation", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a retired operation = %d, want 2; stderr: %s", code, stderr)
	}
	if msg := decodeErr(t, stderr).Error.Message; !strings.Contains(msg, "retiredOperation") {
		t.Errorf("error message %q does not name the operation it could not find", msg)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// Replay re-derives through the spec, so it needs one. Failing with a usage
// error is a deliberate contract change: the alternative is a replay that
// silently sends the stored fields.
func TestHistoryReplayWithNoSpecIsAUsageError(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

	code, _, stderr := runHistory(t, "replay", "1", "--output", "json")
	if code != 2 {
		t.Fatalf("replay with no spec = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// The entry id must never be mistaken for a spec reference: loadSpec gives a
// positional argument precedence over --spec and the environment, so threading
// the replay's argument through would make `replay 1` load a spec named 1.
func TestHistoryReplayTakesTheSpecFromTheFlagNotTheEntryHandle(t *testing.T) {
	srv := newCallServer(t, jsonPet)

	for _, form := range []string{"flag", "env"} {
		t.Run(form, func(t *testing.T) {
			isolateHistory(t)
			seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0))

			args := []string{"replay", "1", "--base-url", srv.URL, "--allow-host", "127.0.0.1", "--output", "json"}
			if form == "flag" {
				args = append(args, "--spec", "testdata/call.yaml")
			} else {
				t.Setenv(spec.EnvSpec, "testdata/call.yaml")
			}

			code, _, stderr := runHistoryKeepingSpecEnv(t, form == "env", args...)
			if code != 0 {
				t.Fatalf("history replay 1 (%s) = %d, want 0; stderr: %s", form, code, stderr)
			}
			if got := srv.received().Path; got != "/pets/42" {
				t.Errorf("the replay sent %q, want /pets/42", got)
			}
		})
	}
}

// runHistoryKeepingSpecEnv is runHistory with the option of leaving TALARIA_SPEC
// alone, so the environment form of the spec reference can be tested at all.
func runHistoryKeepingSpecEnv(t *testing.T, keep bool, args ...string) (int, string, string) {
	t.Helper()

	if !keep {
		t.Setenv(spec.EnvSpec, "")
	}
	t.Setenv("TALARIA_AUTH_BEARER", callCanary)
	t.Setenv("TALARIA_AUTH_APIKEY_PETKEY", callCanary)

	var out, errOut strings.Builder
	code := run(append([]string{"history"}, args...), &out, &errOut)

	if strings.Contains(out.String(), callCanary) {
		t.Fatalf("history leaked the credential on stdout:\n%s", out.String())
	}

	return code, out.String(), errOut.String()
}

// Finding 22, verified as `{"refresh_token":"<redacted>"}` arriving at the API
// with empty stderr. A stored body still carrying a redaction marker cannot be
// replayed: sending the marker is a login attempt whose password is the literal
// text `<redacted>`.
func TestHistoryReplayRefusesABodyStillCarryingARedactionMarker(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "createPet", "POST", srv.URL+"/pets", 0)
	entry.Request.Body = &corpus.Body{
		ContentType: "application/json",
		Data:        `{"refresh_token":"<redacted>"}`,
	}
	seedHistory(t, entry)

	code, _, stderr := runReplay(t, srv, "1", "--allow-mutations")
	if code != 2 {
		t.Fatalf("replay of a redacted body = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the refused replay still reached the server: %+v", rec)
	}
}

// Every credential position in a stored entry is dropped, whatever it names.
// Nothing in an entry is resolved any more, so an edited `<redacted:env:NAME>`
// is a value that goes nowhere rather than a variable talaria reads.
func TestHistoryReplayDropsAStoredCredentialPosition(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	entry := seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/pets/42", 0)
	entry.Request.Headers = map[string]string{
		"X-Steal":       "<redacted:env:MY_UNRELATED_SECRET>",
		"Authorization": "Bearer a-plausible-looking-literal",
	}
	seedHistory(t, entry)

	t.Setenv("MY_UNRELATED_SECRET", callCanary)

	code, _, stderr := runReplay(t, srv, "1")
	if code != 0 {
		t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
	}

	rec := srv.received()
	if got := rec.Header.Get("X-Steal"); got != "" {
		t.Errorf("the replay sent X-Steal %q; nothing in an entry is resolved", got)
	}
	// The credential comes from config.Resolve against the current environment,
	// never from the entry — so the stored literal is gone and the real one is
	// there in its place.
	if got, want := rec.Header.Get("Authorization"), "Bearer "+callCanary; got != want {
		t.Errorf("server saw Authorization %q, want the re-resolved %q", got, want)
	}
	if !strings.Contains(stderr, "X-Steal") {
		t.Errorf("stderr does not report the dropped header: %s", stderr)
	}
}

// A stored path that cannot be matched against the operation's template is exit
// 2, never a request to a half-substituted path.
func TestHistoryReplayFailsWhenTheStoredPathDoesNotMatchTheTemplate(t *testing.T) {
	isolateHistory(t)
	srv := newCallServer(t, jsonPet)

	seedHistory(t, seedEntry(corpus.SourceCall, time.Minute, "getPet", "GET", srv.URL+"/dogs/42/extra", 0))

	code, _, stderr := runReplay(t, srv, "1")
	if code != 2 {
		t.Fatalf("replay of a mismatched path = %d, want 2; stderr: %s", code, stderr)
	}
	if rec := srv.received(); rec.Method != "" {
		t.Errorf("the failed replay still reached the server: %+v", rec)
	}
}

// The design doc requires replayableEnv be *deleted*, not made unreachable: an
// unreachable version passes every behaviour test and comes back next cycle.
func TestReplayableEnvIsGoneFromTheTree(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing the command package: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the glob matched no Go files, so this test proves nothing")
	}

	checked := 0
	for _, path := range paths {
		// Test files are skipped because this one has to name the symbol to look
		// for it; a test file that referenced the real thing would not compile.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		checked++

		if strings.Contains(string(source), "replayableEnv") {
			t.Errorf("%s still names replayableEnv; nothing in a stored entry is resolved any more", path)
		}
	}

	if checked == 0 {
		t.Fatal("no non-test Go file was read, so this test proves nothing")
	}
}

// A replay has to put the original bytes back on the wire, so a stored body
// beginning with @ or - must not be read as a filename or as the console: the
// bytes are handed to the builder directly, never as an argv-style --body.
func TestHistoryReplaySendsAStoredBodyVerbatim(t *testing.T) {
	for _, body := range []string{`{"name":"Rex"}`, "@/etc/passwd", "-"} {
		t.Run(body, func(t *testing.T) {
			isolateHistory(t)
			srv := newCallServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})

			entry := seedEntry(corpus.SourceCall, time.Minute, "createPet", "POST", srv.URL+"/pets", 0)
			entry.Request.Body = &corpus.Body{ContentType: "application/json", Data: body}
			seedHistory(t, entry)

			code, _, stderr := runReplay(t, srv, "1", "--allow-mutations")
			if code != 0 {
				t.Fatalf("history replay = %d, want 0; stderr: %s", code, stderr)
			}
			if got := srv.received().Body; got != body {
				t.Errorf("server saw body %q, want the recorded %q", got, body)
			}
		})
	}
}

// `history show` prints the entry for a human to read. A base64 body is not
// text, and printing its stored form as if it were the body would be a lie
// about what was sent.
