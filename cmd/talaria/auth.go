package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
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
	Scheme  string `json:"scheme"`
	Source  string `json:"source"`
	Present bool   `json:"present"`
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Inspect the credentials a spec's security schemes need",
	}

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

			if err := output.New(format, cmd.OutOrStdout()).Render(authPayload(creds)); err != nil {
				return err
			}

			// The report is printed before the verdict, and whatever the verdict
			// is: the exit code says "act on this", and the body says what to act
			// on. Failing first would leave an agent a code 5 and nothing to read.
			return unsatisfied(index.Operations(), creds)
		},
	}
}

func authPayload(creds []config.Credential) output.Payload {
	view := authView{Schemes: make([]authScheme, 0, len(creds))}
	rows := make([][]string, 0, len(creds))

	for _, cred := range creds {
		present := cred.Present()
		view.Schemes = append(view.Schemes, authScheme{
			Scheme:  cred.Scheme,
			Source:  cred.Ref.Location(),
			Present: present,
		})
		rows = append(rows, []string{cred.Scheme, cred.Ref.Location(), presenceLabel(present)})
	}

	return output.Payload{Data: view, Table: output.Table{Rows: rows}}
}

// presenceLabel is the pretty and TSV form of the present field. The words name
// the two states a reader acts on, and "missing" is the one to go export.
func presenceLabel(present bool) string {
	if present {
		return "present"
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
	for _, op := range ops {
		if satisfied(op, byName) {
			continue
		}

		for _, req := range op.Security {
			for _, want := range req.Schemes {
				if cred, ok := byName[want.Name]; ok && !cred.Present() {
					blocking[want.Name] = true
				}
			}
		}
	}
	if len(blocking) == 0 {
		return nil
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

// satisfied reports whether op has an alternative talaria can authenticate with
// the credentials that are actually set.
//
// A spec may offer alternatives and any one of them is enough, so a bearer
// token alone satisfies an operation that accepts either it or an API key. An
// alternative naming a scheme talaria cannot supply — OAuth2 — is skipped
// rather than counted against the caller: v1's position is that you bring your
// own token for those, and there is no variable to report missing (§5 Auth).
func satisfied(op operation.Operation, byName map[string]config.Credential) bool {
	usable := false

	for _, req := range op.Security {
		// An empty requirement is the spec saying authentication is optional here.
		if len(req.Schemes) == 0 {
			return true
		}

		known, present := true, true
		for _, want := range req.Schemes {
			cred, ok := byName[want.Name]
			switch {
			case !ok:
				known = false
			case !cred.Present():
				present = false
			}
		}

		if !known {
			continue
		}
		usable = true

		if present {
			return true
		}
	}

	// No usable alternative means nothing talaria could have satisfied, which
	// includes the common case of an operation with no security at all.
	return !usable
}
