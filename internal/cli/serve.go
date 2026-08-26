package cli

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/metrics"
	"github.com/curruwilla/vaultd/internal/scheduler"
	"github.com/curruwilla/vaultd/internal/server"
)

// daemon holds the flags `serve` and `run` share.
type daemon struct {
	identityFile string
	noPrune      bool
	lockTTL      time.Duration
}

func (d *daemon) flags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&d.identityFile, "identity-file", "",
		"file holding the age private key, for scheduled structural and restore verification (or set "+identityEnv+")")
	cmd.Flags().BoolVar(&d.noPrune, "no-prune", false,
		"do not apply retention after a successful backup")
	cmd.Flags().DurationVar(&d.lockTTL, "lock-ttl", 0,
		"how long a target lock lives without a heartbeat (default 5m)")
}

func newServeCommand(g *globals) *cobra.Command {
	var (
		d        daemon
		listen   string
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon: scheduler, locks, metrics and UI",
		Long: "serve is the primary way to run vaultd (decision D4). It evaluates every\n" +
			"target's schedule, takes each target's lock before running it, applies the\n" +
			"retention policy after a successful backup, and serves /metrics, /healthz,\n" +
			"/readyz and the API.\n\n" +
			"It keeps no state of its own: what is due is derived from the config and from\n" +
			"the index in the bucket, so a restart neither loses a schedule nor repeats\n" +
			"one, and two replicas cannot run the same target at once.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.daemonConfig()
			if err != nil {
				return err
			}
			if listen != "" {
				cfg.Server.Listen = listen
			}

			identities, err := loadIdentities(d.identityFile)
			if err != nil {
				return err
			}

			jobs, err := scheduler.Jobs(cfg)
			if err != nil {
				return err
			}
			if err := verifiableWithoutAnIdentity(cfg, jobs, identities); err != nil {
				return err
			}

			application := app.New(cfg, g.logger)
			recorder := metrics.New()
			seedMetrics(cmd.Context(), application, recorder, g)

			sched := &scheduler.Scheduler{
				Jobs:     jobs,
				Interval: interval,
				Log:      g.logger,
				Exec: &scheduler.Executor{
					App:        application,
					Log:        g.logger,
					Metrics:    recorder,
					Identities: identities,
					Prune:      !d.noPrune,
					LockTTL:    d.lockTTL,
				},
			}

			http := &server.Server{
				App:     application,
				Metrics: recorder,
				Log:     g.logger,
				Status:  statusOf(sched, jobs),
			}

			g.printSchedule(sched, jobs)

			// SIGTERM is how a container is asked to stop, and the daemon has
			// to treat it as "finish the dump you are on, release the lock":
			// a killed run leaves a lease behind that blocks the target until
			// it expires.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			group, ctx := errgroup.WithContext(ctx)
			group.Go(func() error { return sched.Run(ctx) })
			group.Go(func() error { return http.ListenAndServe(ctx) })

			if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			fmt.Fprintln(g.out, "stopped")
			return nil
		},
	}

	d.flags(cmd)
	cmd.Flags().StringVar(&listen, "listen", "", "address to serve on, overriding server.listen")
	cmd.Flags().DurationVar(&interval, "interval", 0, "how often to evaluate the schedule (default 30s)")

	return cmd
}

