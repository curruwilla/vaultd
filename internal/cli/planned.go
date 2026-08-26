package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newPlannedCommands registers the rest of the CLI surface (SPEC §10) ahead of
// its implementation. The flags and argument rules are real, so `--help`
// documents the shape a script will eventually get; running one fails loudly
// with the milestone that brings it, instead of silently doing nothing.
func newPlannedCommands(g *globals) []*cobra.Command {
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: scheduler, locks, metrics and UI",
		Args:  cobra.NoArgs,
		RunE:  planned(g, "serve", "M7"),
	}

	run := &cobra.Command{
		Use:   "run",
		Short: "Run every target that is due, once, then exit",
		Args:  cobra.NoArgs,
		RunE:  planned(g, "run", "M7"),
	}

	return []*cobra.Command{serve, run}
}

func planned(g *globals, name, milestone string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		fmt.Fprintf(g.err, "hint: run `vaultd validate` to check the config in the meantime\n")
		return fmt.Errorf("%s is not implemented yet; it lands in milestone %s (SPEC §18)", name, milestone)
	}
}
