package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/curruwilla/vaultd/internal/config"
)

// checkTargets connects to every database the config backs up and reports what
// the probe found: version, the consistency it could achieve, how much there
// is to dump, and anything it had to degrade.
//
// The probe is the same one a backup runs, so a green line here means the
// backup's first step already works — including the client-version rule, which
// only a probe can decide (SPEC §3).
func (d *Doctor) checkTargets(ctx context.Context, report *Report, targets []*config.Target) {
	for _, target := range targets {
		report.add(d.withTimeout(ctx, func(ctx context.Context) Check {
			return d.probeTarget(ctx, target)
		}))
	}
}

func (d *Doctor) probeTarget(ctx context.Context, target *config.Target) Check {
	check := Check{Group: "targets", Name: target.Name}

	dumper, err := d.App.Dumper(target)
	if err != nil {
		check.Status = StatusFail
		check.Detail = err.Error()
		return check
	}

	info, err := dumper.Probe(ctx)
	if err != nil {
		check.Status = StatusFail
		check.Detail = err.Error()
		check.Hint = "vaultd connects with the target's own dsn; check the credentials and that this host can reach the server"
		return check
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("%s %s, %s, %s", info.Engine, info.Version,
		plural(len(info.Tables), "table", "tables"), info.Consistency)

	if len(info.Warnings) > 0 {
		check.Status = StatusWarn
		check.Hint = strings.Join(info.Warnings, "; ")
	}
	return check
}

// checkVerifyTargets probes the staging servers L2 restores into, and checks
// the rule that decides whether a verification can run at all: a staging major
// older than the source cannot restore its dump (SPEC §8).
func (d *Doctor) checkVerifyTargets(ctx context.Context, report *Report, targets []*config.Target, cfg *config.Config) {
	used := map[string]bool{}
	for _, target := range targets {
		if target.Verify != nil && target.Verify.Into != "" {
			used[target.Verify.Into] = true
		}
	}

	for _, name := range sortedNames(used) {
		into, ok := cfg.VerifyTarget(name)
		if !ok {
			report.add(Check{
				Group:  "verify targets",
				Name:   name,
				Status: StatusFail,
				Detail: "not declared",
			})
			continue
		}

		report.add(d.withTimeout(ctx, func(ctx context.Context) Check {
			return d.probeVerifyTarget(ctx, into)
		}))
	}
}

func (d *Doctor) probeVerifyTarget(ctx context.Context, into *config.VerifyTarget) Check {
	check := Check{Group: "verify targets", Name: into.Name}

	provisioner, err := d.App.Provisioner(into)
	if err != nil {
		check.Status = StatusFail
		check.Detail = err.Error()
		return check
	}

	info, err := provisioner.Probe(ctx)
	if err != nil {
		check.Status = StatusFail
		check.Detail = err.Error()
		check.Hint = "the verify target needs CREATEDB (PostgreSQL), CREATE/DROP (MySQL) or dbAdminAnyDatabase (MongoDB), scoped to " + into.DatabasePrefix
		return check
	}

	// Anything already sitting under the prefix is a database a crashed run
	// left behind. It is not a failure — `verify --gc` is exactly the repair —
	// but it costs disk on someone's staging server until it is collected.
	leftovers, err := provisioner.List(ctx)
	if err != nil {
		check.Status = StatusWarn
		check.Detail = fmt.Sprintf("%s %s, but its databases could not be listed: %s", info.Engine, info.Version, err.Error())
		return check
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("%s %s, prefix %s", info.Engine, info.Version, into.DatabasePrefix)
	if len(leftovers) > 0 {
		check.Status = StatusWarn
		check.Hint = plural(len(leftovers), "database", "databases") +
			" left over from an interrupted run; collect them with `vaultd verify --gc`"
	}
	return check
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
