package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/spec"
)

// authView is the JSON payload: one entry per security scheme the spec
// declares and talaria can satisfy.
type authView struct {
	Schemes []authScheme `json:"schemes"`
}

// authScheme is DESIGN.md §4's object, field for field. The source is a *name* —
// `env:TALARIA_AUTH_BEARER` — because that is the whole of what an agent needs
// and the whole of what it may learn: it turns the name into "export this and
// retry" without ever holding the value.
type authScheme struct {
	Scheme string `json:"scheme"`
	// Source is omitted for a scheme talaria cannot speak: there is no
	// scheme-specific variable to name, only the bring-your-own-token one the
	// exit-5 error points at.
	Source string `json:"source,omitempty"`
	// Supported is a pointer because it appears precisely when it is false —
	// DESIGN.md:327's object is {"scheme":…,"supported":false,"present":false} —
	// and a plain bool with omitempty says the opposite of that.
	Supported *bool `json:"supported,omitempty"`
	Present   bool  `json:"present"`
	// Withheld reports that a call under these same flags would *not* send this
	// credential, because the destination is outside the allowed host set (§5a).
	// It is omitted when it is false, so the ordinary report is unchanged and an
	// agent reads its presence as the thing to act on.
	Withheld bool `json:"withheld,omitempty"`
}

func newAuthCmd() *cobra.Command {
	// A group, not a command: `auth` holds the subcommands and answers nothing
	// itself, so naming it alone is an incomplete invocation rather than a
	// successful one. groupCommand is what makes that exit 2.
	cmd := groupCommand(&cobra.Command{
		Use:   "auth",
		Short: "Inspect the credentials a spec's security schemes need",
	})

	cmd.AddCommand(newAuthCheckCmd())

	return cmd
}

func newAuthCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [spec]",
		Short: "Report which security schemes have a credential, without reading one",
		Long: "Answer, for each of a spec's security schemes, whether a credential is\n" +
			"present and which environment variable it comes from. Values are never\n" +
			"read, so this is how an agent diagnoses a broken auth setup blind. It\n" +
			"exits 5 when an operation has no credential to authenticate it with.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			doc, index, err := loadSpec(cmd, args)
			if err != nil {
				return err
			}

			cfg, err := config.Load("")
			if err != nil {
				return err
			}

			prof, err := selectProfile(cmd, cfg)
			if err != nil {
				return err
			}

			creds, err := config.Schemes(doc, prof)
			if err != nil {
				return err
			}

			withheld, err := destinationWithholds(cmd, doc, prof)
			if err != nil {
				return err
			}

			if err := output.New(format, cmd.OutOrStdout()).Render(authPayload(creds, withheld)); err != nil {
				return err
			}

			// The report is printed before the verdict, and whatever the verdict
			// is: the exit code says "act on this", and the body says what to act
			// on. Failing first would leave an agent a code 5 and nothing to read.
			return unsatisfied(index.Operations(), creds)
		},
	}
}

// destinationWithholds reports whether a call made under these flags would have
// its credentials withheld: whether the host it would go to is outside the
// allowed set (§5a).
//
// The destination is --base-url, then the profile's, and then the spec's own
// server — which is in the set by construction, so having neither flag nor
// profile base URL is never a withholding.
func destinationWithholds(cmd *cobra.Command, doc *spec.Document, prof *config.Profile) (bool, error) {
	dest, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return false, clierr.Usage("%w", err)
	}
	if dest == "" && prof != nil {
		dest = prof.BaseURL
	}
	if dest == "" {
		return false, nil
	}

	hosts, err := allowedHosts(cmd, doc, prof)
	if err != nil {
		return false, err
	}

	return !hosts.Allows(dest), nil
}

