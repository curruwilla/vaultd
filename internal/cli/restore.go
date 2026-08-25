package cli

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/restore"
)

func newRestoreCommand(g *globals) *cobra.Command {
	var (
		to           string
		confirm      bool
		force        bool
		clean        bool
		identityFile string
	)

	cmd := &cobra.Command{
		Use:   "restore <backup-id>",
		Short: "Restore a backup into an explicit destination",
		Long: "restore streams a stored backup back into the database named by --to:\n" +
			"read from the bucket, decrypted, decompressed and handed to the engine's own\n" +
			"client, with the checksum compared against the manifest on the way past.\n\n" +
			"The destination is always explicit. --confirm is required because a restore\n" +
			"writes to a live database, and a destination that already holds data is\n" +
			"refused unless --force says otherwise.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageErrorf("--to is required: name the database to restore into")
			}
			if !confirm {
				return usageErrorf("restore writes to a live database; re-run with --confirm once you are sure of --to")
			}

			cfg, diags, err := g.load()
			if err != nil {
				return err
			}
			if diags.HasErrors() {
				g.printDiagnostics(diags)
				return fmt.Errorf("%s is invalid", g.configPath)
			}

			identities, err := loadIdentities(identityFile)
			if err != nil {
				return err
			}

			application := app.New(cfg, g.logger)
			targets, err := selectTargets(cfg, nil)
			if err != nil {
				return err
			}

			found, err := g.collect(cmd.Context(), application, targets, args[0], false)
			if err != nil {
				return err
			}
			if len(found) != 1 {
				return fmt.Errorf("no backup with id %q; `vaultd list` shows the ids", args[0])
			}
			item := found[0]

			// Restoring over a database vaultd itself backs up is almost
			// always a mistake, and never a silent one.
			if name, ok := configuredTarget(cfg, to); ok && !force {
				return fmt.Errorf(
					"--to points at %q, a database this config backs up; restoring into it would overwrite production. Pass --force if that is really the intent",
					name)
			}

			idx, err := application.Index(cmd.Context(), item.target)
			if err != nil {
				return err
			}
			m, err := idx.Manifest(cmd.Context(), item.entry.ManifestKey)
			if err != nil {
				return err
			}

			restorer, err := application.Restorer(m.Engine, to, clean)
			if err != nil {
				return err
			}
			store, err := application.Store(cmd.Context(), item.target.Destination)
			if err != nil {
				return err
			}

			spec := restore.Spec{Identities: identities, Force: force}
			if item.target.Encryption != nil {
				spec.Passphrase = item.target.Encryption.Passphrase.Reveal()
			}
			if item.target.Timeout != nil {
				spec.Timeout = item.target.Timeout.Duration()
			}

			runner := &restore.Runner{Store: store, Restorer: restorer, Log: g.logger}

			result, err := runner.Run(cmd.Context(), m, spec)
			if errors.Is(err, restore.ErrDestinationNotEmpty) {
				return fmt.Errorf("%w; pass --force to restore into it anyway, or point --to at an empty database", err)
			}
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ok: restored %s into the destination in %s\n", m.Target, humanDuration(result.Duration))
			fmt.Fprintf(w, "  backup\t%s (%s)\n", m.ID, m.FinishedAt.Format("2006-01-02 15:04Z"))
			fmt.Fprintf(w, "  engine\t%s %s\n", m.Engine, m.ServerVersion)
			fmt.Fprintf(w, "  written\t%s\n", humanBytes(result.Bytes))
			fmt.Fprintf(w, "  checksum\t%s, matching the manifest\n", shortSHA(result.SHA256))
			_ = w.Flush()

			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "connection string of the database to restore into (required)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "acknowledge that this writes to a live database")
	cmd.Flags().BoolVar(&force, "force", false, "allow a destination that already holds data")
	cmd.Flags().BoolVar(&clean, "clean", false, "drop the objects being restored before recreating them")
	cmd.Flags().StringVar(&identityFile, "identity-file", "",
		"file holding the age private key that decrypts the backup (or set "+identityEnv+")")

	return cmd
}

// configuredTarget reports whether a connection string is one this config
// backs up, which is the one destination a restore should never reach by
// accident.
func configuredTarget(cfg *config.Config, dsn string) (string, bool) {
	for _, target := range cfg.Targets {
		if target.Conn().Reveal() == dsn {
			return target.Name, true
		}
	}
	return "", false
}
