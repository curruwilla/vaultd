package cli

import (
	"context"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/retention"
)

// orphanGrace is how old an unreferenced object must be before prune will call
// it an orphan. A backup that is mid-upload has an object and no manifest yet,
// and deleting it would break the run that is writing it.
const orphanGrace = 24 * time.Hour

func newPruneCommand(g *globals) *cobra.Command {
	var (
		apply   bool
		orphans bool
		grace   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "prune <target>",
		Short: "Apply the retention policy (dry run unless --apply)",
		Long: "prune works out which backups the retention policy still keeps and deletes the\n" +
			"rest. It reports what it would do and changes nothing unless --apply is given.\n\n" +
			"Deletion is refused outright when the most recent run failed, when it would\n" +
			"leave fewer than min_keep backups, or when it would remove the most recent\n" +
			"verified backup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, target, err := g.targetFromConfig(args[0])
			if err != nil {
				return err
			}

			application := app.New(cfg, g.logger)
			ctx := cmd.Context()

			idx, err := application.Index(ctx, target)
			if err != nil {
				return err
			}
			entries, cached, err := idx.Entries(ctx)
			if err != nil {
				return err
			}
			if !cached {
				// Failed runs live only in the index, and one of the
				// invariants depends on seeing them.
				fmt.Fprintf(g.err, "warning: %s has no index; it was read from the manifests, "+
					"so failed runs are invisible to this prune. Run `vaultd reindex %s`.\n",
					target.Name, target.Name)
			}

			plan := application.Retention(target).Plan(retention.Input{
				Backups:       backupsOf(entries),
				Now:           time.Now().UTC(),
				LastRunFailed: lastRunFailed(entries),
			})

			g.printPlan2(target.Name, plan)

			var strays []core.ObjectInfo
			if orphans {
				strays, err = findOrphans(ctx, application, target, entries, grace)
				if err != nil {
					return err
				}
				g.printOrphans(strays)
			}

			if !apply {
				g.printDryRun(plan, strays)
				return nil
			}
			return g.applyPrune(ctx, application, target, idx, plan, strays)
		},
	}

	cmd.Flags().BoolVar(&apply, "apply", false, "actually delete; without it prune only reports")
	cmd.Flags().BoolVar(&orphans, "orphans", false, "also consider objects that no manifest refers to")
	cmd.Flags().DurationVar(&grace, "orphan-grace", orphanGrace,
		"how old an unreferenced object must be before it counts as an orphan")

	return cmd
}

// backupsOf turns index entries into what retention reasons about.
func backupsOf(entries []manifest.Entry) []retention.Backup {
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

// lastRunFailed reports whether the most recent attempt — successful or not —
// ended in failure.
func lastRunFailed(entries []manifest.Entry) bool {
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

func (g *globals) printPlan2(target string, plan retention.Plan) {
	if len(plan.Keep) == 0 && len(plan.Delete) == 0 {
		fmt.Fprintf(g.out, "%s has no backups\n", target)
		return
	}

	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tFINISHED\tSIZE\tREASON\tID")

	rows := append(append([]row{}, rowsOf("keep", plan.Keep)...), rowsOf("delete", plan.Delete)...)
	sort.Slice(rows, func(a, b int) bool { return rows[a].at.After(rows[b].at) })

	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.action, r.at.Format("2006-01-02 15:04Z"), humanBytes(r.bytes), orDash(r.reason), r.id)
	}
	_ = w.Flush()

	if plan.Blocked != "" {
		fmt.Fprintf(g.err, "\nblocked: %s\n", plan.Blocked)
	}
}

type row struct {
	action string
	at     time.Time
	bytes  int64
	reason string
	id     string
}

func rowsOf(action string, decisions []retention.Decision) []row {
	out := make([]row, 0, len(decisions))
	for _, decision := range decisions {
		out = append(out, row{
			action: action,
			at:     decision.Backup.At,
			bytes:  decision.Backup.Bytes,
			reason: decision.Why(),
			id:     decision.Backup.ID,
		})
	}
	return out
}

