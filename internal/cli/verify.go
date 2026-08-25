package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/verify"
)

func newVerifyCommand(g *globals) *cobra.Command {
	var (
		targetName   string
		latest       bool
		level        string
		identityFile string
		gc           bool
	)

	cmd := &cobra.Command{
		Use:   "verify [backup-id]",
		Short: "Verify a backup: that it is there, and that it reads back",
		Long: "verify checks a stored backup against its manifest.\n\n" +
			"  integrity   the objects exist and are the size the manifest records (one HEAD,\n" +
			"              no egress)\n" +
			"  structural  the object is read back in full, decrypted, decompressed and\n" +
			"              compared byte for byte with the manifest\n\n" +
			"The outcome is written onto the manifest and into the index, which is what\n" +
			"stops prune from deleting the most recent verified backup.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gc {
				return errors.New("--gc collects leftover verify databases, which arrive with restore verification in milestone M5")
			}

			wanted := verify.Level(level)
			if wanted != verify.LevelIntegrity && wanted != verify.LevelStructural {
				return usageErrorf("unknown verify level %q; use integrity or structural", level)
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

			var backupID string
			if len(args) == 1 {
				backupID = args[0]
			}
			if backupID == "" && targetName == "" {
				return usageErrorf("name a backup id, or pass --target with --latest")
			}

			application := app.New(cfg, g.logger)
			targets, err := selectTargets(cfg, trimmed(targetName))
			if err != nil {
				return err
			}

			work, err := g.collect(cmd.Context(), application, targets, backupID, latest)
			if err != nil {
				return err
			}
			if len(work) == 0 {
				return errors.New("no backup matched")
			}

			failures := 0
			for _, item := range work {
				verifier, err := application.Verifier(cmd.Context(), item.target, identities)
				if err != nil {
					return err
				}

				result, err := verifier.Backup(cmd.Context(), item.entry, wanted)
				if err != nil {
					return err
				}

				g.printVerification(item.target.Name, item.entry, result)
				if !result.OK {
					failures++
				}
			}

			if failures > 0 {
				return &exitError{code: ExitError, err: errSilent}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&targetName, "target", "", "verify backups of this target")
	cmd.Flags().BoolVar(&latest, "latest", false, "with --target, verify only its most recent backup")
	cmd.Flags().StringVar(&level, "level", string(verify.LevelIntegrity), "integrity or structural")
	cmd.Flags().StringVar(&identityFile, "identity-file", "",
		"file holding the age private key that decrypts the backup (or set "+identityEnv+")")
	cmd.Flags().BoolVar(&gc, "gc", false, "drop leftover verify databases from a crashed run")

	return cmd
}

// item is one backup to verify, with the target it belongs to.
type item struct {
	target *config.Target
	entry  manifest.Entry
}

// collect finds the backups a verify invocation names.
func (g *globals) collect(
	ctx context.Context,
	application *app.App,
	targets []*config.Target,
	backupID string,
	latest bool,
) ([]item, error) {
	var work []item

	for _, target := range targets {
		idx, err := application.Index(ctx, target)
		if err != nil {
			return nil, err
		}
		entries, _, err := idx.Entries(ctx)
		if err != nil {
			return nil, err
		}

		succeeded := make([]manifest.Entry, 0, len(entries))
		for _, entry := range entries {
			if entry.Succeeded() {
				succeeded = append(succeeded, entry)
			}
		}
		sort.Slice(succeeded, func(a, b int) bool {
			return succeeded[a].FinishedAt.After(succeeded[b].FinishedAt)
		})

		switch {
		case backupID != "":
			for _, entry := range succeeded {
				if entry.ID == backupID {
					return []item{{target: target, entry: entry}}, nil
				}
			}
		case latest:
			if len(succeeded) > 0 {
				work = append(work, item{target: target, entry: succeeded[0]})
			}
		default:
			for _, entry := range succeeded {
				work = append(work, item{target: target, entry: entry})
			}
		}
	}

	if backupID != "" && len(work) == 0 {
		return nil, fmt.Errorf("no backup with id %q; `vaultd list` shows the ids", backupID)
	}
	return work, nil
}

func (g *globals) printVerification(target string, entry manifest.Entry, result verify.Result) {
	out := g.out
	if !result.OK {
		out = g.err
	}

	fmt.Fprintf(out, "%s\n", result.Summary(target))

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  backup\t%s (%s)\n", entry.ID, entry.FinishedAt.Format("2006-01-02 15:04Z"))
	fmt.Fprintf(w, "  object\t%s\n", entry.Key)
	fmt.Fprintf(w, "  level\t%s in %s\n", result.Level, humanDuration(result.Duration))
	if bytes, ok := result.Details["plaintext_bytes"].(int64); ok {
		fmt.Fprintf(w, "  read back\t%s\n", humanBytes(bytes))
	}
	if format, ok := result.Details["format"].(string); ok {
		fmt.Fprintf(w, "  format\t%s\n", format)
	}
	_ = w.Flush()

	for _, problem := range result.Problems {
		fmt.Fprintf(g.err, "  problem: %s\n", problem)
	}
}

// trimmed turns an optional target name into the argument list selectTargets
// expects.
func trimmed(name string) []string {
	if name == "" {
		return nil
	}
	return []string{name}
}
