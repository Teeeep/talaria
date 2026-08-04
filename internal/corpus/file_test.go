package corpus

// This file is the bounds' own tests: how the retention cap, the whole-file read
// bound and the tail rewrite relate to each other. store_test.go is the Store
// API above them — what Append records and what Read gives back.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// paddedLine is one readable entry marshalling to exactly n bytes, so a test can
// build a store of a stated size out of whole entries.
func paddedLine(t *testing.T, url string, n int) []byte {
	t.Helper()

	entry := Entry{
		Source:  SourceCall,
		Method:  "GET",
		URL:     url,
		Request: EntryRequest{Body: &Body{ContentType: "application/json"}},
	}
	base, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshalling the entry: %v", err)
	}
	if n < len(base) {
		t.Fatalf("cannot pad an entry down to %d bytes; the empty one is already %d", n, len(base))
	}

	entry.Request.Body.Data = strings.Repeat("A", n-len(base))
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshalling the padded entry: %v", err)
	}
	if len(line) != n {
		t.Fatalf("padded line is %d bytes, want %d", len(line), n)
	}

	return line
}

// maximalURL is fixed width so every line in maximalStore pads by the same
// amount, and ordered so a test can name the oldest and the newest.
func maximalURL(i int) string {
	return fmt.Sprintf("https://api.example.com/pets/%04d", i)
}

// maximalStore is n entries at exactly maxEntryBytes each: the longest lines a
// reader will hold, which is the size the retention cap by itself permits. 384
// of them is 96 MiB — past the whole-file read bound, and a seventh of what
// maxPerSource alone allows.
func maximalStore(t *testing.T, n int) []byte {
	t.Helper()

	var file bytes.Buffer
	for i := 0; i < n; i++ {
		file.Write(paddedLine(t, maximalURL(i), maxEntryBytes))
		file.WriteByte('\n')
	}

	return file.Bytes()
}

// storeOfExactly is a store of whole entries totalling exactly n bytes,
// newlines included, so the boundary cases can be stated in bytes. The lines are
// maximal apart from the last, which takes the remainder: that keeps the entry
// count well under maxPerSource, so what these tests observe is the byte budget
// and not the cap.
func storeOfExactly(t *testing.T, n int) []byte {
	t.Helper()

	const unit = maxEntryBytes + 1 // one maximal line and its newline

	var file bytes.Buffer
	for i := 0; ; i++ {
		size := unit
		if remaining := n - file.Len(); remaining <= unit {
			size = remaining
		}
		file.Write(paddedLine(t, maximalURL(i), size-1))
		file.WriteByte('\n')
		if file.Len() >= n {
			break
		}
	}
	if file.Len() != n {
		t.Fatalf("built a store of %d bytes, want %d", file.Len(), n)
	}

	return file.Bytes()
}

// The two numbers are a policy and a bound, and they only agree by arithmetic:
// trim leaves at most maxKeptBytes behind and Append writes one line after it,
// so the file the reader is handed is at most maxStoreBytes. Bump either
// constant without the other and talaria writes stores it will not read.
func TestTheReadBoundAdmitsWhatTrimLeavesPlusOneMaximalLine(t *testing.T) {
	if got, want := maxKeptBytes+maxEntryBytes+1, maxStoreBytes; got != want {
		t.Errorf("trim's budget plus one maximal line is %d bytes, want the read bound %d", got, want)
	}
}

func TestAStoreGrownPastTheReadBoundIsStillReadableAfterAnAppend(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, maximalStore(t, 384))
	if _, err := store.Read(); err == nil {
		t.Fatal("the seeded store is already readable; the bound under test was never crossed")
	}

	if err := store.Append(context.Background(), Entry{Source: SourceCall, Method: "GET", URL: "https://api.example.com/pets/new"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read after Append: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("read no entries back")
	}
	if got := entries[len(entries)-1].URL; got != "https://api.example.com/pets/new" {
		t.Errorf("newest entry = %q, want the one just appended", got)
	}
}

