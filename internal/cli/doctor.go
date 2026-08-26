package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/doctor"
)

func newDoctorCommand(g *globals) *cobra.Command {
	var (
		asJSON     bool
		sendNotify bool
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "doctor [target...]",
		Short: "Check connectivity: databases, client binaries, bucket, notifiers",
		Long: "doctor is the half of the config check that needs the network.\n\n" +
			"It reports which database clients are installed, probes every target's\n" +
			"server, writes and deletes a canary object in each bucket — including the\n" +
			"conditional writes the target lock and the index depend on — and checks that\n" +
			"the notifier endpoints answer.\n\n" +
			"Notifiers are only dialled, not posted to: a notifier subscribed to\n" +
			"backup.failed usually points at somebody's pager. Pass --notify to send a\n" +
			"real signed test delivery.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, diags, err := g.load()
			if err != nil {
				return err
			}
			if diags.HasErrors() {
				g.printDiagnostics(diags)
				return fmt.Errorf("%s is invalid; doctor checks a config that parses", cfg.Path)
			}
			for _, d := range diags {
				fmt.Fprintln(g.err, d)
			}

			// Naming an unknown target is a typo worth catching before a
			// report that silently checked nothing.
			targets, err := selectTargets(cfg, args)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(targets))
			for _, target := range targets {
				names = append(names, target.Name)
			}

			doc := &doctor.Doctor{
				App:     app.New(cfg, g.logger),
				Log:     g.logger,
				Targets: names,
				Notify:  sendNotify,
				Timeout: timeout,
			}
			report := doc.Run(cmd.Context())

			if asJSON {
				if err := g.printJSON(report); err != nil {
					return err
				}
			} else {
				g.printReport(report)
			}

			if !report.OK() {
				return &exitError{code: ExitError, err: errSilent}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.Flags().BoolVar(&sendNotify, "notify", false,
		"send a real signed test delivery to every notifier instead of only dialling it")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long one check may take (default 15s)")

	return cmd
}

func (g *globals) printReport(report *doctor.Report) {
	for _, group := range report.Groups() {
		fmt.Fprintf(g.out, "\n%s\n", group)

		w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
		for _, check := range report.Checks {
			if check.Group != group {
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", mark(check.Status), check.Name, check.Detail)
			if check.Hint != "" {
				fmt.Fprintf(w, "  \t\t→ %s\n", check.Hint)
			}
		}
		_ = w.Flush()
	}

	counts := report.Counts()
	fmt.Fprintf(g.out, "\n%d ok, %d warn, %d fail\n",
		counts[doctor.StatusOK], counts[doctor.StatusWarn], counts[doctor.StatusFail])

	if !report.OK() {
		fmt.Fprintln(g.err, "\nerror: this config cannot back up as it stands")
	}
}

// mark is the leading glyph of a report line. They are ASCII on purpose: this
// output gets pasted into issues, CI logs and terminals with no font for
// anything cleverer.
func mark(status doctor.Status) string {
	switch status {
	case doctor.StatusOK:
		return "[ ok ]"
	case doctor.StatusWarn:
		return "[warn]"
	case doctor.StatusFail:
		return "[FAIL]"
	default:
		return "[skip]"
	}
}

func (g *globals) printJSON(v any) error {
	encoder := json.NewEncoder(g.out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}
