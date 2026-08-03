package main

import (
	"bytes"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/corpus"
	"github.com/Teeeep/talaria/internal/curl"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

func newHistoryReplayCmd() *cobra.Command {
	var allowMutations bool

	cmd := &cobra.Command{
		Use:   "replay <id|n>",
		Short: "Re-issue a recorded call, re-derived from the current spec",
		Long: "Rebuild a recorded call against the current spec and send it again,\n" +
			"recording the result as a new entry. Nothing stored is trusted as an\n" +
			"instruction: the operation's method and path template come from the spec,\n" +
			"the target host from --base-url, the profile or the spec, and credentials\n" +
			"from the environment and profile in force. A spec is therefore required,\n" +
			"from --spec or " + spec.EnvSpec + " — the positional argument names the\n" +
			"entry, never a spec. Name the entry by its id: a replay is itself\n" +
			"recorded, so every index shifts as soon as one runs.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			store, err := openHistory(cmd, cfg)
			if err != nil {
				return err
			}

			entries, err := store.Read()
			if err != nil {
				return err
			}

			_, entry, err := selectEntry(entries, args[0])
			if err != nil {
				return err
			}

			// nil, not args: args[0] is the entry, and spec.Resolve gives a
			// positional spec ref precedence over --spec and the environment, so
			// threading args here would make `replay 3` load a spec named 3.
			doc, index, err := loadSpec(cmd, nil)
			if err != nil {
				return err
			}

			op, err := index.Lookup(entry.OperationID)
			if err != nil {
				return err
			}

			// The same gate `call` applies, decided from the spec's method rather
			// than the stored one: the entry is a line in a file, and believing its
			// method is the one mistake here that sends a real DELETE.
			if op.IsMutation() && !allowMutations {
				return clierr.Usage(
					"entry %s is a %s request and may change server state: pass --allow-mutations to replay it",
					args[0], op.Method)
			}

			req, err := buildReplay(cmd, cfg, entry, args[0], op, doc)
			if err != nil {
				return err
			}
			warnWithheld(cmd.ErrOrStderr(), req)

			renderer := output.New(format, cmd.OutOrStdout())
			redactors := newRedactors(cfg)

			resp, execErr := curl.Execute(cmd.Context(), req)
			recordCall(cmd.ErrOrStderr(), store, corpus.SourceReplay, req, resp, redactors)
			if execErr != nil {
				return execErr
			}

			// The same validation `call` runs: a replay is re-derived from the spec,
			// so there is a contract to check the response against.
			view := redactResponse(resp, redactors.Response)

			return renderer.Render(callPayload(req, view, validateResponse(cmd.ErrOrStderr(), doc, req, view)))
		},
	}

	cmd.Flags().BoolVar(&allowMutations, "allow-mutations", false,
		"permit replaying a method other than GET, HEAD or OPTIONS")

	return cmd
}

// buildReplay rebuilds a runnable request from a stored entry.
//
// Everything that decides where the request goes and what it carries comes from
// the spec and the flags: op supplies the method and the path template, the
// base URL comes from --base-url, the profile or the spec, and the credentials
// from config.Resolve. The entry contributes values and nothing else, which is
// §5a's "a history entry is data, never instruction".
//
// handle is what the caller named the entry by, so a refusal can quote the same
// word they typed.
func buildReplay(
	cmd *cobra.Command,
	cfg *config.Config,
	entry corpus.Entry,
	handle string,
	op operation.Operation,
	doc *spec.Document,
) (*request.Request, error) {
	// The profile is policy here, not connection detail: it is the other half of
	// the set of credentials this replay resolves, and of the hosts they may go to.
	prof, err := selectProfile(cmd, cfg)
	if err != nil {
		return nil, err
	}

	replayable, err := entry.Replay(op)
	if err != nil {
		return nil, err
	}
	for _, dropped := range replayable.Dropped {
		warnUnreplayable(cmd.ErrOrStderr(), dropped)
	}

	hosts, err := allowedHosts(cmd, doc, prof)
	if err != nil {
		return nil, err
	}
	// Refused, where `call` would withhold and run. The two look like one rule
	// and are not (§5a): an off-set --base-url is a human pointing at a twin,
	// while an off-set *stored* host is a line in a file asking to be sent
	// somewhere, and silently retargeting it would replay a request nobody wrote.
	if !hosts.Allows(entry.URL) {
		return nil, clierr.Usage(
			"entry %s was recorded against %s, which is not a host the spec declares; "+
				"pass --allow-host %s to replay it", handle, hosts.Key(entry.URL), hosts.Key(entry.URL))
	}

	creds, err := config.Resolve(op, doc, prof)
	if err != nil {
		return nil, err
	}

	baseURL, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, clierr.Usage("%w", err)
	}

	in := request.Inputs{
		Op:       op,
		Doc:      doc,
		Profile:  prof,
		Creds:    creds,
		BaseURL:  baseURL,
		Hosts:    hosts,
		Params:   replayable.Params,
		Query:    replayable.Query,
		Headers:  replayable.Headers,
		Redactor: newRedactors(cfg).Request,
	}
	if replayable.HasBody {
		// Handed over as this replay's stdin rather than as a --body literal: a
		// stored body beginning with @ or - would otherwise be read as a filename
		// or as the console, which is a file read chosen by a line in a JSONL file.
		in.Body = []string{"-"}
		in.Stdin = bytes.NewReader(replayable.Body)
	}

	return request.Build(in)
}

// warnUnreplayable reports a field the replay had to drop. It is a warning
// rather than a failure: the request is still worth making, and a 401 with an
// explanation on stderr is more useful than a refusal.
func warnUnreplayable(stderr io.Writer, field string) {
	fmt.Fprintf(stderr,
		"warning: the recorded %s held a credential or a redaction marker, which history does not store; replaying without it\n",
		field)
}
