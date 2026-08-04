package corpus

// This file is the file mechanics under the Store: how the history file is
// found, read under a bound, appended to, trimmed and replaced. store.go is the
// API above it — Append, Read, Recording, Path and id assignment — and calls
// into here. The split is the one internal/curl makes between config.go and
// firewall.go: what the thing does, and how the bytes are handled.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"
)

const (
	// fileName is the store, one JSON entry per line, under the state directory.
	fileName = "history.jsonl"
	// maxPerSource is the retention cap, applied per Source rather than over the
	// whole file: a single `run` over a large spec would otherwise evict every
	// interactive call a session had made.
	maxPerSource = 1000

	// maxEntryBytes is the longest line a reader will hold. An entry newBody
	// wrote carries at most two MaxBody-sized bodies, base64 costing a third
	// more, plus a URL and headers, so this is roughly four times the largest
	// line this tool produces — and a line past it is skipped rather than
	// allocated, because its length is a number in a file anyone may edit.
	maxEntryBytes = 256 << 10
	// maxStoreBytes is the whole file the reader will consume. Past it the file
	// has stopped being the store trim maintains and the read is refused, which
	// leaves a file to move aside; loading it and being killed for the memory
	// leaves nothing.
	maxStoreBytes = 64 << 20
	// maxKeptBytes is what trim leaves behind, and it is where the retention
	// policy and the read bound meet: trim runs immediately before Append writes
	// its line, so a store this package maintains is at most maxKeptBytes plus
	// one maximal line — maxStoreBytes exactly. maxPerSource cannot do that job,
	// because it counts entries and the reader holds bytes: 1000 entries per
	// source at maxEntryBytes each is 500 MiB against a 64 MiB bound, which is
	// how talaria came to write stores it would then refuse to read.
	maxKeptBytes = maxStoreBytes - (maxEntryBytes + 1)

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// write appends one line, creating the directory and the file with the modes
// DESIGN.md §5a requires of talaria's highest-risk artifact.
func write(path string, line []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create the history directory: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone and gives a new one
	// dirMode masked by the umask. The Chmod makes 0700 a guarantee rather than
	// an inherited default.
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("cannot secure the history directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("cannot open the history file: %w", err)
	}
	if err := f.Chmod(fileMode); err != nil {
		f.Close() //nolint:errcheck // The Chmod failure is the one worth reporting.
		return fmt.Errorf("cannot secure the history file: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close() //nolint:errcheck // The Write failure is the one worth reporting.
		return fmt.Errorf("cannot write the history entry: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write the history entry: %w", err)
	}

	return nil
}

// trim enforces the retention policy: the newest maxPerSource entries of each
// source, within maxKeptBytes of file. incoming is the source of the entry the
// caller is about to write — Append trims first, so the line it is holding
// counts here. The common append is over neither bound and leaves the file
// alone.
//
// The entry cap is per source and the byte budget is over the whole file,
// because what a reader must hold is the file rather than any one source's share
// of it.
func trim(path string, incoming Source) error {
	data, cut, err := tail(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	all := lines(data)
	sources := make([]Source, len(all))
	readable := make([]bool, len(all))
	counts := map[Source]int{}
	for i, line := range all {
		head, ok := lineHead(line)
		sources[i], readable[i] = head.Source, ok
		if ok {
			counts[head.Source]++
		}
	}
	if incoming != "" {
		// The caller writes its line the moment this returns, so the cap counts
		// it here. Trimming to maxPerSource and then appending would leave the
		// store one entry over its own policy.
		counts[incoming]++
	}

	drop := map[Source]int{}
	for source, n := range counts {
		if n > maxPerSource {
			drop[source] = n - maxPerSource
		}
	}
	if len(drop) == 0 && !cut {
		return nil
	}

	// Oldest first: the file is in append order, so dropping the first n lines
	// of an over-cap source keeps the newest maxPerSource of it, and every other
	// source stays exactly where it was.
	var kept bytes.Buffer
	for i, line := range all {
		if !readable[i] {
			// The rewrite is the one moment a line nothing can parse can be
			// dropped without losing anything Read would have returned.
			continue
		}
		if drop[sources[i]] > 0 {
			drop[sources[i]]--
			continue
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}

	return replace(path, kept.Bytes())
}

// replace swaps the file's contents atomically, so a reader never sees a
// half-written store and a crash mid-trim leaves the old file intact.
func replace(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".history-*")
	if err != nil {
		return fmt.Errorf("cannot stage the trimmed history: %w", err)
	}
	// A no-op once the rename has happened, and the cleanup for every path where
	// it has not.
	defer os.Remove(tmp.Name()) //nolint:errcheck // Best effort; the file is 0600 in the state dir.

	// CreateTemp already opens at 0600; the Chmod makes that a stated guarantee
	// rather than an inherited default.
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close() //nolint:errcheck // The Chmod failure is the one worth reporting.
		return fmt.Errorf("cannot secure the trimmed history: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // The Write failure is the one worth reporting.
		return fmt.Errorf("cannot write the trimmed history: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write the trimmed history: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("cannot replace the history file: %w", err)
	}

	return nil
}

// entryHead is the part of a stored line the store itself consults: the source
// the retention cap counts by, and the id a new one must not collide with.
type entryHead struct {
	ID     string `json:"id"`
	Source Source `json:"source"`
}

// lineHead reads that head off a stored line, reporting whether the line parses
// at all. Trimming and id assignment work from the raw bytes rather than from
// decoded entries so a rewrite reproduces what was written, byte for byte.
func lineHead(line []byte) (entryHead, bool) {
	var head entryHead
	if err := json.Unmarshal(line, &head); err != nil {
		return entryHead{}, false
	}

	return head, true
}

// tail reads the end of the history file: the whole of it when it fits in
// maxKeptBytes, and otherwise the last maxKeptBytes, from the first line
// boundary inside that window. The second result says whether anything was left
// behind, which is what tells trim to rewrite even when no source is over its
// cap.
//
// Reading the tail rather than the whole file is what lets trim repair a store
// Read has already refused. The read bound exists so a file nothing maintains
// cannot exhaust this process, and trim is the only thing that shrinks one: if
// it needed the whole file to run, the over-bound state would be absorbing and
// an operator's only way out would be rm.
func tail(path string) ([]byte, bool, error) {
	f, info, err := openStore(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close() //nolint:errcheck // Read-only; the read error is the one worth reporting.

	// Stat only chooses where to start reading. The bound below is on the bytes
	// actually read, for the same reason readStore's is: the file is appended to
	// by every other talaria on this machine, so its size at the seek is not its
	// size at the read.
	cut := false
	if info.Size() > maxKeptBytes {
		if _, err := f.Seek(info.Size()-maxKeptBytes, io.SeekStart); err != nil {
			return nil, false, fmt.Errorf("cannot read the history file: %w", err)
		}
		cut = true
	}

	data, err := io.ReadAll(io.LimitReader(f, maxKeptBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("cannot read the history file: %w", err)
	}
	if len(data) > maxKeptBytes {
		return nil, false, fmt.Errorf("the history file at %s is not one talaria can trim to %d bytes; move it aside", path, maxKeptBytes)
	}

	if cut {
		// The window opens mid-line. That fragment is not an entry, and keeping
		// it would put a line nothing can parse at the head of the store.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}

	return data, cut, nil
}

// openStore opens the history file for reading and refuses anything that is not
// a regular file. Two reasons it is not just os.Open, and the type check is the
// one the byte bounds cannot make for themselves.
//
// The open is non-blocking because a FIFO's is not: os.Open on one waits for a
// writer that may never arrive, and Append holds the append lock across the read,
// so the wait belongs to every other talaria on the same store as well — a
// process-level failure where §3.1 allows only an entry-level one. A bound on
// bytes read protects nothing when the open never returns.
//
// And the refusal names the path rather than reading on, because this file is
// user-writable by design: a FIFO, a device, or a directory there is not a store
// talaria wrote, and what such a file returns is not history.
func openStore(path string) (*os.File, fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // The Stat failure is the one worth reporting.
		return nil, nil, fmt.Errorf("cannot read the history file at %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		f.Close() //nolint:errcheck // Read-only; the refusal is the one worth reporting.
		return nil, nil, fmt.Errorf("the history file at %s is not a regular file; move it aside", path)
	}

	return f, info, nil
}

// readStore reads the whole history file, refusing one past maxStoreBytes.
//
// The bound is on the bytes actually read, not on what os.Stat reports: the file
// is appended to by every other talaria on this machine, and it is user-writable
// besides. openStore is what keeps the read to a file that can end. Every caller
// passes the refusal up: a store this size has stopped being the file trim
// maintains, and whether to move it aside is a decision only its owner can make.
// Getting there is not a dead end, because trim reads the tail instead and any
// Append repairs it first.
func readStore(path string) ([]byte, error) {
	f, _, err := openStore(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // Read-only; the read error is the one worth reporting.

	data, err := io.ReadAll(io.LimitReader(f, maxStoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the history file: %w", err)
	}
	if len(data) > maxStoreBytes {
		return nil, fmt.Errorf("the history file at %s is larger than the %d bytes talaria will read; move it aside", path, maxStoreBytes)
	}

	return data, nil
}

// lines splits the store into the non-empty lines a reader will hold. A line
// past maxEntryBytes is left out exactly as an unparseable one is: its length is
// a number in a file anyone may edit, and the entries recorded after it must
// still come back (§5a — a hostile entry fails that entry, never the process).
func lines(data []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 || len(line) > maxEntryBytes {
			continue
		}
		out = append(out, line)
	}

	return out
}

// encodeLine is the write side of the bound lines() reads with, and the two are
// one rule: a stored line past maxEntryBytes is one Read, storedIDs, `history`,
// `history show` and `history replay` all skip forever, and trim then deletes it
// with no diagnostic, because trim rebuilds the file from lines(). An Append
// that wrote one and returned nil would report a call recorded that no reader
// can find — the inverse of the hazard the write-last ordering closed, with the
// same consequence: a mutating call re-run.
//
// The bound is on the *encoded* line, never on MaxBody, because the expansion
// happens in the encoding: encoding/json writes a C0 byte as a six-character
// escape, so MaxBody of them is a 393 KB line.
//
// What is cut is what history can most afford to lose. The headers were capped
// on the way in, by capHeaders; here it is the bodies, halved until the line
// fits and marked Truncated to say so. An entry with nothing left to cut — a
// URL of three 100 KB query values, say — is refused rather than written, so
// recordCall's stderr warning fires. Silent loss is the one outcome that must
// not survive.
func encodeLine(e Entry) ([]byte, error) {
	for {
		line, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("cannot encode the history entry: %w", err)
		}
		if len(line) <= maxEntryBytes {
			return line, nil
		}
		if !halveBodies(&e) {
			return nil, fmt.Errorf("the entry encodes to %d bytes, past the %d a stored line may hold, and nothing left in it can be cut", len(line), maxEntryBytes)
		}
	}
}

// halveBodies cuts each body in half, reporting whether there was anything left
// to cut. Halving rather than computing an offset from the overage, because the
// cost of a byte in the encoding runs from one to six and only the encoder
// knows which; the loop above re-measures, and a body reaches empty in at most
// log2(MaxBody) passes.
func halveBodies(e *Entry) bool {
	cut := false
	if body := halfOf(e.Request.Body); body != nil {
		e.Request.Body = body
		cut = true
	}
	if e.Response != nil {
		if body := halfOf(e.Response.Body); body != nil {
			// A copy, because the caller still holds the entry it passed and the
			// Response behind it is a pointer.
			response := *e.Response
			response.Body = body
			e.Response = &response
			cut = true
		}
	}

	return cut
}

// halfOf is b with half its data, or nil when there is none left to drop.
func halfOf(b *Body) *Body {
	if b == nil || b.Data == "" {
		return nil
	}

	cut := *b
	cut.Data = shorten(b.Data, len(b.Data)/2, b.Encoding == EncodingBase64)
	cut.Truncated = true

	return &cut
}

// shorten cuts data to at most n bytes without corrupting what is left. A
// base64 Data is cut at a four-character quantum, since a prefix at any other
// offset does not decode; text is cut back to a rune boundary, since
// encoding/json rewrites a split sequence to U+FFFD and a replay would then
// send bytes the original call never sent.
func shorten(data string, n int, base64 bool) string {
	if base64 {
		return data[:n-n%4]
	}
	for n > 0 {
		if r, size := utf8.DecodeLastRuneInString(data[:n]); r != utf8.RuneError || size > 1 {
			break
		}
		n--
	}

	return data[:n]
}

// stateDir is $XDG_STATE_HOME/talaria, falling back to ~/.local/state/talaria.
// The lookup is spelled out because Go's os package has no UserStateDir; the
// fallback is the one the XDG spec itself names. A relative XDG_STATE_HOME is
// ignored, as the spec requires.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "talaria"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".local", "state", "talaria"), nil
}
