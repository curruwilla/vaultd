// Package retention decides which backups a target keeps and which prune may
// delete (SPEC §7).
//
// The policy is grandfather-father-son: a backup is kept if it is the
// representative of a period some tier still retains, and one backup can
// represent several tiers at once — the Sunday backup is usually the daily,
// the weekly and, on the first of the month, the monthly one too.
//
// Classification is computed from the timestamps every time rather than
// stamped onto a backup when it is taken. That way a policy change applies to
// the backups that already exist, and there is no recorded tier to drift out
// of step with the policy that is actually configured.
package retention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Tier is one rung of the policy.
type Tier string

const (
	TierHourly  Tier = "hourly"
	TierDaily   Tier = "daily"
	TierWeekly  Tier = "weekly"
	TierMonthly Tier = "monthly"
	TierYearly  Tier = "yearly"
)

// Rule keeps the representatives of the Keep most recent periods that hold a
// backup. A gap does not consume a slot: two backups a month apart under
// `daily: {keep: 7}` are both kept, because there are only two days to keep.
type Rule struct {
	Keep int
}

// WeekRule promotes the backup taken on weekday On.
type WeekRule struct {
	Keep int
	On   time.Weekday
}

// MonthRule promotes the backup taken on day-of-month On.
type MonthRule struct {
	Keep int
	On   int
}

// Policy is a target's complete retention configuration.
type Policy struct {
	Hourly  Rule
	Daily   Rule
	Weekly  WeekRule
	Monthly MonthRule
	Yearly  Rule

	// MinKeep is the floor that overrides every rule above: prune never leaves
	// fewer than this many backups, whatever the tiers say.
	MinKeep int

	// Location decides where a day, a week and a month begin. It defaults to
	// UTC, which is also how object keys are stamped. Periods are compared by
	// their calendar components rather than by elapsed time, so a daylight
	// saving transition neither merges two days nor splits one.
	Location *time.Location
}

// Backup is the minimal view of a stored backup that retention reasons about.
type Backup struct {
	ID string
	// At is when the backup finished; it is what places it in a period.
	At       time.Time
	Verified bool
	Bytes    int64
	// Keys are the objects this backup owns, which is what prune deletes.
	Keys []string
}

// Decision is one backup and what happens to it.
type Decision struct {
	Backup Backup
	// Tiers are the rungs that keep this backup, in policy order.
	Tiers []Tier
	// Reason explains a decision no tier accounts for: the floor, the most
	// recent verified backup, a policy that retains everything.
	Reason string
}

// Kept reports whether this decision keeps the backup.
func (d Decision) Kept() bool { return len(d.Tiers) > 0 || d.Reason != "" }

// Why renders the decision for a human: the tiers, or the reason.
func (d Decision) Why() string {
	if len(d.Tiers) == 0 {
		return d.Reason
	}

	names := make([]string, 0, len(d.Tiers))
	for _, tier := range d.Tiers {
		names = append(names, string(tier))
	}
	out := join(names)
	if d.Reason != "" {
		out += ", " + d.Reason
	}
	return out
}

// Input is the state prune reasons about.
type Input struct {
	Backups []Backup
	Now     time.Time
	// LastRunFailed says the most recent attempt for this target did not
	// produce a backup. Nothing is deleted while that is true: a fresh backup
	// that is broken is exactly when the old ones matter most (SPEC §7,
	// invariant 3).
	LastRunFailed bool
}

// Plan is what prune would do.
type Plan struct {
	Keep   []Decision
	Delete []Decision
	// Blocked, when set, explains why the whole plan deletes nothing.
	Blocked string
}

// Bytes returns how much the deletions would free.
func (p Plan) Bytes() int64 {
	var total int64
	for _, decision := range p.Delete {
		total += decision.Backup.Bytes
	}
	return total
}

// Keys returns every object key the plan deletes, oldest backup first.
func (p Plan) Keys() []string {
	var keys []string
	for _, decision := range p.Delete {
		keys = append(keys, decision.Backup.Keys...)
	}
	return keys
}

// KeptIDs returns the ids the plan keeps.
func (p Plan) KeptIDs() []string {
	ids := make([]string, 0, len(p.Keep))
	for _, decision := range p.Keep {
		ids = append(ids, decision.Backup.ID)
	}
	return ids
}

