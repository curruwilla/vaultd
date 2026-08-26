package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/lock"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/verify"
)

// Outcome says what one dispatch of a job actually did.
type Outcome string

const (
	// OutcomeRan means the work was done, whatever it found.
	OutcomeRan Outcome = "ran"
	// OutcomeLocked means somebody else is running this target right now.
	OutcomeLocked Outcome = "locked"
	// OutcomeNotDue means the index said the job had already run — another
	// replica got there first while this one was waiting for the lock.
	OutcomeNotDue Outcome = "not_due"
	// OutcomeNothing means there was nothing to do: a verify with no backup to
	// verify, a prune with nothing expired.
	OutcomeNothing Outcome = "nothing"
)

// Result is what a dispatch reports back to whoever asked for it.
type Result struct {
	Job      Job           `json:"job"`
	Outcome  Outcome       `json:"outcome"`
	Duration time.Duration `json:"-"`
	// Detail is the human sentence: who holds the lock, which backup was
	// verified, how much a prune freed.
	Detail string `json:"detail,omitempty"`
	// BackupID is set when a run produced or examined one.
	BackupID string `json:"backup_id,omitempty"`
	// Manifest is what a backup stored, for the caller that wants to print it.
	Manifest *manifest.Manifest `json:"-"`
	// Err is the failure, if the work itself failed. It is carried rather than
	// returned so a daemon can log one target's failure and keep running the
	// others.
	Err error `json:"-"`
}

// Recorder is the metrics surface a run publishes to.
//
// It is an interface declared here rather than the concrete *metrics.Metrics
// so that a one-shot `vaultd run`, which serves no /metrics endpoint, can pass
// nothing at all.
type Recorder interface {
	BackupSucceeded(target, engine string, d time.Duration, compressed, plain int64, at time.Time)
	BackupFailed(target, phase string)
	VerifySucceeded(target, level string, at time.Time)
	RetentionObjects(target string, byTier map[string]int)
	ScheduleMissed(target, kind string)
	RunStarted(kind string)
	RunFinished(kind string)
}

// Executor performs one job.
//
// It is the single execution path: `vaultd serve` calls it on a schedule,
// `vaultd run` calls it once for everything due, and the UI's "back up now"
// calls it with a manual job. They therefore share the lock, the due check and
// the bookkeeping, which is the point — a manual backup that ignored the lock
// would collide with the daemon's (SPEC §11).
type Executor struct {
	App     *app.App
	Log     *slog.Logger
	Metrics Recorder
	// Identities decrypt backups for structural and restore verification. The
	// private key is not something vaultd stores, so it is supplied to the
	// process that needs it (SPEC §15).
	Identities []age.Identity
	// Prune applies the retention policy after a successful backup, which is
	// what makes rotation automatic (objective O3).
	Prune bool
	// LockTTL is the lease length; zero means lock.DefaultTTL.
	LockTTL time.Duration
	Now     func() time.Time
}

