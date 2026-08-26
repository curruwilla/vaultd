package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

const (
	// defaultInterval is how often the scheduler asks what is due. It is not
	// the resolution of the schedules — cron's is a minute — it is how quickly
	// the daemon notices one, and a short tick is what makes a restart catch
	// up promptly.
	defaultInterval = 30 * time.Second
	// defaultRefresh is how often the cached "when did this last run" is
	// re-read from the index. Between refreshes another replica's run may be
	// invisible here, which is why the executor checks again under the lock.
	defaultRefresh = 5 * time.Minute
)

// Scheduler dispatches jobs when they come due.
type Scheduler struct {
	Jobs []Job
	Exec *Executor
	Log  *slog.Logger
	// Interval is how often to evaluate; zero means defaultInterval.
	Interval time.Duration
	// Refresh is how often to re-read last-run times from the bucket; zero
	// means defaultRefresh.
	Refresh time.Duration
	Now     func() time.Time

	mu    sync.Mutex
	last  map[string]time.Time
	ready bool

	workers map[string]*worker
}

// worker serializes the jobs of one target.
//
// One worker per target rather than per job because the lock is per target:
// a verify and a backup of the same database are mutually exclusive anyway,
// and letting them queue in one place is what makes on_overlap mean something.
type worker struct {
	target  string
	overlap config.Overlap
	jobs    chan Job
	busy    atomic.Bool
	done    chan struct{}
}

// Run evaluates the schedule until ctx is cancelled.
//
// It ticks once immediately: a daemon that has just started has to notice the
// backups it missed while it was down, and waiting a full interval to find out
// would make every deploy a small gap in coverage.
func (s *Scheduler) Run(ctx context.Context) error {
	s.start(ctx)
	defer s.stop()

	if err := s.refresh(ctx); err != nil {
		// A bucket that cannot be read now may be readable in thirty seconds.
		// Starting anyway, loudly, beats refusing to run at all.
		s.log().WarnContext(ctx, "the schedule state could not be read; running on what the config says",
			"error", err)
	}

	s.log().InfoContext(ctx, "scheduler started", "jobs", len(s.Jobs), "interval", s.interval())
	s.tick(ctx)

	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()

	refresh := time.NewTicker(s.refreshInterval())
	defer refresh.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log().InfoContext(ctx, "scheduler stopping; waiting for runs in flight")
			return nil
		case <-refresh.C:
			if err := s.refresh(ctx); err != nil {
				s.log().WarnContext(ctx, "the schedule state could not be refreshed", "error", err)
			}
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// RunOnce dispatches everything due right now and waits for it, which is what
// `vaultd run` does for a Kubernetes CronJob or a systemd timer.
//
// It runs targets one at a time on purpose: a one-shot invocation that dumped
// six databases at once would decide the load profile of the host for the
// operator.
func (s *Scheduler) RunOnce(ctx context.Context) ([]Result, error) {
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}

	var results []Result
	for _, job := range s.Jobs {
		if !s.due(job) {
			continue
		}

		result := s.Exec.Run(ctx, job)
		s.record(job, result)
		results = append(results, result)

		if err := ctx.Err(); err != nil {
			return results, err
		}
	}
	return results, nil
}

// Due lists the jobs that would run right now, without running them.
func (s *Scheduler) Due(ctx context.Context) ([]Job, error) {
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}

	var due []Job
	for _, job := range s.Jobs {
		if s.due(job) {
			due = append(due, job)
		}
	}
	return due, nil
}

// Next reports when each job runs next, for the daemon's startup log and the
// UI's "next execution" column.
func (s *Scheduler) Next(job Job) time.Time {
	s.mu.Lock()
	last := s.last[job.Key()]
	s.mu.Unlock()

	from := last
	if from.IsZero() {
		from = s.now()
	}
	return job.Next(from)
}

// tick dispatches every job that has come due.
func (s *Scheduler) tick(ctx context.Context) {
	for _, job := range s.Jobs {
		if !s.due(job) {
			continue
		}
		s.dispatch(ctx, job)
	}
}

// dispatch hands a job to its target's worker, applying the overlap policy.
func (s *Scheduler) dispatch(ctx context.Context, job Job) {
	w, ok := s.workers[job.Target]
	if !ok {
		return
	}

	// Claim the slot before the worker picks it up. Without this the same job
	// would be dispatched on every tick until the run got far enough to write
	// its first index entry.
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string]time.Time{}
	}
	s.last[job.Key()] = s.now()
	s.mu.Unlock()

	if w.overlap != config.OverlapQueue && (w.busy.Load() || len(w.jobs) > 0) {
		s.missed(ctx, job, w.overlap)
		return
	}

	select {
	case w.jobs <- job:
	default:
		s.missed(ctx, job, w.overlap)
	}
}

