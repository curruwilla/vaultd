package retention_test

import (
	"testing"
	"time"
	_ "time/tzdata" // the zone database, so these tests run on a bare container too

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/retention"
)

// Calendars are where retention arithmetic goes wrong: a day is not always 24
// hours, a year is not always 365 days, and a month can hold five Sundays.
// Periods are therefore compared by their calendar components, never by
// elapsed time, and these tests are what holds that in place.

func location(t *testing.T, name string) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// TestSpringForward: 2026-03-08 is 23 hours long in New York. Two backups on
// either side of the missing hour still belong to the same day.
func TestSpringForwardKeepsOneDay(t *testing.T) {
	newYork := location(t, "America/New_York")
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 1, Location: newYork}

	all := backups(t,
		"2026-03-08T06:30:00Z", // 01:30 EST, before the jump
		"2026-03-08T07:30:00Z", // 03:30 EDT, after it
	)

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Keep, 1, "both backups fall on 2026-03-08 in New York")
	assert.Equal(t, "2026-03-08T07:30:00Z", plan.Keep[0].Backup.ID)
}

// TestFallBack: 2026-11-01 is 25 hours long in New York, and 01:30 happens
// twice. Both instants are the same local date, so they share a day bucket.
func TestFallBackKeepsOneDay(t *testing.T) {
	newYork := location(t, "America/New_York")
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 1, Location: newYork}

	all := backups(t,
		"2026-11-01T05:30:00Z", // 01:30 EDT
		"2026-11-01T06:30:00Z", // 01:30 again, now EST
	)

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Keep, 1)
	assert.Equal(t, "2026-11-01T06:30:00Z", plan.Keep[0].Backup.ID, "the later instant wins the day")
}

// TestDSTDoesNotShiftPeriodCount checks the count of days across a transition:
// arithmetic on 24-hour multiples would be off by one here.
func TestDSTDoesNotShiftPeriodCount(t *testing.T) {
	newYork := location(t, "America/New_York")
	policy := retention.Policy{Daily: retention.Rule{Keep: 3}, MinKeep: 1, Location: newYork}

	all := backups(t,
		"2026-03-06T08:00:00Z", // Mar 6, 03:00 EST
		"2026-03-07T08:00:00Z", // Mar 7, 03:00 EST
		"2026-03-08T07:00:00Z", // Mar 8, 03:00 EDT — the short day
		"2026-03-09T07:00:00Z", // Mar 9, 03:00 EDT
	)

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Equal(t, []string{
		"2026-03-07T08:00:00Z", "2026-03-08T07:00:00Z", "2026-03-09T07:00:00Z",
	}, ids(plan.Keep))
	assert.Equal(t, []string{"2026-03-06T08:00:00Z"}, ids(plan.Delete))
}

// TestLeapDay: 2028 is a leap year, and 29 February is its own day.
func TestLeapDayIsItsOwnPeriod(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 3}, MinKeep: 1}

	all := backups(t,
		"2028-02-27T03:00:00Z",
		"2028-02-28T03:00:00Z",
		"2028-02-29T03:00:00Z",
		"2028-03-01T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Equal(t, []string{
		"2028-02-28T03:00:00Z", "2028-02-29T03:00:00Z", "2028-03-01T03:00:00Z",
	}, ids(plan.Keep))
}

func TestLeapYearMonthlyAndYearlyBoundaries(t *testing.T) {
	policy := retention.Policy{
		Monthly: retention.MonthRule{Keep: 2, On: 1},
		Yearly:  retention.Rule{Keep: 1},
		MinKeep: 1,
	}

	all := backups(t,
		"2028-01-01T03:00:00Z",
		"2028-02-01T03:00:00Z",
		"2028-02-29T03:00:00Z",
		"2028-03-01T03:00:00Z",
	)

	plan := policy.Plan(retention.Input{Backups: all})

	// The two most recent months keep their first-of-month backup; the year
	// keeps its newest.
	assert.Contains(t, ids(plan.Keep), "2028-02-01T03:00:00Z")
	assert.Contains(t, ids(plan.Keep), "2028-03-01T03:00:00Z")
	assert.Contains(t, ids(plan.Delete), "2028-01-01T03:00:00Z")
}

