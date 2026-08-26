package scheduler_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/scheduler"
)

func daily(t *testing.T) scheduler.Job {
	t.Helper()

	cfg := load(t, configYAML)
	jobs, err := scheduler.Jobs(cfg)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	return jobs[0]
}

// A target added to the config today should back up today. Waiting for the
// next cron minute would be four weeks for a monthly schedule.
func TestAJobThatNeverRanIsDue(t *testing.T) {
	t.Parallel()

	job := daily(t)
	assert.True(t, job.Due(time.Time{}, time.Now()))
}

func TestDueFollowsTheCronExpression(t *testing.T) {
	t.Parallel()

	job := daily(t) // 03:00 every day
	last := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	assert.False(t, job.Due(last, last.Add(time.Hour)), "an hour later is not tomorrow")
	assert.False(t, job.Due(last, last.Add(23*time.Hour)), "still short of the next slot")
	assert.True(t, job.Due(last, last.Add(24*time.Hour)), "the next 03:00 has arrived")
	assert.True(t, job.Due(last, last.Add(72*time.Hour)), "a daemon that was down for days is due")
}

// A job nobody scheduled is one somebody asked for, and it runs whatever the
// clock says.
func TestAManualJobIsAlwaysDue(t *testing.T) {
	t.Parallel()

	job := scheduler.Manual("prod-pg", scheduler.KindBackup)
	assert.True(t, job.Due(time.Now(), time.Now()))
}

// A target with no schedule contributes no job: inventing a cadence for it
// would start backing up a database on a timetable nobody asked for.
func TestATargetWithoutAScheduleIsNotScheduled(t *testing.T) {
	t.Parallel()

	cfg := load(t, configYAML)
	cfg.Targets[0].Schedule = ""

	jobs, err := scheduler.Jobs(cfg)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestVerifyGetsItsOwnJob(t *testing.T) {
	t.Parallel()

	cfg := load(t, configYAML)
	cfg.Targets[0].Verify = verifySpec()

	jobs, err := scheduler.Jobs(cfg)
	require.NoError(t, err)
	require.Len(t, jobs, 2)

	assert.Equal(t, scheduler.KindBackup, jobs[0].Kind)
	assert.Equal(t, scheduler.KindVerify, jobs[1].Kind)
	assert.Equal(t, "prod-pg/verify", jobs[1].Key())
}

func TestAnInvalidScheduleIsRefused(t *testing.T) {
	t.Parallel()

	cfg := load(t, configYAML)
	cfg.Targets[0].Schedule = "every tuesday-ish"

	_, err := scheduler.Jobs(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid schedule")
}
