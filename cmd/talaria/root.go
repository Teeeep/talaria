package main

import (
	"github.com/spf13/cobra"
)

// version is the binary's version string. It defaults to "dev" and is
// overridden at build time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

// newRootCmd builds the talaria command tree. Every command is constructed
// per-call rather than living in a package-level var so tests can run against
// an isolated tree with their own output writers.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "talaria",
		Short: "Point it at any API doc; your agent works the API and never sees your credentials",
		Long: "talaria turns any OpenAPI/Swagger spec into an explorable API client, a\n" +
			"spec-driven tester, and a digital twin — without exposing credentials to\n" +
			"the agent driving it.",
		Version: version,
		// Agents parse stderr. Cobra's default behaviour of dumping the usage
		// block on every error, and of printing the error itself on top of the
		// caller doing so, is noise.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Registered once, here, so every present and future subcommand accepts it
	// (DESIGN.md §3.1: "--output json on every command"). The empty default
	// means "decide from the terminal"; see output.Resolve.
	root.PersistentFlags().String("output", "",
		"output format: json|pretty|tsv (default: pretty on a terminal, json when piped)")

	root.AddCommand(newVersionCmd())

	return root
}
