package cli

import (
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/manifest"
)

func newListCommand(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list [target]",
		Short: "List the backups of a target, newest first",
		Long: "list reads the index in the bucket, falling back to the manifests when there\n" +
			"is no index. The bucket is the source of truth: a listing works against a\n" +
			"fresh container with no local state.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, diags, err := g.load()
			if err != nil {
				return err
			}
			if diags.HasErrors() {
				g.printDiagnostics(diags)
				return fmt.Errorf("%s is invalid", g.configPath)
			}

			targets, err := selectTargets(cfg, args)
			if err != nil {
				return err
			}

			application := app.New(cfg, g.logger)

			var all []manifest.Entry
			for _, target := range targets {
				idx, err := application.Index(cmd.Context(), target)
				if err != nil {
					return err
				}

				entries, cached, err := idx.Entries(cmd.Context())
				if err != nil {
					return err
				}
				if !cached && len(entries) > 0 {
					fmt.Fprintf(g.err, "warning: %s has no index; it was read from the manifests. Run `vaultd reindex %s`.\n",
						target.Name, target.Name)
				}
				all = append(all, entries...)
			}

			// Newest first: the question is almost always "how old is the most
			// recent backup".
			sort.Slice(all, func(i, j int) bool { return all[i].FinishedAt.After(all[j].FinishedAt) })

			if asJSON {
				return writeJSON(g, all)
			}
			g.printEntries(all)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the index entries as JSON")

	return cmd
}

func selectTargets(cfg *config.Config, args []string) ([]*config.Target, error) {
	if len(args) == 0 {
		targets := make([]*config.Target, 0, len(cfg.Targets))
		for i := range cfg.Targets {
			targets = append(targets, &cfg.Targets[i])
		}
		return targets, nil
	}

	target, ok := cfg.Target(args[0])
	if !ok {
		return nil, fmt.Errorf("target %q is not declared; %s", args[0], availableTargets(cfg))
	}
	return []*config.Target{target}, nil
}

func (g *globals) printEntries(entries []manifest.Entry) {
	if len(entries) == 0 {
		fmt.Fprintln(g.out, "no backups found")
		return
	}

	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tFINISHED\tAGE\tSTATUS\tSTORED\tDUMPED\tTIER\tVERIFY\tID")

	now := time.Now().UTC()
	for _, entry := range entries {
		if !entry.Succeeded() {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t—\t—\t—\t—\t—\n",
				entry.Target,
				entry.FinishedAt.Format("2006-01-02 15:04Z"),
				humanDuration(now.Sub(entry.FinishedAt)),
				failureStatus(entry))
			continue
		}

		fmt.Fprintf(w, "%s\t%s\t%s\tok\t%s\t%s\t%s\t%s\t%s\n",
			entry.Target,
			entry.FinishedAt.Format("2006-01-02 15:04Z"),
			humanDuration(now.Sub(entry.FinishedAt)),
			humanBytes(entry.Bytes),
			humanBytes(entry.PlaintextBytes),
			orDash(entry.Tier),
			verifyStatus(entry),
			entry.ID)
	}
	_ = w.Flush()
}

func failureStatus(entry manifest.Entry) string {
	if entry.Phase == "" {
		return "FAILED"
	}
	return "FAILED (" + entry.Phase + ")"
}

func verifyStatus(entry manifest.Entry) string {
	if entry.VerifyOK == nil {
		return "—"
	}
	if *entry.VerifyOK {
		return entry.VerifyLevel + " ok"
	}
	return entry.VerifyLevel + " FAILED"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
