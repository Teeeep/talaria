// Package corpus is talaria's record of what it actually sent and what came
// back. It backs `talaria history` now and the twin's recordings later: DESIGN.md
// §5 puts it in Phase 2 even though the recording proxy is Phase 5, because
// "history and the twin's corpus are the same data".
//
// Everything here is written redacted. §5a calls the recording "a permanent
// artifact — the highest-risk surface in the tool" and answers it with
// "redaction at write time, not read time. Un-redacted recording is not an
// option." An Entry is therefore built by NewEntry out of the redacted
// representation of a request and a response, and there is no path through this
// package that puts a resolved credential on disk.
//
// The package deliberately does not import internal/config itself: the
// profile's history setting arrives as the Enabled bool a caller passes to New,
// so nothing here reads a profile. It does still reach config through
// internal/request, which needs config.Credential to build an authenticated
// request; breaking that edge is a package split rather than an import edit and
// is deferred to phase 2b. Only the direct import is enforced, by the
// forbiddenDirect entry in internal/e2e/boundary_test.go.
package corpus

import (
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// MaxBody is how much of a request or response body one entry keeps. A body
// past it is cut and marked truncated: history is for answering "what have I
// already tried", and a 40 MB download answers that no better than its first
// 64 KiB while being the difference between a store you can read and one you
// cannot.
const MaxBody = 64 << 10

// Source says which command produced an entry. It exists because the retention
// cap is applied per source: a `run` over a large spec must not be able to evict
// a session of interactive `call` history.
type Source string

const (
	// SourceCall is one interactive `talaria call`.
	SourceCall Source = "call"
	// SourceReplay is a `talaria history replay` of an earlier entry.
	SourceReplay Source = "replay"
)

// Entry is one recorded request/response pair, already redacted.
//
// A Response of nil is a request that produced no observation — a dry run, or a
// call whose connection failed — which is still worth recording as something
// that was tried.
type Entry struct {
	// ID is the entry's stable handle, assigned by Store.Append. A positional
	// index is recomputed on every read, and `call`, `run` and `replay` all
	// append, so the index printed by one command has already shifted by the time
	// the next one runs; the id is what lets `history show` and `history replay`
	// name the same entry twice in a row.
	//
	// An entry written by an older talaria has none. It stays empty rather than
	// being invented at read time, because an id derived on the way out would
	// differ between two reads and could collide with an assigned one.
	ID          string    `json:"id,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	Source      Source    `json:"source"`
	OperationID string    `json:"operation_id,omitempty"`
	Method      string    `json:"method"`
	// URL is the full request URL with credentials in the query string rendered
	// as <redacted:env:NAME>.
	URL      string         `json:"url"`
	Request  EntryRequest   `json:"request"`
	Response *EntryResponse `json:"response,omitempty"`
}

// EntryRequest is the redacted request. Headers and cookies are maps rather
// than ordered pairs because history is read by name; a name that repeats is
// joined the way HTTP joins repeated field values.
type EntryRequest struct {
	Headers map[string]string `json:"headers,omitempty"`
	Cookies map[string]string `json:"cookies,omitempty"`
	Body    *Body             `json:"body,omitempty"`
}

// EntryResponse is the redacted response. Headers keep their repetitions, since
// Set-Cookie legitimately appears more than once.
type EntryResponse struct {
	Status   int                 `json:"status"`
	Headers  map[string][]string `json:"headers,omitempty"`
	Body     *Body               `json:"body,omitempty"`
	TimingMS int64               `json:"timing_ms"`
}

// EncodingBase64 marks a Data that holds standard base64 rather than the body's
// own bytes. It is the only encoding this build writes or reads.
const EncodingBase64 = "base64"

// Body is a recorded body and how much of it was kept.
type Body struct {
	ContentType string `json:"content_type,omitempty"`
	Data        string `json:"data"`
	// Encoding says how to read Data. Empty means Data is the body's bytes as
	// text, which is the case for everything that is valid UTF-8; a body that is
	// not gets EncodingBase64, because a JSON string cannot carry an invalid
	// byte — encoding/json rewrites each one to U+FFFD, and a reader replaying
	// that would send bytes the original call never sent.
	Encoding string `json:"encoding,omitempty"`
	// Truncated reports that Data is the first MaxBody bytes and not the whole
	// body. It is explicit so a reader never mistakes a cut body for what the
	// server sent.
	Truncated bool `json:"truncated,omitempty"`
}

// Bytes returns the recorded body as the bytes it was on the wire.
//
// An encoding this build does not know is refused rather than guessed at: the
// store is a file anything can write, and a newer talaria may have added one.
// Reading such a Data as literal text is the exact silent corruption the
// Encoding field exists to prevent.
//
// MaxBody bounds the result. newBody applies it on the way in by truncating, so
// no entry talaria wrote can exceed it — but the store is user-writable, and a
// base64 Data is an amplifier: a few hundred bytes of line can name hundreds of
// megabytes of decode. The encoded length is checked first, so a body that
// could not possibly fit is refused without allocating; the decoded length is
// checked after, because base64 rounds to three-byte groups and cannot tell
// MaxBody from MaxBody+1 on its own.
func (b Body) Bytes() ([]byte, error) {
	switch b.Encoding {
	case "":
		if len(b.Data) > MaxBody {
			return nil, tooLarge(len(b.Data))
		}

		return []byte(b.Data), nil
	case EncodingBase64:
		if len(b.Data) > base64.StdEncoding.EncodedLen(MaxBody) {
			return nil, tooLarge(base64.StdEncoding.DecodedLen(len(b.Data)))
		}
		data, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			return nil, clierr.Usage("the recorded body claims %s encoding but does not decode: %v", EncodingBase64, err)
		}
		if len(data) > MaxBody {
			return nil, tooLarge(len(data))
		}

		return data, nil
	default:
		return nil, clierr.Usage("the recorded body has encoding %q, which this version of talaria cannot read", b.Encoding)
	}
}

// tooLarge is the refusal a recorded body past MaxBody gets. It names the size
// asked for, since the entry itself is the only place that number came from.
func tooLarge(size int) error {
	return clierr.Usage("the recorded body is %d bytes, past the %d this version of talaria will read back", size, MaxBody)
}

// Observed is what came back, in the only shape this package needs to record
// it: a status, the response headers, the body's bytes and how long the call
// took.
//
// It exists so the store does not name the executor's response type. `curl` is
// how talaria makes a call today and the twin will make its own with net/http,
// so a corpus that imported internal/curl would tie the recording of a call to
// one way of placing it — the edge internal/e2e/boundary_test.go forbids.
type Observed struct {
	Status   int
	Headers  map[string][]string
	Body     []byte
	TimingMS int64
}

// Redactors are the two firewalls an entry passes through on its way to disk.
// Both are nil-safe: the zero Redactors applies the built-in lists, so a caller
// that forgets to configure them records less rather than more.
type Redactors struct {
	// Request matches request header names against the built-in credential list
	// plus whatever the profile added.
	Request *secret.Redactor
	// Response redacts response headers by name and both bodies by JSON path.
	Response *secret.ResponseRedactor
}

// NewEntry builds the record of one request and the response it produced.
//
// Redaction happens here rather than at read time, and it happens before
// truncation: cutting a JSON body first would leave a prefix the path redactor
// can no longer parse, and a secret in that prefix would survive.
func NewEntry(source Source, req *request.Request, obs *Observed, red Redactors) Entry {
	entry := Entry{
		Timestamp: time.Now().UTC(),
		Source:    source,
	}

	if req != nil {
		entry.OperationID = req.OperationID
		entry.Method = req.Method
		// request.Redacted cannot fail, and a URL that somehow did not render is
		// not worth dropping the whole recording over.
		entry.URL, _ = req.URL(request.Redacted)
		entry.Request = EntryRequest{
			Headers: red.headers(req.Headers),
			Cookies: red.cookies(req.Cookies),
			Body:    red.requestBody(req.Body),
		}
	}

	if obs != nil {
		entry.Response = &EntryResponse{
			Status:   obs.Status,
			Headers:  red.Response.Headers(obs.Headers),
			Body:     newBody(contentType(obs.Headers), red.Response.Body(obs.Body)),
			TimingMS: obs.TimingMS,
		}
	}

	return entry
}

// headers renders the request headers by whichever route redacts them.
//
// A Value holding a SecretRef prints as <redacted:env:NAME> by construction and
// keeps that form, which tells a reader which variable to export. Everything
// else goes through the name-based matcher, which is what catches a credential
// the user typed into --header as a literal — nothing structural would.
func (r Redactors) headers(pairs []request.Pair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}

	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		value := p.Value.String()
		if !p.Value.IsSecret() {
			value = r.Request.Value(p.Name, value)
		}
		if existing, ok := out[p.Name]; ok {
			value = existing + ", " + value
		}
		out[p.Name] = value
	}

	return out
}

// cookies records cookie names and no literal cookie values.
//
// On the wire these are one Cookie header, which the built-in list redacts
// whole; matching a per-cookie name like "session" against that list would
// catch nothing, so keeping literals here would make the same credential safe
// as a header and exposed as a cookie. A cookie that names a SecretRef keeps
// its <redacted:env:NAME> form, which says more than the placeholder does.
func (r Redactors) cookies(pairs []request.Pair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}

	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p.Value.IsSecret() {
			out[p.Name] = p.Value.String()
			continue
		}
		out[p.Name] = secret.Placeholder
	}

	return out
}

// requestBody redacts and cuts the body that was sent. The response redactor's
// JSON paths are applied to it too: the built-in list names access_token and
// refresh_token, and a token-refresh request carries one in exactly that field.
func (r Redactors) requestBody(b *request.Body) *Body {
	if b == nil {
		return nil
	}

	return newBody(b.ContentType, r.Response.Body(b.Data))
}

// newBody keeps at most MaxBody bytes of data, reporting whether it had to cut.
//
// A JSONL line has to stay parseable — one unparseable line is one a reader has
// to skip — and encoding/json buys that by rewriting every byte a Go string
// cannot hold to U+FFFD. That is fine for text and destroys anything else, so
// the bytes are checked first: valid UTF-8 is stored as itself and stays
// readable in the store, and anything else is base64'd so the entry survives the
// round trip byte for byte. The cut is at a byte offset and may land inside a
// UTF-8 sequence, which lands such a body in the base64 case too.
func newBody(contentType string, data []byte) *Body {
	if len(data) == 0 {
		return nil
	}

	body := &Body{ContentType: contentType}
	if len(data) > MaxBody {
		data = data[:MaxBody]
		body.Truncated = true
	}
	if utf8.Valid(data) {
		body.Data = string(data)

		return body
	}
	body.Data = base64.StdEncoding.EncodeToString(data)
	body.Encoding = EncodingBase64

	return body
}

// contentType reads the media type off a response, for a reader deciding how to
// interpret the recorded bytes.
func contentType(headers map[string][]string) string {
	for _, value := range headers["Content-Type"] {
		return value
	}

	return ""
}
