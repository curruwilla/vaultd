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
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Check connectivity: databases, client binaries, bucket, webhooks",
		Args:  cobra.NoArgs,
		RunE:  planned(g, "doctor", "M6"),
	}

	backup := &cobra.Command{
		Use:   "backup <target>",
		Short: "Back up one target now",
		Args:  cobra.ExactArgs(1),
		RunE:  planned(g, "backup", "M1"),
	}
	backup.Flags().Bool("dry-run", false, "probe and plan without writing anything")
	backup.Flags().String("tier", "", "force the retention tier: hourly, daily, weekly, monthly, yearly")

	list := &cobra.Command{
		Use:   "list [target]",
		Short: "List the backups of a target, newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE:  planned(g, "list", "M1"),
	}
	list.Flags().Bool("json", false, "emit one JSON object per backup")

	verify := &cobra.Command{
		Use:   "verify [backup-id]",
		Short: "Verify a backup: integrity, structure or a real restore",
		Args:  cobra.MaximumNArgs(1),
		RunE:  planned(g, "verify", "M4"),
	}
	verify.Flags().String("target", "", "verify a target's backup instead of naming an id")
	verify.Flags().Bool("latest", false, "with --target, verify its most recent backup")
	verify.Flags().String("level", "", "integrity, structural or restore")
	verify.Flags().Bool("gc", false, "drop leftover verify databases from a crashed run")

	restore := &cobra.Command{
		Use:   "restore <backup-id>",
		Short: "Restore a backup into an explicit target",
		Args:  cobra.ExactArgs(1),
		RunE:  planned(g, "restore", "M4"),
	}
	restore.Flags().String("to", "", "destination DSN (required)")
	restore.Flags().Bool("confirm", false, "acknowledge that this writes to a live database")
	restore.Flags().Bool("force", false, "allow restoring into a non-empty database")

	prune := &cobra.Command{
		Use:   "prune <target>",
		Short: "Apply the retention policy (dry run unless --apply)",
		Args:  cobra.ExactArgs(1),
		RunE:  planned(g, "prune", "M3"),
	}
	prune.Flags().Bool("apply", false, "actually delete; without it prune only reports")
	prune.Flags().Bool("orphans", false, "also remove objects that have no manifest")

	reindex := &cobra.Command{
		Use:   "reindex <target>",
		Short: "Rebuild the index and local cache from the bucket",
		Args:  cobra.ExactArgs(1),
		RunE:  planned(g, "reindex", "M3"),
	}

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

	return []*cobra.Command{doctor, backup, list, verify, restore, prune, reindex, serve, run}
}

func planned(g *globals, name, milestone string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		fmt.Fprintf(g.err, "hint: run `vaultd validate` to check the config in the meantime\n")
		return fmt.Errorf("%s is not implemented yet; it lands in milestone %s (SPEC §18)", name, milestone)
	}
}
