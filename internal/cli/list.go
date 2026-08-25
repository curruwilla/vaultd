package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
)

// manifestFetchers is how many manifests are read in parallel. A listing of a
// year of daily backups is a few hundred small objects; without some
// concurrency it is a few hundred round trips.
const manifestFetchers = 8

func newListCommand(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list [target]",
		Short: "List the backups of a target, newest first",
		Long: "list reads the manifests stored in the bucket. The bucket is the source of\n" +
			"truth: a listing works against a fresh container with no local state.",
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

			var all []*manifest.Manifest
			for _, target := range targets {
				manifests, err := listTarget(cmd.Context(), application, target)
				if err != nil {
					return err
				}
				all = append(all, manifests...)
			}

			// Newest first: the question is almost always "how old is the
			// most recent backup".
			sort.Slice(all, func(i, j int) bool { return all[i].FinishedAt.After(all[j].FinishedAt) })

			if asJSON {
				return writeJSON(g, all)
			}
			g.printBackups(all)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the manifests as JSON")

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

// listTarget reads every manifest under a target's prefix.
func listTarget(ctx context.Context, application *app.App, target *config.Target) ([]*manifest.Manifest, error) {
	store, err := application.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	layout, err := application.Layout(target)
	if err != nil {
		return nil, err
	}

	var keys []string
	for object, err := range store.List(ctx, layout.TargetPrefix()) {
		if err != nil {
			return nil, fmt.Errorf("listing backups of %s: %w", target.Name, err)
		}
		if manifest.IsManifestKey(object.Key) {
			keys = append(keys, object.Key)
		}
	}

	manifests := make([]*manifest.Manifest, len(keys))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(manifestFetchers)
	for i, key := range keys {
		g.Go(func() error {
			m, err := fetchManifest(ctx, store, key)
			if err != nil {
				return err
			}
			manifests[i] = m
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return manifests, nil
}

func fetchManifest(ctx context.Context, store core.Store, key string) (*manifest.Manifest, error) {
	body, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	m, err := manifest.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	return m, nil
}

func (g *globals) printBackups(manifests []*manifest.Manifest) {
	if len(manifests) == 0 {
		fmt.Fprintln(g.out, "no backups found")
		return
	}

	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tFINISHED\tAGE\tSTORED\tDUMPED\tTIER\tVERIFY\tID")

	now := time.Now().UTC()
	for _, m := range manifests {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Target,
			m.FinishedAt.Format("2006-01-02 15:04Z"),
			humanDuration(m.Age(now)),
			humanBytes(m.Object.Bytes),
			humanBytes(m.Plaintext.Bytes),
			orDash(m.Tier),
			verifyStatus(m),
			m.ID,
		)
	}
	_ = w.Flush()
}

func verifyStatus(m *manifest.Manifest) string {
	if m.Verify == nil {
		return "—"
	}
	if m.Verify.OK {
		return m.Verify.Level + " ok"
	}
	return m.Verify.Level + " FAILED"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
