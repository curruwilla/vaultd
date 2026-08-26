// Package scheduler runs targets on their declared schedules (decision D4).
//
// The daemon keeps no state of its own. What is due is derived every time from
// two things that already exist: the cron expression in the config, and when
// the target last ran, which the index in the bucket records. A daemon that
// has just started, a daemon that has been up for a month and a one-shot
// `vaultd run` from a Kubernetes CronJob all compute the same answer — so a
// restart neither loses a schedule nor repeats one.
//
// Two replicas reach that same answer at the same moment, which is exactly
// what the per-target lock is for. The lock is not the whole story though: a
// replica whose cached view is a few minutes stale would take the lock the
// instant the other released it and run the backup twice. So the due check
// happens again, against the index, once the lock is held (see exec.go).
package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/curruwilla/vaultd/internal/config"
)

// Kind is what a scheduled run does.
type Kind string

const (
	KindBackup Kind = "backup"
	KindVerify Kind = "verify"
)

// parser accepts the same dialect config validation accepts, which is what
// makes `vaultd validate` a real check on a schedule: five fields, plus the
// @daily-style descriptors.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Job is one target's scheduled work of one kind.
type Job struct {
	Target string
	Kind   Kind
	// Spec is the cron expression as written in the config, for reporting.
	Spec string
	// Schedule is the parsed expression. A nil schedule means the job was
	// asked for explicitly rather than by the clock, and is always due.
	Schedule cron.Schedule
	// Level is the verification level, for a verify job.
	Level config.VerifyLevel
	// Tier is the label to record on a backup's manifest. Empty means the
	// default; it is only a label either way, since which tiers actually keep
	// a backup is computed at prune time (SPEC §7).
	Tier string
}

// Key identifies a job for bookkeeping.
func (j Job) Key() string { return j.Target + "/" + string(j.Kind) }

// String renders the job for a log line.
func (j Job) String() string { return j.Key() }

// Due reports whether this job should run at now, given when it last ran.
//
// A job that has never run is due: a target added to the config today should
// back up today rather than at its next cron minute, which for a monthly
// schedule could be four weeks away.
func (j Job) Due(last, now time.Time) bool {
	if j.Schedule == nil {
		return true
	}
	if last.IsZero() {
		return true
	}
	return !j.Schedule.Next(last).After(now)
}

// Next is when this job runs after t.
func (j Job) Next(t time.Time) time.Time {
	if j.Schedule == nil {
		return time.Time{}
	}
	return j.Schedule.Next(t)
}

// Jobs builds every scheduled job the config declares.
//
// A target with no schedule contributes nothing: it is a target somebody runs
// by hand, and inventing a cadence for it would start backing up a database on
// a timetable nobody asked for.
func Jobs(cfg *config.Config) ([]Job, error) {
	var jobs []Job

	for i := range cfg.Targets {
		target := &cfg.Targets[i]

		if target.Schedule != "" {
			schedule, err := parser.Parse(target.Schedule)
			if err != nil {
				return nil, fmt.Errorf("target %q has an invalid schedule %q: %w", target.Name, target.Schedule, err)
			}
			jobs = append(jobs, Job{
				Target:   target.Name,
				Kind:     KindBackup,
				Spec:     target.Schedule,
				Schedule: schedule,
			})
		}

		if target.Verify == nil || target.Verify.Schedule == "" {
			continue
		}

		schedule, err := parser.Parse(target.Verify.Schedule)
		if err != nil {
			return nil, fmt.Errorf("target %q has an invalid verify schedule %q: %w",
				target.Name, target.Verify.Schedule, err)
		}
		jobs = append(jobs, Job{
			Target:   target.Name,
			Kind:     KindVerify,
			Spec:     target.Verify.Schedule,
			Schedule: schedule,
			Level:    target.Verify.Level,
		})
	}

	return jobs, nil
}

// Manual returns a job that is due now, whatever the clock says. It is what
// `vaultd backup <target>` and a UI's "back up now" button run.
func Manual(target string, kind Kind) Job {
	return Job{Target: target, Kind: kind, Spec: "manual"}
}
