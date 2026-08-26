package retention

import (
	"fmt"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

// PrunedEvent describes a prune that deleted something. A prune that deleted
// nothing sends nothing: a nightly policy with nothing to expire would
// otherwise post the same "0 backups deleted" every night, and a channel
// nobody reads is a channel that misses the one that mattered.
func PrunedEvent(target string, at time.Time, plan Plan, objects int) core.Notification {
	n := notify.Notification(core.EventRetentionPruned, at, target,
		fmt.Sprintf("%s pruned %s (%s freed), %s kept",
			target,
			plural(len(plan.Delete), "backup", "backups"),
			humanBytes(plan.Bytes()),
			plural(len(plan.Keep), "backup", "backups")))
	n.Details = map[string]any{
		"deleted":      len(plan.Delete),
		"kept":         len(plan.Keep),
		"objects":      objects,
		"bytes_freed":  plan.Bytes(),
		"deleted_ids":  deletedIDs(plan),
		"oldest_kept":  oldestKept(plan),
		"tier_summary": TierCounts(plan),
	}
	return n
}

// BlockedEvent announces a prune that an invariant stopped (SPEC §7).
//
// It is a warning rather than an information event because a block that stays
// blocked is how a bucket quietly grows without bound: the first night is
// correct behaviour, the thirtieth is an outage nobody looked at.
func BlockedEvent(target string, at time.Time, plan Plan) core.Notification {
	n := notify.Notification(core.EventRetentionBlocked, at, target,
		fmt.Sprintf("%s kept every backup: %s", target, plan.Blocked))
	n.Details = map[string]any{
		"reason": plan.Blocked,
		"kept":   len(plan.Keep),
	}
	return n
}

// TierCounts is how many retained backups each tier accounts for, which is
// what the vaultd_retention_objects gauge publishes. A backup promoted into
// several tiers counts in each: the question the number answers is "what is
// the weekly rung holding", not "how many objects exist".
func TierCounts(plan Plan) map[string]int {
	counts := map[string]int{}
	for _, decision := range plan.Keep {
		if len(decision.Tiers) == 0 {
			// Kept by an invariant rather than by a tier — the min_keep floor,
			// the most recent verified backup — which is worth its own bucket:
			// a target whose entire retention is "floor" has a policy that
			// does not match its schedule.
			counts["none"]++
			continue
		}
		for _, tier := range decision.Tiers {
			counts[string(tier)]++
		}
	}
	return counts
}

func deletedIDs(plan Plan) []string {
	ids := make([]string, 0, len(plan.Delete))
	for _, decision := range plan.Delete {
		ids = append(ids, decision.Backup.ID)
	}
	return ids
}

// oldestKept is the start of the retained window, rendered for a payload.
func oldestKept(plan Plan) string {
	var oldest time.Time
	for _, decision := range plan.Keep {
		if oldest.IsZero() || decision.Backup.At.Before(oldest) {
			oldest = decision.Backup.At
		}
	}
	if oldest.IsZero() {
		return ""
	}
	return oldest.UTC().Format(time.RFC3339)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
