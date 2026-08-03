package main

import (
	"fmt"
	"io"

	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
)

// This file is the recording plumbing `call` and `history replay` share: the
// redaction firewall an entry passes through, the write itself, and the one
// translation from an executor response to what the store records. It is not
// the `call` command, which is why it does not live in call.go.

// newRedactors builds the pair of firewalls an entry passes through on its way
// to disk, extended with whatever the config file added. Request and response
// share the header list: a name worth hiding on the way back is worth hiding on
// the way out.
//
// Built once per invocation, in the command's RunE, and threaded from there
// into the binder, the view and the store. Two constructions of the same
// firewall would be two places a per-surface change could apply to only one.
func newRedactors(cfg *config.Config) corpus.Redactors {
	return corpus.Redactors{
		Request:  secret.NewRedactor(cfg.Redact.Headers...),
		Response: secret.NewResponseRedactor(cfg.Redact.Headers, cfg.Redact.BodyPaths),
	}
}

// recordCall writes one entry, reporting a failure to write as a warning and
// nothing more.
//
// A call that reached the server and came back succeeded; whether talaria then
// managed to write the fact down is not a reason to change the exit code an
// agent branches on. The warning still goes to stderr, because history silently
// not recording is how a user discovers weeks later that it never was.
func recordCall(
	stderr io.Writer,
	store *corpus.Store,
	source corpus.Source,
	req *request.Request,
	resp *curl.Response,
	red corpus.Redactors,
) {
	if !store.Recording() {
		return
	}

	if err := store.Append(corpus.NewEntry(source, req, observed(resp), red)); err != nil {
		fmt.Fprintf(stderr, "warning: the call was not recorded in history: %v\n", err)
	}
}

// observed narrows an executor response to the four fields the store records.
//
// This is the one place the two types meet: internal/corpus may not import
// internal/curl, so something has to translate, and having it here keeps every
// caller of recordCall passing the response it already holds. A nil response —
// a dry run, or a call whose connection failed — stays nil, which NewEntry
// records as a request that produced no observation.
func observed(resp *curl.Response) *corpus.Observed {
	if resp == nil {
		return nil
	}

	return &corpus.Observed{
		Status:   resp.Status,
		Headers:  resp.Headers,
		Body:     resp.Body,
		TimingMS: resp.TimingMS,
	}
}