func (g *globals) printOrphans(strays []core.ObjectInfo) {
	if len(strays) == 0 {
		fmt.Fprintln(g.out, "\nno orphaned objects")
		return
	}

	var total int64
	w := tabwriter.NewWriter(g.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "\nORPHAN\tSIZE\tLAST MODIFIED\n")
	for _, object := range strays {
		total += object.Bytes
		fmt.Fprintf(w, "%s\t%s\t%s\n", object.Key, humanBytes(object.Bytes), object.LastModified.Format("2006-01-02 15:04Z"))
	}
	_ = w.Flush()
	fmt.Fprintf(g.out, "%s in %s with no manifest\n", plural(len(strays), "object", "objects"), humanBytes(total))
}

func (g *globals) printDryRun(plan retention.Plan, strays []core.ObjectInfo) {
	if len(plan.Delete) == 0 && len(strays) == 0 {
		fmt.Fprintln(g.out, "\nnothing to delete")
		return
	}

	fmt.Fprintf(g.out, "\ndry run: %s (%s) would be deleted",
		plural(len(plan.Delete), "backup", "backups"), humanBytes(plan.Bytes()))
	if len(strays) > 0 {
		fmt.Fprintf(g.out, ", plus %s", plural(len(strays), "orphaned object", "orphaned objects"))
	}
	fmt.Fprintln(g.out, "; re-run with --apply to carry it out")
}

func (g *globals) applyPrune(
	ctx context.Context,
	application *app.App,
	target *config.Target,
	idx *index.Store,
	plan retention.Plan,
	strays []core.ObjectInfo,
) error {
	keys := plan.Keys()
	for _, object := range strays {
		keys = append(keys, object.Key)
	}
	if len(keys) == 0 {
		fmt.Fprintln(g.out, "\nnothing to delete")
		return nil
	}

	store, err := application.Store(ctx, target.Destination)
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, keys); err != nil {
		return err
	}

	// The index is updated only after the objects are gone: an index that
	// still lists a deleted backup is a stale cache, while one that hides a
	// backup which is still there hides a restore.
	if err := idx.Remove(ctx, deletedIDs(plan), oldestKept(plan)); err != nil {
		return fmt.Errorf("the objects were deleted but the index was not updated; run `vaultd reindex %s`: %w",
			target.Name, err)
	}

	fmt.Fprintf(g.out, "\ndeleted %s and %s (%s freed)\n",
		plural(len(plan.Delete), "backup", "backups"),
		plural(len(keys), "object", "objects"),
		humanBytes(plan.Bytes()))
	return nil
}

// deletedIDs are the backups the plan removes.
func deletedIDs(plan retention.Plan) []string {
	ids := make([]string, 0, len(plan.Delete))
	for _, decision := range plan.Delete {
		ids = append(ids, decision.Backup.ID)
	}
	return ids
}

// oldestKept is where the retained window starts; failure records older than
// it describe a period nothing survives from.
func oldestKept(plan retention.Plan) time.Time {
	var oldest time.Time
	for _, decision := range plan.Keep {
		if oldest.IsZero() || decision.Backup.At.Before(oldest) {
			oldest = decision.Backup.At
		}
	}
	return oldest
}

// findOrphans lists objects under the target's prefix that no index entry
// claims. They are the residue of an interrupted run or of a manual deletion,
// and they are only ever removed on an explicit --orphans (SPEC §7,
// invariant 4).
func findOrphans(
	ctx context.Context,
	application *app.App,
	target *config.Target,
	entries []manifest.Entry,
	grace time.Duration,
) ([]core.ObjectInfo, error) {
	store, err := application.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	layout, err := application.Layout(target)
	if err != nil {
		return nil, err
	}

	claimed := map[string]bool{layout.Index(): true, layout.Lock(): true}
	for _, entry := range entries {
		for _, key := range entry.Keys() {
			claimed[key] = true
		}
	}

	cutoff := time.Now().UTC().Add(-grace)

	var strays []core.ObjectInfo
	for object, err := range store.List(ctx, layout.TargetPrefix()) {
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", target.Name, err)
		}
		if claimed[object.Key] {
			continue
		}
		// A backup that is still uploading has an object and no manifest yet.
		if object.LastModified.After(cutoff) {
			continue
		}
		strays = append(strays, object)
	}

	sort.Slice(strays, func(a, b int) bool { return strays[a].Key < strays[b].Key })
	return strays, nil
}