func authPayload(creds []config.Credential, withheld bool) output.Payload {
	view := authView{Schemes: make([]authScheme, 0, len(creds))}
	rows := make([][]string, 0, len(creds))

	for _, cred := range creds {
		present := cred.Present()
		source := cred.Ref.Location()

		var unsupported *bool
		if !cred.Supported {
			unsupported, source = new(bool), ""
		}

		view.Schemes = append(view.Schemes, authScheme{
			Scheme:    cred.Scheme,
			Source:    source,
			Supported: unsupported,
			Present:   present,
			Withheld:  present && withheld,
		})
		rows = append(rows, []string{cred.Scheme, source, presenceLabel(cred, present, withheld)})
	}

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
}

// presenceLabel is the pretty and TSV form of the present field. The words name
// the states a reader acts on: "missing" is the one to go export, "withheld"
// the one to pass --allow-host for, and "unsupported" the one where exporting
// $TALARIA_AUTH_BEARER is the only move talaria has.
func presenceLabel(cred config.Credential, present, withheld bool) string {
	switch {
	case !cred.Supported && !present:
		return "unsupported"
	case !cred.Supported:
		return "present (brought token)"
	case !present:
		return "missing"
	case withheld:
		return "present but withheld"
	default:
		return "present"
	}
}

// unsatisfied returns the error naming what stands between the caller and some
// operation, or nil when every operation in the spec can be authenticated.
//
// There are two ways to be blocked and they carry different codes, because they
// have different fixes: a declared scheme whose variable is unset is exit 5 and
// a human's job, while a scheme the document never declared is exit 2 and no
// variable will help. `call` draws the same line in config.Resolve; drawing it
// differently here is how the pre-flight and the call come to disagree.
func unsatisfied(ops []operation.Operation, creds []config.Credential) error {
	byName := make(map[string]config.Credential, len(creds))
	for _, cred := range creds {
		byName[cred.Scheme] = cred
	}

	// creds is already sorted by scheme name, so walking it to collect the
	// blocking ones keeps the message's order stable too.
	blocking := map[string]bool{}
	var undeclared []string
	seen := map[string]bool{}
	for _, op := range ops {
		if satisfied(op, byName) {
			continue
		}

		for _, req := range op.Security {
			for _, want := range req.Schemes {
				switch cred, ok := byName[want.Name]; {
				case ok && !cred.Present():
					blocking[want.Name] = true
				case !ok && !seen[want.Name]:
					seen[want.Name] = true
					undeclared = append(undeclared, want.Name)
				}
			}
		}
	}

	var parts []string
	for _, cred := range creds {
		if blocking[cred.Scheme] {
			parts = append(parts, cred.Scheme+" (set "+cred.Ref.Symbolic()+")")
		}
	}

	switch {
	case len(parts) == 1:
		return clierr.CredentialMissing("no credential for security scheme %s", parts[0])
	case len(parts) > 1:
		return clierr.CredentialMissing("no credential for security schemes %s", strings.Join(parts, ", "))
	case len(undeclared) > 0:
		return clierr.Usage("spec requires security scheme %s, which components.securitySchemes does not declare",
			strings.Join(undeclared, ", "))
	}

	return nil
}

// satisfied reports whether op has an alternative talaria can authenticate with
// the credentials that are actually set.
//
// A spec may offer alternatives and any one of them is enough, so a bearer
// token alone satisfies an operation that accepts either it or an API key. An
// alternative naming a scheme talaria cannot speak — OAuth2 — counts when a
// brought token covers it and not otherwise (§5 Auth), which is config.Covers'
// answer rather than a judgement made here.
//
// The rule itself is config.Covers, the same one config.Resolve picks an
// alternative with. This verdict is the pre-flight for that call, so deriving
// it here a second time is how the two came to disagree.
func satisfied(op operation.Operation, byName map[string]config.Credential) bool {
	// An operation with no security requires nothing, so there is nothing to be
	// missing. Said outright, because the loop below cannot say it.
	if len(op.Security) == 0 {
		return true
	}

	for _, req := range op.Security {
		switch config.Covers(req, byName) {
		case config.Optional, config.Satisfied:
			return true
		case config.Incomplete, config.Unsupported:
			// Neither is an alternative the caller could make this call with.
		}
	}

	return false
}
