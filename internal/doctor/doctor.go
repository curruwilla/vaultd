// Package doctor is the network half of the config check (SPEC §9).
//
// `vaultd validate` proves the file is coherent without opening a socket;
// doctor proves the world it describes actually exists — the clients are
// installed, the databases answer, the bucket accepts the writes vaultd
// depends on, and the notifier endpoints are reachable.
//
// Every check runs even when an earlier one failed, for the same reason
// config.Validate collects its diagnostics: an operator setting this up wants
// the whole list, not the first line of it.
package doctor

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
)

// Status is the outcome of one check.
type Status string

const (
	// StatusOK means the thing works.
	StatusOK Status = "ok"
	// StatusWarn means it works but something will bite later.
	StatusWarn Status = "warn"
	// StatusFail means a backup would not succeed right now.
	StatusFail Status = "fail"
	// StatusSkip means the check was not attempted, and says why.
	StatusSkip Status = "skip"
)

// Check is one thing doctor looked at.
type Check struct {
	// Group is the section it is printed under: clients, destinations,
	// targets, verify targets, notifiers.
	Group string `json:"group"`
	// Name is what was checked — a target name, a binary, a bucket.
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Detail is the one-line answer: a version, an error, a reason.
	Detail string `json:"detail"`
	// Hint is what to do about a failure, when there is something to say.
	Hint string `json:"hint,omitempty"`
	// TookMS is how long the check took, which is itself diagnostic: a probe
	// that answers in four seconds is a network problem waiting to happen.
	TookMS int64 `json:"took_ms"`
}

// Report is everything doctor found.
type Report struct {
	Checks  []Check   `json:"checks"`
	Variant string    `json:"variant"`
	At      time.Time `json:"at"`
}

// OK reports whether nothing failed. Warnings do not make a report fail: they
// are things to fix, not things that stop a backup tonight.
func (r *Report) OK() bool {
	for _, check := range r.Checks {
		if check.Status == StatusFail {
			return false
		}
	}
	return true
}

// Counts returns how many checks landed in each status.
func (r *Report) Counts() map[Status]int {
	counts := map[Status]int{}
	for _, check := range r.Checks {
		counts[check.Status]++
	}
	return counts
}

// Groups returns the check groups in the order they were produced.
func (r *Report) Groups() []string {
	var (
		groups []string
		seen   = map[string]bool{}
	)
	for _, check := range r.Checks {
		if !seen[check.Group] {
			seen[check.Group] = true
			groups = append(groups, check.Group)
		}
	}
	return groups
}

// Doctor runs the checks against one configuration.
type Doctor struct {
	App *app.App
	Log *slog.Logger
	// Targets narrows the run to these target names; empty means all of them.
	Targets []string
	// Notify sends a real signed delivery to each notifier instead of only
	// checking that the endpoint answers.
	//
	// It is off by default on purpose: a notifier subscribed to
	// backup.failed points at somebody's pager, and a health check that pages
	// on-call every time it runs teaches people to mute the channel.
	Notify bool
	// Timeout bounds each individual check.
	Timeout time.Duration
}

// defaultTimeout is what one check gets before it counts as unreachable.
const defaultTimeout = 15 * time.Second

// Run performs every check and returns what it found.
func (d *Doctor) Run(ctx context.Context) *Report {
	report := &Report{At: time.Now().UTC(), Variant: Variant()}

	cfg := d.App.Config()
	targets := d.selected(cfg)

	d.checkClients(ctx, report, targets, cfg)
	d.checkDestinations(ctx, report, targets)
	d.checkTargets(ctx, report, targets)
	d.checkVerifyTargets(ctx, report, targets, cfg)
	d.checkNotifiers(ctx, report, cfg)

	return report
}

// selected resolves which targets this run covers.
func (d *Doctor) selected(cfg *config.Config) []*config.Target {
	if len(d.Targets) == 0 {
		out := make([]*config.Target, 0, len(cfg.Targets))
		for i := range cfg.Targets {
			out = append(out, &cfg.Targets[i])
		}
		return out
	}

	out := make([]*config.Target, 0, len(d.Targets))
	for _, name := range d.Targets {
		if target, ok := cfg.Target(name); ok {
			out = append(out, target)
		}
	}
	return out
}

// add records one check, timing it.
func (r *Report) add(check Check) { r.Checks = append(r.Checks, check) }

func (d *Doctor) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// timeout bounds one check.
func (d *Doctor) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return defaultTimeout
}

// withTimeout runs one check under its own deadline and times it, so a
// hanging endpoint costs one check rather than the whole run.
func (d *Doctor) withTimeout(ctx context.Context, fn func(context.Context) Check) Check {
	started := time.Now()

	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	check := fn(ctx)
	check.TookMS = time.Since(started).Milliseconds()
	return check
}

// sortedNames keeps the report stable between runs.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