func newRunCommand(g *globals) *cobra.Command {
	var (
		d      daemon
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run every target that is due, once, then exit",
		Long: "run is the one-shot form of the daemon, for a Kubernetes CronJob or a\n" +
			"systemd timer. It works out what is due from the same config and the same\n" +
			"index `vaultd serve` reads, takes the same per-target lock, and exits.\n\n" +
			"Two invocations that overlap do not collide: the second finds the lock held,\n" +
			"or finds the work already done, and reports it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.daemonConfig()
			if err != nil {
				return err
			}

			identities, err := loadIdentities(d.identityFile)
			if err != nil {
				return err
			}

			jobs, err := scheduler.Jobs(cfg)
			if err != nil {
				return err
			}
			if err := verifiableWithoutAnIdentity(cfg, jobs, identities); err != nil {
				return err
			}

			application := app.New(cfg, g.logger)
			sched := &scheduler.Scheduler{
				Jobs: jobs,
				Log:  g.logger,
				Exec: &scheduler.Executor{
					App:        application,
					Log:        g.logger,
					Identities: identities,
					Prune:      !d.noPrune,
					LockTTL:    d.lockTTL,
				},
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if dryRun {
				due, err := sched.Due(ctx)
				if err != nil {
					return err
				}
				g.printDue(due)
				return nil
			}

			results, err := sched.RunOnce(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}

			return g.printResults(results)
		},
	}

	d.flags(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what is due and run nothing")

	return cmd
}

// daemonConfig loads a config the daemon may actually run on. Unlike a
// one-off command, a daemon that starts on a broken config would look healthy
// while backing nothing up.
func (g *globals) daemonConfig() (*config.Config, error) {
	cfg, diags, err := g.load()
	if err != nil {
		if formatted := config.FormatError(err); formatted != "" {
			fmt.Fprintf(g.err, "error: %s is not valid YAML\n%s\n", g.configPath, formatted)
			return nil, &exitError{code: ExitError, err: errSilent}
		}
		return nil, err
	}
	if diags.HasErrors() {
		g.printDiagnostics(diags)
		return nil, fmt.Errorf("%s is invalid; the daemon will not start on a config it cannot trust", cfg.Path)
	}
	for _, d := range diags {
		fmt.Fprintln(g.err, d)
	}
	return cfg, nil
}

// verifiableWithoutAnIdentity refuses to start when a scheduled verification
// could never run.
//
// Structural and restore verification have to decrypt the backup, and the
// private key is deliberately not something vaultd stores. Starting anyway
// would mean a nightly check that fails every night for a reason nobody reads
// until the week they need a restore — so it is a startup error instead
// (SPEC §8, §15).
func verifiableWithoutAnIdentity(cfg *config.Config, jobs []scheduler.Job, identities []age.Identity) error {
	if len(identities) > 0 {
		return nil
	}

	for _, job := range jobs {
		if job.Kind != scheduler.KindVerify || job.Level == config.VerifyIntegrity || job.Level == "" {
			continue
		}

		target, ok := cfg.Target(job.Target)
		if !ok || target.Encryption == nil || target.Encryption.Mode != config.EncryptionAge {
			continue
		}

		return fmt.Errorf(
			"target %q schedules %s verification of age-encrypted backups but no age identity was given; "+
				"pass --identity-file or set %s",
			target.Name, job.Level, identityEnv)
	}
	return nil
}

// seedMetrics publishes what the index already knows, so a restarted daemon
// does not read as a fleet of targets that have never backed up.
func seedMetrics(ctx context.Context, application *app.App, recorder *metrics.Metrics, g *globals) {
	cfg := application.Config()

	for i := range cfg.Targets {
		target := &cfg.Targets[i]

		idx, err := application.Index(ctx, target)
		if err != nil {
			fmt.Fprintf(g.err, "warning: %s: the metrics could not be seeded: %s\n", target.Name, err)
			continue
		}
		entries, _, err := idx.Entries(ctx)
		if err != nil {
			fmt.Fprintf(g.err, "warning: %s: the metrics could not be seeded: %s\n", target.Name, err)
			continue
		}

		for _, entry := range entries {
			if !entry.Succeeded() {
				continue
			}
			recorder.SeedBackup(target.Name, entry.FinishedAt, entry.Bytes, entry.PlaintextBytes)
			if entry.VerifiedAt != nil && entry.Verified() {
				recorder.SeedVerify(target.Name, entry.VerifyLevel, *entry.VerifiedAt)
			}
		}
	}
}

// statusOf adapts the scheduler to what the API reports.
func statusOf(sched *scheduler.Scheduler, jobs []scheduler.Job) server.StatusFunc {
	started := time.Now().UTC()

	return func(context.Context) server.Status {
		status := server.Status{
			StartedAt: started,
			Uptime:    time.Since(started).Round(time.Second).String(),
			Jobs:      make([]server.JobStatus, 0, len(jobs)),
		}
		for _, job := range jobs {
			status.Jobs = append(status.Jobs, server.JobStatus{
				Target:   job.Target,
				Kind:     string(job.Kind),
				Schedule: job.Spec,
				Next:     sched.Next(job),
			})
		}
		return status
	}
}

func (g *globals) printSchedule(sched *scheduler.Scheduler, jobs []scheduler.Job) {
	if len(jobs) == 0 {
		fmt.Fprintln(g.err, "warning: no target declares a schedule; the daemon will serve metrics and nothing else")
		return
	}

	sorted := append([]scheduler.Job(nil), jobs...)
	sort.Slice(sorted, func(a, b int) bool { return sched.Next(sorted[a]).Before(sched.Next(sorted[b])) })

	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tKIND\tSCHEDULE\tNEXT")
	for _, job := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", job.Target, job.Kind, job.Spec,
			sched.Next(job).Format("2006-01-02 15:04Z"))
	}
	_ = w.Flush()
}

func (g *globals) printDue(due []scheduler.Job) {
	if len(due) == 0 {
		fmt.Fprintln(g.out, "nothing is due")
		return
	}

	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DUE\tKIND\tSCHEDULE")
	for _, job := range due {
		fmt.Fprintf(w, "%s\t%s\t%s\n", job.Target, job.Kind, job.Spec)
	}
	_ = w.Flush()
}

// printResults reports one line per dispatch and fails the command if any run
// failed, so a CronJob's exit code means what a CronJob's exit code should.
func (g *globals) printResults(results []scheduler.Result) error {
	if len(results) == 0 {
		fmt.Fprintln(g.out, "nothing was due")
		return nil
	}

	failed := 0
	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	for _, result := range results {
		out := "ok"
		switch {
		case result.Err != nil:
			out = "FAILED"
			failed++
		case result.Outcome != scheduler.OutcomeRan:
			out = string(result.Outcome)
		}

		detail := result.Detail
		if result.Err != nil {
			detail = result.Err.Error()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			out, result.Job.Target, result.Job.Kind, humanDuration(result.Duration), detail)
	}
	_ = w.Flush()

	if failed > 0 {
		return &exitError{code: ExitError, err: errSilent}
	}
	return nil
}