// Plan works out which backups survive.
func (p Policy) Plan(in Input) Plan {
	location := p.location()

	backups := append([]Backup(nil), in.Backups...)
	// Newest first: every rule below is expressed as "the most recent N".
	sort.Slice(backups, func(a, b int) bool { return backups[a].At.After(backups[b].At) })

	if len(backups) == 0 {
		return Plan{}
	}

	// A policy that retains nothing by tier retains everything. This is the
	// config's documented default (no retention block means keep forever), and
	// the floor below must never turn it into "delete all but MinKeep".
	if p.empty() {
		return keepAll(backups, "no retention policy is configured, so nothing is ever deleted")
	}

	tiers := map[string][]Tier{}
	for _, tier := range p.rungs() {
		for _, id := range p.survivors(tier, backups, location) {
			tiers[id] = append(tiers[id], tier)
		}
	}

	reasons := map[string]string{}

	// Invariant 2: the most recent verified backup is the one a restore would
	// actually be trusted to use, so it outlives any tier.
	if verified, ok := newestVerified(backups); ok && len(tiers[verified.ID]) == 0 {
		reasons[verified.ID] = "most recent verified backup"
	}

	// Invariant 1: the floor. Promote the newest otherwise-doomed backups
	// until enough survive.
	kept := countKept(backups, tiers, reasons)
	for _, backup := range backups {
		if kept >= p.MinKeep {
			break
		}
		if len(tiers[backup.ID]) == 0 && reasons[backup.ID] == "" {
			reasons[backup.ID] = fmt.Sprintf("min_keep floor of %d", p.MinKeep)
			kept++
		}
	}

	plan := Plan{}
	for _, backup := range backups {
		decision := Decision{Backup: backup, Tiers: tiers[backup.ID], Reason: reasons[backup.ID]}
		if decision.Kept() {
			plan.Keep = append(plan.Keep, decision)
		} else {
			plan.Delete = append(plan.Delete, decision)
		}
	}

	// Invariant 3: a failed run freezes retention entirely.
	if in.LastRunFailed && len(plan.Delete) > 0 {
		blocked := keepAll(backups, "the most recent run of this target failed")
		blocked.Blocked = "the most recent run of this target failed; retention is frozen until a backup succeeds"
		return blocked
	}

	return plan
}

// survivors returns the ids one tier keeps.
func (p Policy) survivors(tier Tier, newestFirst []Backup, location *time.Location) []string {
	keep := p.keepOf(tier)
	if keep <= 0 {
		return nil
	}

	var (
		order   []string
		buckets = map[string][]Backup{}
	)
	for _, backup := range newestFirst {
		key := bucketKey(backup.At, tier, location)
		if _, seen := buckets[key]; !seen {
			// newestFirst means periods are met newest first too.
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], backup)
	}

	if len(order) > keep {
		order = order[:keep]
	}

	ids := make([]string, 0, len(order))
	for _, key := range order {
		ids = append(ids, p.representative(tier, buckets[key], location).ID)
	}
	return ids
}

// representative picks which backup stands for a period.
//
// For weekly and monthly the policy names a day — `on: sunday`, `on: 1` — and
// that day's backup is the one promoted. When the named day has no backup (the
// server was down, the schedule changed), the period keeps its newest backup
// instead of keeping nothing: an approximate weekly is worth more than a gap.
func (p Policy) representative(tier Tier, bucket []Backup, location *time.Location) Backup {
	switch tier {
	case TierWeekly:
		for _, backup := range bucket {
			if backup.At.In(location).Weekday() == p.Weekly.On {
				return backup
			}
		}
	case TierMonthly:
		for _, backup := range bucket {
			if backup.At.In(location).Day() == p.Monthly.On {
				return backup
			}
		}
	}
	// bucket is newest first.
	return bucket[0]
}

func (p Policy) keepOf(tier Tier) int {
	switch tier {
	case TierHourly:
		return p.Hourly.Keep
	case TierDaily:
		return p.Daily.Keep
	case TierWeekly:
		return p.Weekly.Keep
	case TierMonthly:
		return p.Monthly.Keep
	case TierYearly:
		return p.Yearly.Keep
	default:
		return 0
	}
}

// rungs lists the tiers in policy order, coarsest last.
func (p Policy) rungs() []Tier {
	return []Tier{TierHourly, TierDaily, TierWeekly, TierMonthly, TierYearly}
}

func (p Policy) empty() bool {
	for _, tier := range p.rungs() {
		if p.keepOf(tier) > 0 {
			return false
		}
	}
	return true
}

func (p Policy) location() *time.Location {
	if p.Location == nil {
		return time.UTC
	}
	return p.Location
}

// bucketKey names the period a backup falls in. It formats calendar
// components rather than dividing elapsed time, so a 23- or 25-hour day
// crosses no boundary it should not.
func bucketKey(at time.Time, tier Tier, location *time.Location) string {
	local := at.In(location)

	switch tier {
	case TierHourly:
		return local.Format("2006-01-02T15")
	case TierDaily:
		return local.Format("2006-01-02")
	case TierWeekly:
		year, week := local.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	case TierMonthly:
		return local.Format("2006-01")
	case TierYearly:
		return local.Format("2006")
	default:
		return local.Format(time.RFC3339)
	}
}

func newestVerified(newestFirst []Backup) (Backup, bool) {
	for _, backup := range newestFirst {
		if backup.Verified {
			return backup, true
		}
	}
	return Backup{}, false
}

func countKept(backups []Backup, tiers map[string][]Tier, reasons map[string]string) int {
	kept := 0
	for _, backup := range backups {
		if len(tiers[backup.ID]) > 0 || reasons[backup.ID] != "" {
			kept++
		}
	}
	return kept
}

func keepAll(backups []Backup, reason string) Plan {
	plan := Plan{Keep: make([]Decision, 0, len(backups))}
	for _, backup := range backups {
		plan.Keep = append(plan.Keep, Decision{Backup: backup, Reason: reason})
	}
	return plan
}

func join(names []string) string { return strings.Join(names, "+") }
