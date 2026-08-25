package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/backup"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/manifest"
)

func newBackupCommand(g *globals) *cobra.Command {
	var (
		dryRun bool
		tier   string
	)

	cmd := &cobra.Command{
		Use:   "backup <target>",
		Short: "Back up one target now",
		Long: "backup dumps one target and streams it to its destination: compressed,\n" +
			"encrypted and uploaded in one pass, with nothing buffered on disk. It writes\n" +
			"a manifest beside the object describing what was dumped and how to read it back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, target, err := g.targetFromConfig(args[0])
			if err != nil {
				return err
			}

			application := app.New(cfg, g.logger)
			spec, err := application.BackupSpec(target, tier)
			if err != nil {
				return err
			}
			runner, err := application.Runner(cmd.Context(), target)
			if err != nil {
				return err
			}

			if dryRun {
				plan, err := runner.Plan(cmd.Context(), spec)
				if err != nil {
					return err
				}
				g.printPlan(plan)
				return nil
			}

			m, err := runner.Run(cmd.Context(), spec)
			if err != nil {
				return err
			}

			g.printBackup(m)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "probe the server and report what would be written, without writing it")
	cmd.Flags().StringVar(&tier, "tier", "daily", "retention tier to record on the backup")

	return cmd
}

// targetFromConfig loads the config and resolves one target, refusing to run
// against a config that does not validate: a backup taken from a broken config
// is the kind of thing nobody notices until a restore.
func (g *globals) targetFromConfig(name string) (*config.Config, *config.Target, error) {
	cfg, diags, err := g.load()
	if err != nil {
		if formatted := config.FormatError(err); formatted != "" {
			fmt.Fprintf(g.err, "error: %s is not valid YAML\n%s\n", g.configPath, formatted)
			return nil, nil, &exitError{code: ExitError, err: errSilent}
		}
		return nil, nil, err
	}
	if diags.HasErrors() {
		g.printDiagnostics(diags)
		return nil, nil, fmt.Errorf("%s is invalid; fix it before running a backup", cfg.Path)
	}
	for _, d := range diags {
		fmt.Fprintln(g.err, d)
	}

	target, ok := cfg.Target(name)
	if !ok {
		return nil, nil, fmt.Errorf("target %q is not declared; %s", name, availableTargets(cfg))
	}
	return cfg, target, nil
}

func availableTargets(cfg *config.Config) string {
	if len(cfg.Targets) == 0 {
		return "the config declares none"
	}

	names := make([]string, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		names = append(names, t.Name)
	}
	return "declared targets are " + strings.Join(names, ", ")
}

func (g *globals) printPlan(p *backup.Plan) {
	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "dry run: %s would be backed up\n", p.Target)
	fmt.Fprintf(w, "  server\t%s %s\n", p.Engine, p.ServerVersion)
	fmt.Fprintf(w, "  consistency\t%s\n", p.Consistency)
	fmt.Fprintf(w, "  tables\t%d (%d rows, estimated)\n", p.Tables, p.Rows)
	fmt.Fprintf(w, "  pipeline\tcompression %s, encryption %s\n", p.Compression, p.Encryption)
	fmt.Fprintf(w, "  object\t%s\n", p.DataKey)
	fmt.Fprintf(w, "  manifest\t%s\n", p.ManifestKey)
	if p.GlobalsKey != "" {
		fmt.Fprintf(w, "  globals\t%s\n", p.GlobalsKey)
	}
	_ = w.Flush()

	for _, warning := range p.Warnings {
		fmt.Fprintln(g.err, "warning: "+warning)
	}
}

func (g *globals) printBackup(m *manifest.Manifest) {
	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ok: %s backed up in %s\n", m.Target, humanDuration(time.Duration(m.DurationMS)*time.Millisecond))
	fmt.Fprintf(w, "  object\t%s\n", m.Object.Key)
	fmt.Fprintf(w, "  size\t%s stored, %s dumped", humanBytes(m.Object.Bytes), humanBytes(m.Plaintext.Bytes))
	if r := ratio(m.Plaintext.Bytes, m.Object.Bytes); r != "" {
		fmt.Fprintf(w, " (%s)", r)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  sha256\t%s\n", shortSHA(m.Object.SHA256))
	fmt.Fprintf(w, "  pipeline\t%s, %s, %s\n", m.Pipeline.Compression, m.Pipeline.Encryption, m.Pipeline.Dumper)
	if m.Globals != nil {
		fmt.Fprintf(w, "  globals\t%s (%s)\n", m.Globals.Key, humanBytes(m.Globals.Bytes))
	}
	fmt.Fprintf(w, "  id\t%s\n", m.ID)
	_ = w.Flush()

	// What the server could not give — an oplog on a standalone, a
	// replication position without the privilege — is recorded on the manifest
	// and told to the operator now, not discovered during a restore.
	for _, warning := range m.Warnings {
		fmt.Fprintln(g.err, "warning: "+warning)
	}
}
