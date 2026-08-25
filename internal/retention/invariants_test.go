package retention_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/retention"
)

// The invariants in SPEC §7 are the difference between a retention policy and
// a data-loss bug. Each one gets a test that would fail loudly if the rule
// were dropped.

// Invariant 1: never leave fewer than min_keep backups.
func TestMinKeepFloorPromotesDoomedBackups(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 3}

	plan := policy.Plan(retention.Input{Backups: daily(t, "2026-08-01T03:00:00Z", 5)})

	require.Len(t, plan.Keep, 3, "the floor must hold even when the tiers keep one")
	assert.Equal(t, []string{
		"2026-08-03T03:00:00Z", "2026-08-04T03:00:00Z", "2026-08-05T03:00:00Z",
	}, ids(plan.Keep), "the floor keeps the newest of the doomed")

	for _, decision := range plan.Keep {
		if len(decision.Tiers) == 0 {
			assert.Contains(t, decision.Why(), "min_keep floor of 3")
		}
	}
}

func TestMinKeepAboveTheBackupCountKeepsEverything(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 10}

	plan := policy.Plan(retention.Input{Backups: daily(t, "2026-08-01T03:00:00Z", 4)})

	assert.Empty(t, plan.Delete)
	assert.Len(t, plan.Keep, 4)
}

// Invariant 2: the most recent verified backup is never deleted. It is the
// only one anybody has evidence restores.
func TestTheMostRecentVerifiedBackupSurvives(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1}

	all := daily(t, "2026-08-01T03:00:00Z", 6)
	all[1].Verified = true // 2026-08-02, far outside the daily window

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Contains(t, ids(plan.Keep), "2026-08-02T03:00:00Z")
	assert.NotContains(t, ids(plan.Delete), "2026-08-02T03:00:00Z")

	for _, decision := range plan.Keep {
		if decision.Backup.ID == "2026-08-02T03:00:00Z" {
			assert.Equal(t, "most recent verified backup", decision.Why())
		}
	}
}

func TestOnlyTheNewestVerifiedBackupIsProtected(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 1}

	all := daily(t, "2026-08-01T03:00:00Z", 6)
	all[0].Verified = true // an older verified backup
	all[2].Verified = true // the newest verified one

	plan := policy.Plan(retention.Input{Backups: all})

	assert.Contains(t, ids(plan.Keep), "2026-08-03T03:00:00Z")
	assert.Contains(t, ids(plan.Delete), "2026-08-01T03:00:00Z",
		"an older verified backup has no special claim")
}

// Invariant 3: a failed run freezes retention. A backup that just broke is
// exactly when the old ones matter.
func TestAFailedRunFreezesRetention(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 1}, MinKeep: 1}

	plan := policy.Plan(retention.Input{
		Backups:       daily(t, "2026-08-01T03:00:00Z", 5),
		LastRunFailed: true,
	})

	assert.Empty(t, plan.Delete)
	assert.Len(t, plan.Keep, 5)
	assert.Contains(t, plan.Blocked, "the most recent run of this target failed")
}

func TestAFailedRunWithNothingToDeleteIsNotReportedAsBlocked(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 30}, MinKeep: 1}

	plan := policy.Plan(retention.Input{
		Backups:       daily(t, "2026-08-01T03:00:00Z", 3),
		LastRunFailed: true,
	})

	assert.Empty(t, plan.Blocked, "nothing was going to be deleted anyway")
	assert.Len(t, plan.Keep, 3)
}

// A policy with no tiers keeps everything. The config treats an absent
// retention block as "keep forever", and the floor must not turn that into
// "delete all but min_keep".
func TestAnEmptyPolicyKeepsEverything(t *testing.T) {
	policy := retention.Policy{MinKeep: 1}

	plan := policy.Plan(retention.Input{Backups: daily(t, "2026-08-01T03:00:00Z", 20)})

	assert.Empty(t, plan.Delete)
	require.Len(t, plan.Keep, 20)
	assert.Contains(t, plan.Keep[0].Why(), "no retention policy")
}

func TestNoBackupsIsNotAnError(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 7}, MinKeep: 3}

	plan := policy.Plan(retention.Input{})

	assert.Empty(t, plan.Keep)
	assert.Empty(t, plan.Delete)
	assert.Empty(t, plan.Blocked)
}

func TestPlanReportsWhatItWouldFree(t *testing.T) {
	policy := retention.Policy{Daily: retention.Rule{Keep: 2}, MinKeep: 1}

	all := daily(t, "2026-08-01T03:00:00Z", 5)
	for i := range all {
		all[i].Bytes = 1 << 20
		all[i].Keys = []string{all[i].ID + ".dump", all[i].ID + ".manifest.json"}
	}

	plan := policy.Plan(retention.Input{Backups: all})

	require.Len(t, plan.Delete, 3)
	assert.Equal(t, int64(3<<20), plan.Bytes())
	assert.Len(t, plan.Keys(), 6, "every object of every deleted backup")
	assert.Len(t, plan.KeptIDs(), 2)
}

// A plan must never delete every backup, whatever the arithmetic says.
func TestPlanNeverDeletesEverything(t *testing.T) {
	policies := []retention.Policy{
		{Daily: retention.Rule{Keep: 1}, MinKeep: 1},
		{Hourly: retention.Rule{Keep: 1}, MinKeep: 1},
		{Weekly: retention.WeekRule{Keep: 1, On: time.Sunday}, MinKeep: 1},
		{Monthly: retention.MonthRule{Keep: 1, On: 1}, MinKeep: 1},
		{Yearly: retention.Rule{Keep: 1}, MinKeep: 1},
	}

	all := daily(t, "2026-08-01T03:00:00Z", 40)

	for _, policy := range policies {
		plan := policy.Plan(retention.Input{Backups: all})

		assert.NotEmpty(t, plan.Keep, "a plan that keeps nothing is a data-loss bug")
		assert.Len(t, plan.Keep, len(all)-len(plan.Delete))
	}
}