// Run executes one job under the target's lock.
func (e *Executor) Run(ctx context.Context, job Job) Result {
	started := e.now()
	result := Result{Job: job}

	target, ok := e.App.Config().Target(job.Target)
	if !ok {
		result.Err = fmt.Errorf("target %q is not declared", job.Target)
		return e.finish(result, started)
	}

	held, err := e.acquire(ctx, target, job)
	if err != nil {
		var locked *lock.ErrLocked
		if errors.As(err, &locked) {
			// Somebody else has it. That is the lock doing its job, not a
			// failure: the other run will produce the backup.
			result.Outcome = OutcomeLocked
			result.Detail = locked.Owner.String()
			return e.finish(result, started)
		}
		result.Err = err
		return e.finish(result, started)
	}
	defer func() {
		if err := held.Release(ctx); err != nil {
			e.log().WarnContext(ctx, "the lock was not released; it expires on its own",
				"target", job.Target, "error", err)
		}
	}()

	// From here on the work runs on the lock's context: if the lease is lost,
	// the dump stops rather than racing whoever took it.
	//nolint:contextcheck // held.Context() derives from ctx; losing the lease has to stop the work.
	ctx = held.Context()

	// The due check happens again now that the lock is held. A replica whose
	// cached view was minutes stale would otherwise take the lock the instant
	// the other released it and back the same database up twice.
	if job.Schedule != nil {
		last, err := e.lastRun(ctx, target, job.Kind)
		if err != nil {
			result.Err = err
			return e.finish(result, started)
		}
		if !job.Due(last, e.now()) {
			result.Outcome = OutcomeNotDue
			result.Detail = "already run at " + last.Format(time.RFC3339)
			return e.finish(result, started)
		}
	}

	if e.Metrics != nil {
		e.Metrics.RunStarted(string(job.Kind))
		defer e.Metrics.RunFinished(string(job.Kind))
	}

	switch job.Kind {
	case KindBackup:
		result = e.backup(ctx, target, job, result)
	case KindVerify:
		result = e.verify(ctx, target, job, result)
	default:
		result.Err = fmt.Errorf("unknown job kind %q", job.Kind)
	}

	return e.finish(result, started)
}

// acquire takes the target's lock.
func (e *Executor) acquire(ctx context.Context, target *config.Target, job Job) (*lock.Held, error) {
	store, err := e.App.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	layout, err := e.App.Layout(target)
	if err != nil {
		return nil, err
	}

	locker := &lock.Locker{
		Store: store,
		Key:   layout.Lock(),
		TTL:   e.LockTTL,
		Log:   e.log(),
	}
	return locker.Acquire(ctx, target.Name, string(job.Kind))
}

// backup dumps the target and, when asked, applies its retention policy.
func (e *Executor) backup(ctx context.Context, target *config.Target, job Job, result Result) Result {
	spec, err := e.App.BackupSpec(target, tierOf(job))
	if err != nil {
		result.Err = err
		return result
	}
	runner, err := e.App.Runner(ctx, target)
	if err != nil {
		result.Err = err
		return result
	}
	// This run's logger, not the process-wide one: it is what the UI shows
	// beside the button somebody pressed.
	runner.Log = e.log()

	m, err := runner.Run(ctx, spec)
	if err != nil {
		result.Err = err
		if e.Metrics != nil {
			e.Metrics.BackupFailed(target.Name, phaseOf(err))
		}
		return result
	}

	result.Outcome = OutcomeRan
	result.BackupID = m.ID
	result.Manifest = m
	result.Detail = fmt.Sprintf("%d bytes stored at %s", m.Object.Bytes, m.Object.Key)

	if e.Metrics != nil {
		e.Metrics.BackupSucceeded(target.Name, string(m.Engine),
			time.Duration(m.DurationMS)*time.Millisecond, m.Object.Bytes, m.Plaintext.Bytes, m.FinishedAt)
	}

	if e.Prune {
		e.prune(ctx, target)
	}
	return result
}

// prune applies the retention policy of a target after a backup.
//
// It never fails the run that triggered it: the backup is already stored, and
// reporting it as failed because an old object could not be deleted would be
// exactly backwards.
func (e *Executor) prune(ctx context.Context, target *config.Target) {
	log := e.log().With("target", target.Name)

	runner, err := e.App.Pruner(ctx, target)
	if err != nil {
		log.WarnContext(ctx, "retention was not applied", "error", err)
		return
	}
	runner.Log = log

	plan, _, err := runner.Plan(ctx)
	if err != nil {
		log.WarnContext(ctx, "retention was not applied", "error", err)
		return
	}

	if e.Metrics != nil {
		e.Metrics.RetentionObjects(target.Name, retentionTiers(plan))
	}

	if plan.Blocked != "" {
		runner.AnnounceBlocked(ctx, plan)
		log.InfoContext(ctx, "retention kept everything", "reason", plan.Blocked)
		return
	}
	if len(plan.Delete) == 0 {
		return
	}

	objects, err := runner.Apply(ctx, plan, nil)
	if err != nil {
		log.WarnContext(ctx, "retention was not applied", "error", err)
		return
	}
	log.InfoContext(ctx, "retention applied",
		"deleted_backups", len(plan.Delete), "deleted_objects", objects, "bytes_freed", plan.Bytes())
}

