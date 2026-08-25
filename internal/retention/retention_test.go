package retention_test

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/retention"
)

// Backups are named by their timestamp: a failing test then says which backup
// was kept, not which index.
func at(t *testing.T, stamp string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, stamp)
	require.NoError(t, err, "bad timestamp in the test itself")
	return parsed
}

func backups(t *testing.T, stamps ...string) []retention.Backup {
	t.Helper()

	out := make([]retention.Backup, 0, len(stamps))
	for _, stamp := range stamps {
		out = append(out, retention.Backup{ID: stamp, At: at(t, stamp), Bytes: 1000, Keys: []string{stamp + ".dump"}})
	}
	return out
}

func ids(decisions []retention.Decision) []string {
	out := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		out = append(out, decision.Backup.ID)
	}
	sort.Strings(out)
	return out
}

// daily builds a run of one backup per day at 03:00Z.
func daily(t *testing.T, from string, days int) []retention.Backup {
	t.Helper()

	start := at(t, from)
	stamps := make([]string, 0, days)
	for i := range days {
		stamps = append(stamps, start.AddDate(0, 0, i).Format(time.RFC3339))
	}
	return backups(t, stamps...)
}

func TestDailyKeepsTheMostRecentDays(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 7}, MinKeep: 1}

	plan := policy.Plan(retention.Input{Backups: daily(t, "2026-08-01T03:00:00Z", 10)})

	require.Len(t, plan.Keep, 7)
	require.Len(t, plan.Delete, 3)
	assert.Equal(t, []string{
		"2026-08-01T03:00:00Z", "2026-08-02T03:00:00Z", "2026-08-03T03:00:00Z",
	}, ids(plan.Delete), "the three oldest days should go")
}

// TestOneBackupSatisfiesSeveralTiers is the promotion the SPEC describes: the
// same object can be the daily, the weekly and the monthly backup at once.
func TestOneBackupSatisfiesSeveralTiers(t *testing.T) {
	policy := retention.Policy{
		Daily:   retention.Rule{Keep: 2},
		Weekly:  retention.WeekRule{Keep: 2, On: time.Sunday},
		Monthly: retention.MonthRule{Keep: 2, On: 1},
		MinKeep: 1,
	}

	// 2026-11-01 is a Sunday and the first of the month.
	all := backups(t,
		"2026-10-25T03:00:00Z", // Sunday
		"2026-11-01T03:00:00Z", // Sunday and the 1st
		"2026-11-02T03:00:00Z",
		"2026-11-03T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	var promoted retention.Decision
	for _, decision := range plan.Keep {
		if decision.Backup.ID == "2026-11-01T03:00:00Z" {
			promoted = decision
		}
	}
	require.NotEmpty(t, promoted.Tiers, "the Sunday-and-first backup should be kept")
	assert.ElementsMatch(t, []retention.Tier{retention.TierWeekly, retention.TierMonthly}, promoted.Tiers)
	assert.Equal(t, "weekly+monthly", promoted.Why())
}

func TestWeeklyPromotesTheConfiguredWeekday(t *testing.T) {
	policy := retention.Policy{Weekly: retention.WeekRule{Keep: 1, On: time.Sunday}, MinKeep: 1}

	// One ISO week, Monday through Sunday, with the Sunday backup taken first
	// in wall-clock order of the week but last in the week's bucket.
	all := backups(t,
		"2026-08-17T03:00:00Z", // Monday
		"2026-08-19T03:00:00Z", // Wednesday
		"2026-08-23T03:00:00Z", // Sunday, the last day of the ISO week
	)

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Keep, 1)
	assert.Equal(t, "2026-08-23T03:00:00Z", plan.Keep[0].Backup.ID)
}

// TestWeeklyFallsBackWhenTheDayIsMissing keeps an approximate weekly rather
// than keeping nothing for that week.
func TestWeeklyFallsBackWhenTheDayIsMissing(t *testing.T) {
	policy := retention.Policy{Weekly: retention.WeekRule{Keep: 1, On: time.Sunday}, MinKeep: 1}

	all := backups(t,
		"2026-08-17T03:00:00Z", // Monday
		"2026-08-19T03:00:00Z", // Wednesday, the newest of a Sunday-less week
	)

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Keep, 1)
	assert.Equal(t, "2026-08-19T03:00:00Z", plan.Keep[0].Backup.ID)
}

func TestMonthlyPromotesTheConfiguredDay(t *testing.T) {
	policy := retention.Policy{Monthly: retention.MonthRule{Keep: 2, On: 1}, MinKeep: 1}

	all := backups(t,
		"2026-07-01T03:00:00Z",
		"2026-07-15T03:00:00Z",
		"2026-08-01T03:00:00Z",
		"2026-08-24T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Equal(t, []string{"2026-07-01T03:00:00Z", "2026-08-01T03:00:00Z"}, ids(plan.Keep))
}

func TestHourlyBuckets(t *testing.T) {
	policy := retention.Policy{Hourly: retention.Rule{Keep: 3}, MinKeep: 1}

	all := backups(t,
		"2026-08-24T01:00:00Z",
		"2026-08-24T02:00:00Z",
		"2026-08-24T03:00:00Z",
		"2026-08-24T03:30:00Z", // same hour as the one above
		"2026-08-24T04:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	// Three hourly periods survive, and within the shared hour the newer one.
	assert.Equal(t, []string{
		"2026-08-24T02:00:00Z", "2026-08-24T03:30:00Z", "2026-08-24T04:00:00Z",
	}, ids(plan.Keep))
}

func TestYearlyBuckets(t *testing.T) {
	policy := retention.Policy{Yearly: retention.Rule{Keep: 2}, MinKeep: 1}

	all := backups(t,
		"2024-06-01T03:00:00Z",
		"2025-06-01T03:00:00Z",
		"2025-12-31T03:00:00Z",
		"2026-01-01T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Equal(t, []string{"2025-12-31T03:00:00Z", "2026-01-01T03:00:00Z"}, ids(plan.Keep))
}

// TestGapsDoNotConsumePeriods: `daily: {keep: 7}` means the seven most recent
// days that have a backup, not the last seven calendar days. A week of
// downtime must not silently expire everything that came before it.
func TestGapsDoNotConsumePeriods(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 3}, MinKeep: 1}

	all := backups(t,
		"2026-01-01T03:00:00Z",
		"2026-04-01T03:00:00Z",
		"2026-08-01T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all, Now: at(t, "2026-08-24T03:00:00Z")})

	assert.Empty(t, plan.Delete)
	assert.Len(t, plan.Keep, 3)
}

func TestSeveralBackupsInOneDayKeepTheNewest(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 1}

	all := backups(t,
		"2026-08-24T03:00:00Z",
		"2026-08-24T15:00:00Z",
		"2026-08-24T21:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Keep, 1)
	assert.Equal(t, "2026-08-24T21:00:00Z", plan.Keep[0].Backup.ID)
}
