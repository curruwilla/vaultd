// Package prune applies a target's retention policy to its bucket: work out
// what the policy still keeps, delete the rest, and put the index back in step
// (SPEC §7).
//
// The decision itself is retention.Policy's and stays pure. What lives here is
// the part that touches the world, in the order that matters: objects first,
// index second. A stale index that still lists a deleted backup is a nuisance
// somebody fixes with `vaultd reindex`; an index that hides a backup which is
// still in the bucket hides a restore.
package prune

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
	"github.com/curruwilla/vaultd/internal/retention"
)

// Runner applies one target's policy.
type Runner struct {
	Target string
	Store  core.Store
	Index  *index.Store
	Policy retention.Policy
	// Notify receives retention.pruned and retention.blocked.
	Notify core.Notifier
	Now    func() time.Time
	Log    *slog.Logger
}

// Plan reads the index and works out what the policy keeps. It writes nothing.
//
// The second return value says whether the entries came from the index or were
// reconstructed from the manifests. It matters: failed runs live only in the
// index, and one of the invariants — never prune while the most recent attempt
// failed — has nothing to read without them.
func (r *Runner) Plan(ctx context.Context) (retention.Plan, bool, error) {
	entries, cached, err := r.Index.Entries(ctx)
	if err != nil {
		return retention.Plan{}, false, err
	}

	plan := r.Policy.Plan(retention.Input{
		Backups:       Backups(entries),
		Now:           r.now(),
		LastRunFailed: LastRunFailed(entries),
	})
	return plan, cached, nil
}

// Apply carries a plan out. Extra keys — the orphans `--orphans` found — are
// deleted alongside the backups the policy expired.
//
// It returns how many objects were removed, which is more than the number of
// backups: each one owns its data object, its manifest and possibly its
// globals.
func (r *Runner) Apply(ctx context.Context, plan retention.Plan, extra []string) (int, error) {
	keys := append(plan.Keys(), extra...)
	if len(keys) == 0 {
		return 0, nil
	}

	if err := r.Store.Delete(ctx, keys); err != nil {
		return 0, err
	}

	if err := r.Index.Remove(ctx, DeletedIDs(plan), OldestKept(plan)); err != nil {
		return len(keys), fmt.Errorf(
			"the objects were deleted but the index was not updated; run `vaultd reindex %s`: %w", r.Target, err)
	}

	notify.Emit(ctx, r.Notify, r.Log, retention.PrunedEvent(r.Target, r.now(), plan, len(keys)))
	return len(keys), nil
}

// AnnounceBlocked reports a plan an invariant stopped. It is called on a run
// that meant to delete: the first night a block holds is the invariants doing
// their job, and the thirtieth is a bucket growing without bound.
func (r *Runner) AnnounceBlocked(ctx context.Context, plan retention.Plan) {
	if plan.Blocked == "" {
		return
	}
	notify.Emit(ctx, r.Notify, r.Log, retention.BlockedEvent(r.Target, r.now(), plan))
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

// Backups turns index entries into what retention reasons about. Failed runs
// are not backups and are left out; LastRunFailed is how they are accounted
// for instead.
func Backups(entries []manifest.Entry) []retention.Backup {
	out := make([]retention.Backup, 0, len(entries))
	for _, entry := range entries {
		if !entry.Succeeded() {
			continue
		}
		out = append(out, retention.Backup{
			ID:       entry.ID,
			At:       entry.FinishedAt,
			Verified: entry.Verified(),
			Bytes:    entry.Bytes,
			Keys:     entry.Keys(),
		})
	}
	return out
}

// LastRunFailed reports whether the most recent attempt — successful or not —
// ended in failure (SPEC §7, invariant 3).
func LastRunFailed(entries []manifest.Entry) bool {
	if len(entries) == 0 {
		return false
	}

	latest := entries[0]
	for _, entry := range entries[1:] {
		if entry.FinishedAt.After(latest.FinishedAt) {
			latest = entry
		}
	}
	return !latest.Succeeded()
}

// DeletedIDs are the backups a plan removes.
func DeletedIDs(plan retention.Plan) []string {
	ids := make([]string, 0, len(plan.Delete))
	for _, decision := range plan.Delete {
		ids = append(ids, decision.Backup.ID)
	}
	return ids
}

// OldestKept is where the retained window starts. Failure records older than
// it describe a period nothing survives from, so they go with the backups.
func OldestKept(plan retention.Plan) time.Time {
	var oldest time.Time
	for _, decision := range plan.Keep {
		if oldest.IsZero() || decision.Backup.At.Before(oldest) {
			oldest = decision.Backup.At
		}
	}
	return oldest
}
