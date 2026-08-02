package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/secret"
	"github.com/Teeeep/talaria/internal/spec"
)

// callView is the payload from DESIGN.md §4's sketch. A dry run fills in
// everything except the response and validation blocks, so an agent parses one
// structure whether or not the request was sent.
type callView struct {
	DryRun   bool          `json:"dry_run"`
	Request  requestView   `json:"request"`
	Response *responseView `json:"response,omitempty"`
}

// requestView is what was, or would have been, sent. Curl is the symbolic
// reproduction; every other field is the redacted display form.
type requestView struct {
	Curl    string            `json:"curl"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Cookies map[string]string `json:"cookies,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// responseView is what came back. Headers keep their repetitions — Set-Cookie
// legitimately appears more than once, and HTTP forbids folding it into one
// comma-joined value the way requestView's map does.
type responseView struct {
	Status   int                 `json:"status"`
	Headers  map[string][]string `json:"headers"`
	Body     responseBody        `json:"body"`
	TimingMS int64               `json:"timing_ms"`
}

// responseBody is the raw bytes of a response, rendered per §4's sketch: a JSON
// body is embedded as JSON so an agent can reach into it with one parse instead
// of two, and anything else becomes a string.
type responseBody []byte

// MarshalJSON embeds the body when it is a JSON object or array, and quotes it
// otherwise.
//
// The object/array test is deliberately narrower than "is valid JSON": a
// plain-text body of `42` or `true` parses as JSON, and embedding it would
// report a document the server never sent. Composite syntax is unambiguous, so
// that is where the line goes.
func (b responseBody) MarshalJSON() ([]byte, error) {
	if len(b) == 0 {
		return []byte("null"), nil
	}

	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		// encoding/json compacts and re-validates whatever a MarshalJSON returns,
		// so the body's own formatting does not leak into the envelope.
		return trimmed, nil
	}

	return json.Marshal(string(b))
}

func newCallCmd() *cobra.Command {
	var (
		params         []string
		queries        []string
		headers        []string
		body           []string
		dryRun         bool
		allowMutations bool
	)

	// Built here rather than per-run so the warning fires once for the whole
	// command tree, which — since main calls run exactly once — is once per
	// process (§5a). Tests get a fresh tree, and so a fresh warner, per case.
	warner := secret.NewQueryKeyWarner()

	cmd := &cobra.Command{
		Use:   "call [spec] <operationId>",
		Short: "Build a request for one operation and show the curl that runs it",
		Long: "Bind parameters, headers and credentials to an operation and render the\n" +
			"request as curl. Credentials are referenced by environment-variable name,\n" +
			"never by value, so the emitted command is safe to paste into a bug report\n" +
			"and runnable wherever the variable is set.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// The operationId is always last: with one argument the spec comes
			// from --spec or the environment, with two it is the first.
			id := args[len(args)-1]

			doc, index, err := loadSpec(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			op, err := index.Lookup(id)
			if err != nil {
				return err
			}

			// Gated before the request is built, and before --dry-run is even
			// consulted: an agent that tried to POST learns the rule in one
			// round trip rather than after fixing an unrelated flag, and
			// without a network call (DESIGN.md §4).
			if op.IsMutation() && !allowMutations {
				return clierr.Usage(
					"%s is a %s request and may change server state: pass --allow-mutations to call it",
					operationName(op), op.Method)
			}

			// Read whether or not --profile was given: the redaction lists are a
			// security setting, and one that only takes effect when you happen to
			// be using a profile is one that silently does not.
			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			req, err := buildRequest(cmd, cfg, op, doc, params, queries, headers, body)
			if err != nil {
				return err
			}

			// Before the dry-run branch: the exposure is a property of the
			// request's shape, and an emitted curl is a command the caller may
			// well run.
			warnQueryCredentials(cmd.ErrOrStderr(), warner, req)

			renderer := output.New(format, cmd.OutOrStdout())
			redactor := secret.NewResponseRedactor(cfg.Redact.Headers, cfg.Redact.BodyPaths)
			if dryRun {
				return renderer.Render(callPayload(req, nil, redactor))
			}

			resp, err := curl.Execute(req)
			if err != nil {
				return err
			}

			return renderer.Render(callPayload(req, resp, redactor))
		},
	}

	// Local to call: these are how one request is parameterised, and no other
	// command binds parameters. --profile and --base-url are persistent on the
	// root, because run and auth check take them too.
	cmd.Flags().StringArrayVar(&params, "param", nil,
		"bind a declared parameter, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&queries, "query", nil,
		"add a query parameter, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&headers, "header", nil,
		"add a request header, name=value (repeatable)")
	cmd.Flags().StringArrayVar(&body, "body", nil,
		"request body: a literal, @file to read one, or - to read this process's stdin")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the curl command and send nothing")
	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"permit a method other than GET, HEAD or OPTIONS")

	return cmd
}