// missed reports a run the overlap policy did not start.
//
// `skip` is the default and is a warning: it is normal for a dump that ran
// long, and abnormal if it keeps happening. `fail` says the operator wants to
// hear about it as a failure, so it is logged as one — but it still does not
// stop the daemon, which has other targets to serve.
func (s *Scheduler) missed(ctx context.Context, job Job, overlap config.Overlap) {
	if s.Exec.Metrics != nil {
		s.Exec.Metrics.ScheduleMissed(job.Target, string(job.Kind))
	}

	reason := fmt.Sprintf("%s of %s was due while the previous run was still going (on_overlap: %s)",
		job.Kind, job.Target, overlapName(overlap))

	if overlap == config.OverlapFail {
		s.log().ErrorContext(ctx, "a scheduled run was skipped", "target", job.Target, "kind", string(job.Kind))
	} else {
		s.log().WarnContext(ctx, "a scheduled run was skipped", "target", job.Target, "kind", string(job.Kind))
	}

	s.announce(ctx, job, reason)
}

// announce sends schedule.missed to the target's notifiers.
func (s *Scheduler) announce(ctx context.Context, job Job, reason string) {
	target, ok := s.Exec.App.Config().Target(job.Target)
	if !ok {
		return
	}

	notifier, err := s.Exec.App.Notifier(target)
	if err != nil || notifier == nil {
		return
	}

	n := notify.Notification(core.EventScheduleMissed, s.now(), job.Target, reason)
	n.Details = map[string]any{"kind": string(job.Kind), "schedule": job.Spec}
	notify.Emit(ctx, notifier, s.log(), n)
}

// start brings up one worker per target.
func (s *Scheduler) start(ctx context.Context) {
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string]time.Time{}
	}
	s.mu.Unlock()

	s.workers = map[string]*worker{}
	for _, job := range s.Jobs {
		if _, ok := s.workers[job.Target]; ok {
			continue
		}

		w := &worker{
			target: job.Target,
			// One pending slot: queueing two runs of the same database behind
			// a third would be a backlog, not a schedule.
			jobs: make(chan Job, 1),
			done: make(chan struct{}),
		}
		if target, ok := s.Exec.App.Config().Target(job.Target); ok {
			w.overlap = target.OnOverlap
		}

		s.workers[job.Target] = w
		go s.serve(ctx, w)
	}
}

// serve is one worker's loop.
func (s *Scheduler) serve(ctx context.Context, w *worker) {
	defer close(w.done)

	for job := range w.jobs {
		w.busy.Store(true)

		result := s.Exec.Run(ctx, job)
		s.record(job, result)
		s.report(ctx, result)

		w.busy.Store(false)
	}
}

// stop closes the workers and waits for the runs in flight.
func (s *Scheduler) stop() {
	for _, w := range s.workers {
		close(w.jobs)
	}
	for _, w := range s.workers {
		<-w.done
	}
}

// report turns a result into the one log line an operator reads.
func (s *Scheduler) report(ctx context.Context, result Result) {
	log := s.log().With("target", result.Job.Target, "kind", string(result.Job.Kind),
		"duration", result.Duration.Round(time.Millisecond))

	switch {
	case result.Err != nil:
		log.ErrorContext(ctx, "the scheduled run failed", "error", result.Err)
	case result.Outcome == OutcomeLocked:
		log.InfoContext(ctx, "another process is running this target", "holder", result.Detail)
	case result.Outcome == OutcomeNotDue:
		log.InfoContext(ctx, "another process already ran this", "detail", result.Detail)
	case result.Outcome == OutcomeNothing:
		log.InfoContext(ctx, "nothing to do", "detail", result.Detail)
	default:
		log.InfoContext(ctx, "the scheduled run finished", "detail", result.Detail)
	}
}

// record updates the cached last-run time.
//
// A run that was locked out or found nothing is recorded too: without that,
// a verification that keeps being skipped — a staging server a major behind —
// would be retried on every tick for as long as the daemon is up.
func (s *Scheduler) record(job Job, _ Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = map[string]time.Time{}
	}
	s.last[job.Key()] = s.now()
}

// refresh re-reads last-run times from the index, which is the shared truth
// between replicas and across restarts.
func (s *Scheduler) refresh(ctx context.Context) error {
	last := map[string]time.Time{}

	for _, job := range s.Jobs {
		target, ok := s.Exec.App.Config().Target(job.Target)
		if !ok {
			continue
		}

		at, err := s.Exec.lastRun(ctx, target, job.Kind)
		if err != nil {
			return err
		}
		last[job.Key()] = at
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = map[string]time.Time{}
	}

	// Never move a job backwards: this process may have run something since
	// the index was read, and forgetting that would run it again.
	for key, at := range last {
		if current, ok := s.last[key]; !ok || at.After(current) {
			s.last[key] = at
		}
	}
	s.ready = true
	return nil
}

// due answers from the cache. The executor asks the index again once it holds
// the lock, which is what keeps two replicas from both acting on a stale yes.
func (s *Scheduler) due(job Job) bool {
	s.mu.Lock()
	last := s.last[job.Key()]
	s.mu.Unlock()

	return job.Due(last, s.now())
}

func (s *Scheduler) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return defaultInterval
}

func (s *Scheduler) refreshInterval() time.Duration {
	if s.Refresh > 0 {
		return s.Refresh
	}
	return defaultRefresh
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func overlapName(overlap config.Overlap) string {
	if overlap == "" {
		return string(config.OverlapSkip)
	}
	return string(overlap)
}
