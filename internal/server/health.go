package server

import (
	"time"

	"github.com/robfig/cron/v3"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/manifest"
)

// cronParser accepts the same dialect the config validates and the scheduler
// runs, so the interval this file estimates is the one that will actually fire.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Health is the traffic light on the overview grid (SPEC §13).
type Health string

const (
	HealthGreen Health = "green"
	HealthAmber Health = "amber"
	HealthRed   Health = "red"
	// HealthUnknown is a target that has never run. It is not green — nothing
	// has proved it works — and not red either, because a config committed an
	// hour ago has not failed at anything yet.
	HealthUnknown Health = "unknown"
)

// defaultMaxAge is the staleness threshold for a target that declares neither
// a max_age assertion nor a schedule. A backup nobody has taken for more than
// a day is worth a red light whatever the config forgot to say.
const defaultMaxAge = 26 * time.Hour

// staleFactor turns a schedule into a red threshold: a daily backup is stale
// once it has missed two of its own slots, which is late enough not to alarm
// on a run that started ten minutes behind.
const staleFactor = 2

// Assessment is why a target has the colour it has. The reason travels with
// the colour because a red light nobody can explain gets ignored.
type Assessment struct {
	Health Health `json:"health"`
	Reason string `json:"reason"`
	// MaxAge is the threshold that was applied, so the UI can say what "old"
	// means for this target rather than leaving it a mystery.
	MaxAge time.Duration `json:"-"`
}

// Assess grades one target from what its index says.
//
// The rules are explicit on purpose (SPEC §13): a backup older than max_age is
// red, a failed most-recent run is red, and everything soft — a verification
// that failed, a backup nothing has verified — is amber.
func Assess(target *config.Target, latest, latestSuccess *manifest.Entry, now time.Time) Assessment {
	maxAge := MaxAgeOf(target)

	switch {
	case latestSuccess == nil && latest == nil:
		return Assessment{Health: HealthUnknown, Reason: "no run has been recorded yet", MaxAge: maxAge}

	case latestSuccess == nil:
		return Assessment{Health: HealthRed, Reason: "every recorded run failed", MaxAge: maxAge}

	case latest != nil && !latest.Succeeded():
		return Assessment{
			Health: HealthRed,
			Reason: "the most recent run failed during " + latest.Phase,
			MaxAge: maxAge,
		}
	}

	age := now.Sub(latestSuccess.FinishedAt)
	if age > maxAge {
		return Assessment{
			Health: HealthRed,
			Reason: "the newest backup is " + age.Round(time.Minute).String() + " old, past the " + maxAge.String() + " limit",
			MaxAge: maxAge,
		}
	}

	if latestSuccess.VerifyOK != nil && !*latestSuccess.VerifyOK {
		return Assessment{
			Health: HealthAmber,
			Reason: "the newest backup failed " + latestSuccess.VerifyLevel + " verification",
			MaxAge: maxAge,
		}
	}

	if target.Verify != nil && latestSuccess.VerifiedAt == nil {
		return Assessment{
			Health: HealthAmber,
			Reason: "the newest backup has not been verified yet",
			MaxAge: maxAge,
		}
	}

	return Assessment{Health: HealthGreen, Reason: "backed up " + age.Round(time.Minute).String() + " ago", MaxAge: maxAge}
}

// MaxAgeOf is how old this target's newest backup may get before it is red.
//
// A max_age assertion is the operator saying it outright, so it wins. Failing
// that the schedule is the best available statement of intent: a target that
// backs up every six hours should not wait a day to go red.
func MaxAgeOf(target *config.Target) time.Duration {
	if target.Verify != nil {
		for _, assertion := range target.Verify.Assertions {
			if assertion.Type == config.AssertMaxAge && assertion.Value != nil {
				return assertion.Value.Duration()
			}
		}
	}

	if interval := ScheduleInterval(target.Schedule); interval > 0 {
		return interval * staleFactor
	}
	return defaultMaxAge
}

// ScheduleInterval estimates the gap between two runs of a cron expression by
// asking it for its next two firings. It is an estimate — "0 3 * * 0" is a
// week apart, "0 3 1 * *" varies with the month — and an estimate is all a
// staleness threshold needs.
func ScheduleInterval(spec string) time.Duration {
	if spec == "" {
		return 0
	}

	schedule, err := cronParser.Parse(spec)
	if err != nil {
		return 0
	}

	from := time.Now().UTC()
	first := schedule.Next(from)
	second := schedule.Next(first)
	if second.IsZero() || first.IsZero() {
		return 0
	}
	return second.Sub(first)
}
