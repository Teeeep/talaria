package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/request"
	"github.com/Teeeep/talaria/internal/spec"
)

// allowedHosts is the set of hosts this invocation may send a resolved
// credential to: the spec's servers[] after variable substitution, plus every
// --allow-host, plus the active profile's allow_hosts (DESIGN.md §5a).
//
// Every command that resolves a credential builds it the same way, so `call`,
// `auth check` and `history replay` cannot disagree about where one may go.
func allowedHosts(cmd *cobra.Command, doc *spec.Document, prof *config.Profile) (request.HostSet, error) {
	flags, err := cmd.Flags().GetStringArray("allow-host")
	if err != nil {
		return request.HostSet{}, clierr.Usage("%w", err)
	}

	var profileHosts []string
	if prof != nil {
		profileHosts = prof.AllowHosts
	}

	return request.NewHostSet(request.ServerURLs(doc), flags, profileHosts)
}

// warnWithheld reports on stderr the credentials the host-binding rule kept off
// a request: one line for the whole request, naming the schemes, the host and
// the flag that overrides it.
//
// The machine-readable half is credentials_withheld in the envelope. This is
// the half a human reads, and it is one line rather than one per scheme because
// a warning repeated per credential is a warning an agent learns to skip.
func warnWithheld(stderr io.Writer, req *request.Request) {
	if len(req.Withheld) == 0 {
		return
	}

	schemes := make([]string, 0, len(req.Withheld))
	for _, w := range req.Withheld {
		schemes = append(schemes, w.Scheme)
	}
	host := req.Withheld[0].Host

	fmt.Fprintf(stderr,
		"warning: credentials withheld from %s (%s): the spec does not declare that host; "+
			"pass --allow-host %s to send them\n",
		host, strings.Join(schemes, ", "), host)
}