// buildRequest resolves the profile and the operation's credentials, then binds
// the flags to the operation.
func buildRequest(
	cmd *cobra.Command,
	cfg *config.Config,
	op operation.Operation,
	doc *spec.Document,
	params, queries, headers, body []string,
) (*request.Request, error) {
	prof, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	creds, err := config.Resolve(op, doc, prof)
	if err != nil {
		return nil, err
	}

	baseURL, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	return request.Build(request.Inputs{
		Op:      op,
		Doc:     doc,
		Profile: prof,
		Creds:   creds,
		BaseURL: baseURL,
		Params:  params,
		Query:   queries,
		Headers: headers,
		Body:    body,
		// The Go process owns the real stdin, and `--body -` is the only thing
		// that reads it. curl's stdin carries the config document and nothing
		// else (DESIGN.md §5a).
		Stdin: cmd.InOrStdin(),
	})
}

// selectProfile picks the profile named by --profile out of an already-loaded
// config, or returns nil when no name was given.
func selectProfile(cmd *cobra.Command, cfg *config.Config) (*config.Profile, error) {
	name, err := cmd.Flags().GetString("profile")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}
	if name == "" {
		return nil, nil
	}

	return cfg.Profile(name)
}

// warnQueryCredentials fires the one-time query-string warning if this request
// carries a credential in its URL. One warning covers the request: a scheme
// that puts two keys in the query string is a curiosity, and the point is made
// by the first.
func warnQueryCredentials(stderr io.Writer, warner *secret.QueryKeyWarner, req *request.Request) {
	for _, p := range req.Query {
		if p.Value.IsSecret() {
			warner.Warn(stderr, p.Name, p.Value.Ref())
			return
		}
	}
}

// callPayload renders one call in both shapes from the same values, so the JSON
// block and the printed command cannot disagree about what was sent. A nil resp
// is a dry run: the request block is identical either way, which is what makes
// `--dry-run` a faithful preview rather than a separate code path.
//
// The view is built from the request's *redacted* representation throughout —
// requestView holds Value.String() and curl.Render's symbolic form, never a
// resolved credential. The only code that resolves one is internal/curl, at
// exec time, and it hands back a Response rather than a Request (§5a).
func callPayload(req *request.Request, resp *curl.Response, redactor *secret.ResponseRedactor) output.Payload {
	view := callView{
		DryRun: resp == nil,
		Request: requestView{
			Curl:    curl.Render(req),
			Method:  req.Method,
			URL:     curl.URL(req),
			Headers: pairMap(req.Headers),
			Cookies: pairMap(req.Cookies),
		},
	}
	if req.Body != nil {
		view.Request.Body = string(req.Body.Data)
	}

	// One line each: the request being described, then the command that makes
	// it. A table would put the curl in a column and pad it into unreadability.
	rows := [][]string{
		{view.Request.Method + " " + view.Request.URL},
		{view.Request.Curl},
	}

	if resp != nil {
		// The response is redacted on its way into the view and nowhere else:
		// curl.Response keeps what came off the wire, which is what response
		// validation has to check against (§5a).
		view.Response = &responseView{
			Status:   resp.Status,
			Headers:  redactor.Headers(resp.Headers),
			Body:     responseBody(redactor.Body(resp.Body)),
			TimingMS: resp.TimingMS,
		}
		// Status and timing only. The response headers are not summarised here
		// because a Set-Cookie or an X-Auth-Token would land in a human's
		// scrollback unasked; --output json is where the full response lives.
		rows = append(rows, []string{statusLine(resp)})
	}

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
}

// statusLine is the pretty renderer's one-line summary of a response.
func statusLine(resp *curl.Response) string {
	line := strconv.Itoa(resp.Status)
	if text := http.StatusText(resp.Status); text != "" {
		line += " " + text
	}

	return fmt.Sprintf("%s in %dms", line, resp.TimingMS)
}

// pairMap renders headers or cookies as the object §4's sketch shows. Values
// are the redacted display form: this field is read, not run, so it carries
// <redacted:env:NAME> rather than the shell reference. A name that repeats is
// joined the way HTTP itself joins repeated field values, so nothing is lost.
func pairMap(pairs []request.Pair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}

	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if existing, ok := out[p.Name]; ok {
			out[p.Name] = existing + ", " + p.Value.String()
			continue
		}
		out[p.Name] = p.Value.String()
	}

	return out
}

// operationName is the operation's ID, falling back to method and path for a
// spec that sets no operationId.
func operationName(op operation.Operation) string {
	if op.ID != "" {
		return op.ID
	}

	return strings.TrimSpace(op.Method + " " + op.Path)
}