// verify checks the target's most recent backup at its configured level.
func (e *Executor) verify(ctx context.Context, target *config.Target, job Job, result Result) Result {
	level := verify.Level(job.Level)
	if level == "" {
		level = verify.LevelIntegrity
	}

	entry, ok, err := e.latestBackup(ctx, target)
	if err != nil {
		result.Err = err
		return result
	}
	if !ok {
		// Nothing has been backed up yet. That is not a verification failure;
		// there is simply nothing to verify.
		result.Outcome = OutcomeNothing
		result.Detail = "no backup to verify yet"
		return result
	}

	verifier, err := e.App.Verifier(ctx, target, e.Identities, level)
	if err != nil {
		result.Err = err
		return result
	}
	verifier.Log = e.log()

	outcome, err := verifier.Backup(ctx, entry, level)
	if err != nil {
		result.Err = err
		return result
	}

	result.Outcome = OutcomeRan
	result.BackupID = entry.ID
	result.Detail = outcome.Summary(target.Name)

	switch {
	case outcome.Skipped:
		// A skip is not a failure and is not recorded (SPEC §8).
		result.Outcome = OutcomeNothing
	case outcome.OK:
		if e.Metrics != nil {
			e.Metrics.VerifySucceeded(target.Name, string(level), outcome.At)
		}
	default:
		result.Err = errors.New(outcome.Summary(target.Name))
	}
	return result
}

// lastRun is when this job last ran for this target, according to the index —
// the bucket, not this process. It is what makes the schedule survive a
// restart and stay honest across replicas.
//
// It is the last *attempt*, not the last success: a target failing every night
// must not be retried on every tick of the scheduler.
func (e *Executor) lastRun(ctx context.Context, target *config.Target, kind Kind) (time.Time, error) {
	idx, err := e.App.Index(ctx, target)
	if err != nil {
		return time.Time{}, err
	}
	entries, _, err := idx.Entries(ctx)
	if err != nil {
		return time.Time{}, err
	}

	var last time.Time
	for _, entry := range entries {
		switch kind {
		case KindBackup:
			if entry.FinishedAt.After(last) {
				last = entry.FinishedAt
			}
		case KindVerify:
			if entry.VerifiedAt != nil && entry.VerifiedAt.After(last) {
				last = *entry.VerifiedAt
			}
		}
	}
	return last, nil
}

// latestBackup is the newest stored backup of a target.
func (e *Executor) latestBackup(ctx context.Context, target *config.Target) (manifest.Entry, bool, error) {
	idx, err := e.App.Index(ctx, target)
	if err != nil {
		return manifest.Entry{}, false, err
	}
	entries, _, err := idx.Entries(ctx)
	if err != nil {
		return manifest.Entry{}, false, err
	}

	succeeded := make([]manifest.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Succeeded() {
			succeeded = append(succeeded, entry)
		}
	}
	if len(succeeded) == 0 {
		return manifest.Entry{}, false, nil
	}

	sort.Slice(succeeded, func(a, b int) bool {
		return succeeded[a].FinishedAt.After(succeeded[b].FinishedAt)
	})
	return succeeded[0], true, nil
}

func (e *Executor) finish(result Result, started time.Time) Result {
	result.Duration = e.now().Sub(started)
	return result
}

func (e *Executor) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

func (e *Executor) log() *slog.Logger {
	if e.Log == nil {
		return slog.Default()
	}
	return e.Log
}