func TestTrimRepairsAStoreTheReaderRefuses(t *testing.T) {
	store, path := newStore(t)

	writeStore(t, path, maximalStore(t, 384))
	if _, err := store.Read(); err == nil {
		t.Fatal("the seeded store is already readable; the bound under test was never crossed")
	}

	// Trim is the only thing that shrinks the store. If it needed the whole file
	// the way Read does, the over-bound state would be absorbing and rm would be
	// the only way out of it.
	if err := trim(path, SourceCall); err != nil {
		t.Fatalf("trim: %v", err)
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("Read after trim: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("trim left nothing readable")
	}
	if got, want := entries[len(entries)-1].URL, maximalURL(383); got != want {
		t.Errorf("newest kept entry = %q, want %q", got, want)
	}
	if got := entries[0].URL; got == maximalURL(0) {
		t.Error("trim kept the oldest entry, so it did not drop the head of an over-budget store")
	}
}

func TestTrimKeepsAStoreExactlyAtTheBudgetAndCutsOneByteOver(t *testing.T) {
	t.Run("exactly at the budget", func(t *testing.T) {
		_, path := newStore(t)
		seeded := storeOfExactly(t, maxKeptBytes)
		writeStore(t, path, seeded)

		if err := trim(path, ""); err != nil {
			t.Fatalf("trim: %v", err)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the store back: %v", err)
		}
		if !bytes.Equal(after, seeded) {
			t.Errorf("trim rewrote a store of %d bytes that was exactly at the %d-byte budget, leaving %d", len(seeded), maxKeptBytes, len(after))
		}
	})

	t.Run("one byte over the budget", func(t *testing.T) {
		_, path := newStore(t)
		writeStore(t, path, storeOfExactly(t, maxKeptBytes+1))

		if err := trim(path, ""); err != nil {
			t.Fatalf("trim: %v", err)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the store back: %v", err)
		}
		if len(after) > maxKeptBytes {
			t.Errorf("trim left %d bytes, past its %d-byte budget", len(after), maxKeptBytes)
		}
		// Whole entries only: the window opens mid-line, and keeping that
		// fragment would put a line nothing can parse at the head of the store.
		for i, line := range lines(after) {
			if _, ok := lineHead(line); !ok {
				t.Fatalf("line %d of the trimmed store does not parse", i)
			}
		}
	})
}

// A history file that is a character device, or a symlink to one, is not a store
// talaria wrote. Trimming it must neither block forever nor allocate until the
// process dies — and it must not rewrite the file to whatever it managed to read.
func TestTrimOfAnEndlessStoreTerminates(t *testing.T) {
	_, path := newStore(t)

	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero on this platform")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("/dev/zero", path); err != nil {
		t.Fatalf("symlinking the store: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- trim(path, SourceCall) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("trim accepted an endless store, want a refusal")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("trim did not return within 30s on an endless store")
	}
}

// A FIFO at the history path is worse than an endless store: the bound on bytes
// read never runs, because os.Open on one blocks until a writer arrives. Append
// holds the append lock across the read, so the wait is not this process's alone
// — every other talaria on the same store queues behind it, which is the
// process-level failure §3.1 forbids. Both readers refuse it instead, with the
// entry-level refusal the over-bound case gives, naming the path.
func TestAStoreThatIsAFIFOIsRefusedRatherThanWaitedOn(t *testing.T) {
	readers := map[string]func(string) error{
		"readStore": func(path string) error { _, err := readStore(path); return err },
		"tail":      func(path string) error { _, _, err := tail(path); return err },
	}

	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			_, path := newStore(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("cannot create a FIFO at %s: %v", path, err)
			}

			done := make(chan error, 1)
			go func() { done <- read(path) }()

			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("%s accepted a FIFO as the store, want a refusal", name)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("%s refused a FIFO without naming it: %v", name, err)
				}
			case <-time.After(10 * time.Second):
				// Nothing will ever open this FIFO for writing, so the goroutine
				// stays blocked for the rest of the run: the leak is the defect.
				t.Fatalf("%s blocked on a FIFO; the open must not wait for a writer", name)
			}
		})
	}
}

func TestTrimOfAStoreThatWasNeverWrittenIsANoOp(t *testing.T) {
	_, path := newStore(t)

	if err := trim(path, SourceCall); err != nil {
		t.Fatalf("trim: %v", err)
	}
	assertNoFile(t, path)
}