// TestMonthWithFiveSundays: August 2026 has Sundays on the 2nd, 9th, 16th,
// 23rd and 30th. `weekly: {keep: 4}` keeps four of them, not five and not
// three, and the one it drops is the oldest.
func TestMonthWithFiveSundays(t *testing.T) {
	policy := retention.Policy{Weekly: retention.WeekRule{Keep: 4, On: time.Sunday}, MinKeep: 1}

	sundays := backups(t,
		"2026-08-02T03:00:00Z",
		"2026-08-09T03:00:00Z",
		"2026-08-16T03:00:00Z",
		"2026-08-23T03:00:00Z",
		"2026-08-30T03:00:00Z",
	)
	for _, backup := range sundays {
		require.Equal(t, time.Sunday, backup.At.Weekday(), "the test's own calendar is wrong")
	}

	plan := policy.Plan(retention.Input{Backups: sundays})

	assert.Equal(t, []string{
		"2026-08-09T03:00:00Z", "2026-08-16T03:00:00Z", "2026-08-23T03:00:00Z", "2026-08-30T03:00:00Z",
	}, ids(plan.Keep))
	assert.Equal(t, []string{"2026-08-02T03:00:00Z"}, ids(plan.Delete))
}

// TestISOWeeksAcrossTheYearBoundary: 1 January 2027 falls in ISO week 53 of
// 2026. Formatting the year separately from the week number would collapse it
// into week 53 of 2027 and lose a weekly backup.
func TestISOWeeksAcrossTheYearBoundary(t *testing.T) {
	policy := retention.Policy{Weekly: retention.WeekRule{Keep: 3, On: time.Sunday}, MinKeep: 1}

	all := backups(t,
		"2026-12-20T03:00:00Z", // Sunday, week 51
		"2026-12-27T03:00:00Z", // Sunday, week 52
		"2027-01-01T03:00:00Z", // Friday, ISO week 53 of 2026
		"2027-01-03T03:00:00Z", // Sunday, ISO week 53 of 2026
	)

	plan := policy.Plan(retention.Input{Backups: all})

	// Three ISO weeks survive; the shared week keeps its Sunday.
	assert.Equal(t, []string{
		"2026-12-20T03:00:00Z", "2026-12-27T03:00:00Z", "2027-01-03T03:00:00Z",
	}, ids(plan.Keep))
	assert.Equal(t, []string{"2027-01-01T03:00:00Z"}, ids(plan.Delete))
}

// TestLocationDecidesTheDay: the same two instants are one day or two,
// depending on where the day is measured.
func TestLocationDecidesTheDay(t *testing.T) {
	all := backups(t,
		"2026-08-24T23:30:00Z", // 20:30 in São Paulo, still the 24th
		"2026-08-25T02:30:00Z", // 23:30 in São Paulo, still the 24th
	)

	// Two daily slots: enough for both backups if they fall on different days.
	inUTC := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1}
	plan := inUTC.Plan(retention.Input{Backups: all})
	assert.Len(t, plan.Keep, 2, "in UTC these are two different days")

	inSaoPaulo := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1, Location: location(t, "America/Sao_Paulo")}
	plan = inSaoPaulo.Plan(retention.Input{Backups: all})
	assert.Len(t, plan.Keep, 1, "in São Paulo they are the same day, so one slot covers both")
}

// TestUTCIsTheDefault documents the choice: object keys are stamped in UTC, so
// periods are measured there too unless a policy says otherwise.
func TestUTCIsTheDefault(t *testing.T) {
	all := backups(t, "2026-08-24T23:30:00Z", "2026-08-25T02:30:00Z")

	unset := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1}
	explicit := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1, Location: time.UTC}

	assert.Equal(t, ids(explicit.Plan(retention.Input{Backups: all}).Keep),
		ids(unset.Plan(retention.Input{Backups: all}).Keep))
}
