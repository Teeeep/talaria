package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

// authView is the JSON payload: one entry per security scheme the spec
// declares, whether or not talaria can satisfy it.
type authView struct {
	Schemes []authScheme `json:"schemes"`
}

// authScheme is DESIGN.md §4's object plus the withheld field §5a adds. The
// source is a *name* — `env:TALARIA_AUTH_BEARER` — because that is the whole of
// what an agent needs and the whole of what it may learn: it turns the name into
// "export this and retry" without ever holding the value.
type authScheme struct {
	Scheme string `json:"scheme"`
	Source string `json:"source"`
	// Supported reports whether talaria resolves this scheme's type. False is a
	// scheme it cannot run the flow for — oauth2, openIdConnect, mutualTLS, an
	// apiKey somewhere a request has no room for — where the source names the
	// variable to put a token the caller obtained elsewhere in (§5 Auth).
	Supported bool `json:"supported"`
	Present   bool `json:"present"`
	// Withheld reports that the credential is exported but would not be sent to
	// the host this invocation resolves to. Without it "present" would mean
	// something `call` disagrees with, which is the §5 clause — "auth check
	// never reports a scheme satisfied when the call would refuse it" — read
	// through the host-binding door.
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

			inv, err := newInvocation(cmd)
			if err != nil {
				return err
			}

			creds, err := config.Schemes(doc, inv.Profile)
			if err != nil {
				return err
			}

			withheld, err := credentialsWithheld(inv, doc)
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

// credentialsWithheld reports whether the host this invocation resolves to is
// one the spec's credentials are bound to.
//
// It reads the same invocation and calls the same two functions `call` does, so
// the answer cannot drift from what a call would actually do. A spec that
// declares no server and an invocation with no --base-url resolve to no host at
// all: there is nothing to withhold from, so the report is the plain one.
func credentialsWithheld(inv *invocation, doc *spec.Document) (bool, error) {
	allowed, err := request.AllowedHosts(doc, inv.Profile, inv.AllowHosts)
	if err != nil {
		return false, err
	}

	target, err := request.Target(inv.BaseURL, inv.Profile, doc)
	if err != nil {
		return false, err
	}

	return target != "" && !allowed.Allows(target), nil
}

func authPayload(creds []config.Credential, withheld bool) output.Payload {
	view := authView{Schemes: make([]authScheme, 0, len(creds))}
	rows := make([][]string, 0, len(creds))

	for _, cred := range creds {
		// A credential that is not set is not withheld: there is nothing to
		// withhold, and reporting both would send a reader to --allow-host when
		// what they need is to export the variable.
		present := cred.Present()
		hidden := present && withheld

		view.Schemes = append(view.Schemes, authScheme{
			Scheme:    cred.Scheme,
			Source:    cred.Ref.Location(),
			Supported: cred.Supported,
			Present:   present,
			Withheld:  hidden,
		})
		rows = append(rows, []string{
			cred.Scheme, cred.Ref.Location(), presenceLabel(cred.Supported, present, hidden),
		})
	}

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
}

// presenceLabel is the pretty and TSV form of the supported, present and
// withheld fields. The words name the four states a reader acts on: "missing"
// is the one to go export the named variable for, "withheld" is the one to go
// pass --allow-host for, and "unsupported" is the one where the variable is a
// token to obtain out of band first, because talaria cannot run the flow.
func presenceLabel(supported, present, withheld bool) string {
	switch {
	case withheld:
		return "withheld"
	case present:
		return "present"
	case !supported:
		return "unsupported"
	}

	return "missing"
}

// unsatisfied returns the exit-5 error naming the credentials that stand
// between the caller and some operation, or nil when every operation in the
// spec can be authenticated.
func unsatisfied(ops []operation.Operation, creds []config.Credential) error {
	byName := make(map[string]config.Credential, len(creds))
	for _, cred := range creds {
		byName[cred.Scheme] = cred
	}

	// creds is already sorted by scheme name, so walking it to collect the
	// blocking ones keeps the message's order stable too.
	blocking := map[string]bool{}
	undeclared := map[string]bool{}
	for _, op := range ops {
		if satisfied(op, byName) {
			continue
		}

		for _, req := range op.Security {
			for _, want := range req.Schemes {
				switch cred, ok := byName[want.Name]; {
				case !ok:
					undeclared[want.Name] = true
				case !cred.Present():
					blocking[want.Name] = true
				}
			}
		}
	}
	if len(blocking) == 0 {
		// An operation nothing blocks but that is still unsatisfiable names a
		// scheme components.securitySchemes does not declare. No variable would
		// fix it, so it is a usage error, and it is the same verdict `call`
		// reaches through config.Resolve.
		return undeclaredSchemes(undeclared)
	}

	var parts []string
	for _, cred := range creds {
		if blocking[cred.Scheme] {
			parts = append(parts, cred.Scheme+" (set "+cred.Ref.Symbolic()+")")
		}
	}

	if len(parts) == 1 {
		return clierr.CredentialMissing("no credential for security scheme %s", parts[0])
	}

	return clierr.CredentialMissing("no credential for security schemes %s", strings.Join(parts, ", "))
}

// undeclaredSchemes is the usage error for requirements naming schemes the
// document never declares, or nil when there are none.
func undeclaredSchemes(names map[string]bool) error {
	if len(names) == 0 {
		return nil
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	return clierr.Usage(
		"the spec requires security scheme(s) %s, which components.securitySchemes does not declare",
		strings.Join(sorted, ", "))
}

// satisfied reports whether op has an alternative talaria can authenticate with
// the credentials that are actually set.
//
// A spec may offer alternatives and any one of them is enough, so a bearer
// token alone satisfies an operation that accepts either it or an API key. An
// alternative talaria cannot put on the wire — OAuth2 with no token exported,
// or a scheme the document never declares — is counted against the caller
// rather than skipped: it used to be skipped, and that is how `auth check`
// exited 0 on a spec `call` refuses (§5 Auth).
//
// The rule itself is config.Covers, the same one config.Resolve picks an
// alternative with. This verdict is the pre-flight for that call, so deriving
// it here a second time is how the two came to disagree.
func satisfied(op operation.Operation, byName map[string]config.Credential) bool {
	usable, blocked := false, false

	for _, req := range op.Security {
		switch config.Covers(req, byName) {
		case config.Optional, config.Satisfied:
			return true
		case config.Incomplete:
			usable = true
		case config.Unsupported:
			blocked = true
		}
	}

	// Neither usable nor blocked means no alternative applied at all, which is
	// the common case of an operation with no security.
	return !usable && !blocked
}
