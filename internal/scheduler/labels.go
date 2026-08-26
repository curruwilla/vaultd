package scheduler

import (
	"errors"

	"github.com/curruwilla/vaultd/internal/backup"
	"github.com/curruwilla/vaultd/internal/retention"
)

// defaultTier is the label stamped on a scheduled backup's manifest.
//
// It is only a label. Which tiers actually keep a backup is computed from its
// timestamp every time retention runs (SPEC §7), so this cannot drift out of
// step with a policy that changes — and a backup taken on a Sunday is counted
// as the weekly one whatever this says.
const defaultTier = "daily"

// tierOf is the label a job stamps on its manifest.
func tierOf(job Job) string {
	if job.Tier != "" {
		return job.Tier
	}
	return defaultTier
}

// phaseOf is the phase a failed backup died in, for the failure counter. A
// failure that carries no phase is attributed to the dump: it is by far the
// most common, and an unlabelled counter is worse than a slightly wrong one.
func phaseOf(err error) string {
	var failure *backup.Error
	if errors.As(err, &failure) {
		return string(failure.Phase)
	}
	return string(backup.PhaseDump)
}

// retentionTiers is how many retained backups each tier accounts for.
func retentionTiers(plan retention.Plan) map[string]int { return retention.TierCounts(plan) }
