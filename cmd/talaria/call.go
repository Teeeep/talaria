package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

// callView is the payload from DESIGN.md §4's sketch. A dry run fills in
// everything except the response and validation blocks, so an agent parses one
// structure whether or not the request was sent.
type callView struct {
	DryRun  bool        `json:"dry_run"`
	Request requestView `json:"request"`
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

func newCallCmd() *cobra.Command {
	var (
		params         []string
		queries        []string
		headers        []string
		dryRun         bool
		allowMutations bool
	)

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

			if !dryRun {
				// Phase 1 ends at the dry run: there is no execution path yet,
				// and silently printing one would misreport what happened.
				return clierr.Usage(
					"talaria cannot send requests yet: pass --dry-run to print the curl command for %s",
					operationName(op))
			}

			req, err := buildRequest(cmd, op, doc, params, queries, headers)
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).Render(dryRunPayload(req))
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
	op operation.Operation,
	doc *spec.Document,
	params, queries, headers []string,
) (*request.Request, error) {
	prof, err := loadProfile(cmd)
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
	})
}

// loadProfile reads the profile named by --profile, or returns nil when none
// was given. The config file is opened only when it is asked for, so an
// invocation with no profile never reads — or refuses to read — the user's
// configuration at all.
func loadProfile(cmd *cobra.Command) (*config.Profile, error) {
	name, err := cmd.Flags().GetString("profile")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}
	if name == "" {
		return nil, nil
	}

	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}

	return cfg.Profile(name)
}

// dryRunPayload renders the request in both shapes from the same values, so the
// JSON block and the printed command cannot disagree about what would be sent.
func dryRunPayload(req *request.Request) output.Payload {
	view := callView{
		DryRun: true,
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

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
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