// shortenCases are the two ways a Data may be cut without corrupting what is
// left: a base64 Data at a four-character quantum, and text at a rune boundary.
// Cutting either at the byte the arithmetic asks for is silent corruption — a
// base64 prefix that no longer decodes, or a split UTF-8 sequence that
// encoding/json rewrites to U+FFFD, so a replay sends bytes the original call
// never sent.
func TestShorteningTextCutsAtARuneBoundary(t *testing.T) {
	// Three bytes per rune, so every offset that is not a multiple of three
	// splits one.
	data := strings.Repeat("☃", 8)

	for n := 0; n <= len(data); n++ {
		got := shorten(data, n, false)
		if len(got) > n {
			t.Fatalf("shorten(%d) kept %d bytes", n, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("shorten(%d) split a rune: %q", n, got)
		}
		if want := n - n%3; len(got) != want {
			t.Errorf("shorten(%d) kept %d bytes, want the %d before the boundary", n, len(got), want)
		}
	}
}

func TestShorteningBase64CutsAtAQuantum(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff, 0xfe, 0xfd}, 32))

	for n := 0; n <= len(encoded); n++ {
		got := shorten(encoded, n, true)
		if len(got) > n {
			t.Fatalf("shorten(%d) kept %d characters", n, len(got))
		}
		if _, err := base64.StdEncoding.DecodeString(got); err != nil {
			t.Fatalf("shorten(%d) left %d characters that do not decode: %v", n, len(got), err)
		}
	}
}

// oversizeEntry is an entry whose encoded line is past maxEntryBytes because of
// its bodies, so encodeLine has something to cut.
func oversizeEntry() Entry {
	big := strings.Repeat("\x01", MaxBody) // six bytes each once encoded

	return Entry{
		Source:  SourceCall,
		Method:  "POST",
		URL:     "https://api.example.com/pets",
		Request: EntryRequest{Body: &Body{ContentType: "application/json", Data: big}},
		Response: &EntryResponse{
			Status: 200,
			Body:   &Body{ContentType: "application/json", Data: big},
		},
	}
}

func TestEncodeLineCutsTheBodiesUntilTheLineFits(t *testing.T) {
	line, err := encodeLine(oversizeEntry())
	if err != nil {
		t.Fatalf("encodeLine: %v", err)
	}
	if len(line) > maxEntryBytes {
		t.Fatalf("the line is %d bytes, past the %d lines() admits", len(line), maxEntryBytes)
	}

	var got Entry
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("the line does not parse: %v", err)
	}
	for name, body := range map[string]*Body{"request": got.Request.Body, "response": got.Response.Body} {
		if body == nil {
			t.Fatalf("%s body is nil", name)
		}
		if !body.Truncated {
			t.Errorf("%s body was cut but is not marked truncated", name)
		}
	}
}

// The entry Append is handed is the caller's, and its Response and its bodies
// are pointers into it. call still holds that entry — recordCall is handed the
// same request the executor sent — so a cut that reached through them would
// shorten what the caller believes it recorded.
func TestEncodeLineLeavesTheCallersEntryAlone(t *testing.T) {
	entry := oversizeEntry()
	requestBody, responseBody := *entry.Request.Body, *entry.Response.Body

	if _, err := encodeLine(entry); err != nil {
		t.Fatalf("encodeLine: %v", err)
	}

	if *entry.Request.Body != requestBody {
		t.Error("encodeLine cut the caller's request body")
	}
	if *entry.Response.Body != responseBody {
		t.Error("encodeLine cut the caller's response body")
	}
}

func TestEncodeLineRefusesWhatItCannotCut(t *testing.T) {
	entry := Entry{
		Source: SourceCall,
		Method: "GET",
		URL:    "https://api.example.com/pets?q=" + strings.Repeat("x", maxEntryBytes),
	}

	line, err := encodeLine(entry)
	if err == nil {
		t.Fatalf("encodeLine returned a %d-byte line, which lines() drops", len(line))
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxEntryBytes)) {
		t.Errorf("the refusal does not name the %d-byte bound: %v", maxEntryBytes, err)
	}
}
