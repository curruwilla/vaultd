package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/config"
)

// validateReport is the --json shape of `vaultd validate`.
type validateReport struct {
	Path        string             `json:"path"`
	OK          bool               `json:"ok"`
	Diagnostics config.Diagnostics `json:"diagnostics"`
	Summary     summary            `json:"summary"`
}

type summary struct {
	Targets       int `json:"targets"`
	Destinations  int `json:"destinations"`
	VerifyTargets int `json:"verify_targets"`
	Notifiers     int `json:"notifiers"`
	Errors        int `json:"errors"`
	Warnings      int `json:"warnings"`
}

func newValidateCommand(g *globals) *cobra.Command {
	var asJSON, printEffective bool

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the config file without touching the network",
		Long: "validate parses the config, expands its ${VAR} references, applies the\n" +
			"defaults every target inherits and runs every semantic check: cross-references,\n" +
			"cron syntax, age recipients, per-engine options and the mandatory encryption\n" +
			"rule. It never connects to a database, a bucket or a webhook — that is `vaultd doctor`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, diags, err := g.load()
			if err != nil {
				if formatted := config.FormatError(err); formatted != "" {
					fmt.Fprintf(g.err, "error: %s is not valid YAML\n%s\n", g.configPath, formatted)
					return &exitError{code: ExitError, err: errSilent}
				}
				return err
			}

			report := validateReport{
				Path:        cfg.Path,
				OK:          !diags.HasErrors(),
				Diagnostics: diags,
				Summary: summary{
					Targets:       len(cfg.Targets),
					Destinations:  len(cfg.Destinations),
					VerifyTargets: len(cfg.VerifyTargets),
					Notifiers:     len(cfg.Notifiers),
					Errors:        diags.Count(config.SeverityError),
					Warnings:      diags.Count(config.SeverityWarn),
				},
			}

			if asJSON {
				if err := writeJSON(g, report); err != nil {
					return err
				}
			} else {
				g.printDiagnostics(diags)
				g.printSummary(report)
			}

			if printEffective && report.OK {
				effective, err := config.Marshal(cfg)
				if err != nil {
					return fmt.Errorf("rendering the effective config: %w", err)
				}
				fmt.Fprintf(g.out, "\n# effective config (secrets redacted)\n%s", effective)
			}

			if diags.HasErrors() {
				return &exitError{code: ExitError, err: errSilent}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "report diagnostics as JSON")
	cmd.Flags().BoolVar(&printEffective, "print-effective", false, "print the merged config with secrets redacted")

	return cmd
}

func (g *globals) printDiagnostics(diags config.Diagnostics) {
	for _, d := range diags {
		fmt.Fprintln(g.err, d)
	}
}

func (g *globals) printSummary(r validateReport) {
	if !r.OK {
		fmt.Fprintf(g.err, "\n%s is invalid: %s, %s\n",
			r.Path, plural(r.Summary.Errors, "error", "errors"), plural(r.Summary.Warnings, "warning", "warnings"))
		return
	}

	fmt.Fprintf(g.out, "ok: %s is valid — %s, %s, %s, %s",
		r.Path,
		plural(r.Summary.Targets, "target", "targets"),
		plural(r.Summary.Destinations, "destination", "destinations"),
		plural(r.Summary.VerifyTargets, "verify target", "verify targets"),
		plural(r.Summary.Notifiers, "notifier", "notifiers"))
	if r.Summary.Warnings > 0 {
		fmt.Fprintf(g.out, " (%s)", plural(r.Summary.Warnings, "warning", "warnings"))
	}
	fmt.Fprintln(g.out)
}

func writeJSON(g *globals, v any) error {
	enc := json.NewEncoder(g.out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("writing JSON output: %w", err)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
