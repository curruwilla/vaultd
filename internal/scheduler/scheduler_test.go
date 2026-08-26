package scheduler_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/scheduler"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

const configYAML = `
version: 1
defaults:
  encryption: { mode: none }
  compression: { algo: none }
  retention: { daily: { keep: 7 }, min_keep: 1 }
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret
    prefix: prod
targets:
  - name: prod-pg
    engine: postgres
    dsn: postgres://backup@pg:5432/app
    destination: r2
    schedule: "0 3 * * *"
`

func load(t *testing.T, yaml string) *config.Config {
	t.Helper()

	cfg, diags, err := config.Parse([]byte(yaml), config.LoadOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "%v", diags)
	return cfg
}

// fakeDumper produces a fixed payload and counts how often it was asked to.
type fakeDumper struct {
	dumps atomic.Int32
	delay time.Duration
}

func (f *fakeDumper) Probe(context.Context) (core.ServerInfo, error) {
	return core.ServerInfo{
		Engine:      core.EnginePostgres,
		Version:     "17.2",
		Consistency: core.ConsistencySerializableSnapshot,
		Tables:      []core.TableInfo{{Name: "public.users", Rows: 42}},
	}, nil
}

func (f *fakeDumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	f.dumps.Add(1)

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return core.DumpResult{}, ctx.Err()
		}
	}

	if _, err := w.Write(bytes.Repeat([]byte("PGDMP payload\n"), 64)); err != nil {
		return core.DumpResult{}, err
	}
	return core.DumpResult{DumperVersion: "pg_dump 17.2"}, nil
}

// replica builds one daemon's view of the same bucket, the way two pods would
// each build their own from the same config.
func replica(t *testing.T, store core.Store, dumper core.Dumper) (*app.App, *scheduler.Executor) {
	t.Helper()
	return replicaOf(t, configYAML, store, dumper)
}

func replicaOf(t *testing.T, yaml string, store core.Store, dumper core.Dumper) (*app.App, *scheduler.Executor) {
	t.Helper()

	application := app.New(load(t, yaml), nil)
	application.SetStore("r2", store)
	application.SetDumper("prod-pg", dumper)

	return application, &scheduler.Executor{App: application, LockTTL: time.Minute}
}

func jobs(t *testing.T, application *app.App) []scheduler.Job {
	t.Helper()

	built, err := scheduler.Jobs(application.Config())
	require.NoError(t, err)
	return built
}

// The M7 acceptance gate: two replicas, one backup.
func TestTwoReplicasDoNotBothRunTheSameTarget(t *testing.T) {
	store := memory.New()
	dumper := &fakeDumper{delay: 150 * time.Millisecond}

	appA, execA := replica(t, store, dumper)
	_, execB := replica(t, store, dumper)

	job := jobs(t, appA)[0]

	var (
		wg      sync.WaitGroup
		results = make([]scheduler.Result, 2)
	)
	for i, exec := range []*scheduler.Executor{execA, execB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = exec.Run(t.Context(), job)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), dumper.dumps.Load(), "the database must be dumped exactly once")

	ran, blocked := 0, 0
	for _, result := range results {
		require.NoError(t, result.Err)
		switch result.Outcome {
		case scheduler.OutcomeRan:
			ran++
		case scheduler.OutcomeLocked, scheduler.OutcomeNotDue:
			blocked++
		}
	}
	assert.Equal(t, 1, ran)
	assert.Equal(t, 1, blocked, "the loser reports why, rather than failing")
}

// The lock alone is not enough: a replica whose cached view is stale would
// take the lock the instant the other released it. The due check under the
// lock is what stops the second run.
func TestASecondRunAfterTheFirstFinishesIsNotDue(t *testing.T) {
	store := memory.New()
	dumper := &fakeDumper{}

	appA, execA := replica(t, store, dumper)
	_, execB := replica(t, store, dumper)

	job := jobs(t, appA)[0]

	first := execA.Run(t.Context(), job)
	require.NoError(t, first.Err)
	require.Equal(t, scheduler.OutcomeRan, first.Outcome)

	second := execB.Run(t.Context(), job)
	require.NoError(t, second.Err)
	assert.Equal(t, scheduler.OutcomeNotDue, second.Outcome)
	assert.Equal(t, int32(1), dumper.dumps.Load())
}

// A run somebody asked for explicitly still takes the lock, but is never
// "not due": that is what makes `vaultd backup` and the UI's button work.
func TestAManualRunIsAlwaysDue(t *testing.T) {
	store := memory.New()
	dumper := &fakeDumper{}

	appA, exec := replica(t, store, dumper)

	first := exec.Run(t.Context(), jobs(t, appA)[0])
	require.NoError(t, first.Err)

	second := exec.Run(t.Context(), scheduler.Manual("prod-pg", scheduler.KindBackup))
	require.NoError(t, second.Err)
	assert.Equal(t, scheduler.OutcomeRan, second.Outcome)
	assert.Equal(t, int32(2), dumper.dumps.Load())
}

// A released lock leaves nothing behind, so the next run is not slowed down by
// waiting for a lease nobody holds to expire.
func TestTheLockIsReleasedAfterARun(t *testing.T) {
	store := memory.New()

	appA, exec := replica(t, store, &fakeDumper{})
	require.NoError(t, exec.Run(t.Context(), jobs(t, appA)[0]).Err)

	assert.NotContains(t, store.Objects(), "prod/_locks/prod-pg.lock")
}

// A daemon that has been down for a day has to notice, and running a target
// that has never run is what "notice" means.
func TestRunOnceRunsWhatIsDueAndNothingElse(t *testing.T) {
	store := memory.New()
	dumper := &fakeDumper{}

	application, exec := replica(t, store, dumper)
	sched := &scheduler.Scheduler{Jobs: jobs(t, application), Exec: exec}

	results, err := sched.RunOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, scheduler.OutcomeRan, results[0].Outcome)

	// Immediately afterwards nothing is due: the schedule is derived from the
	// bucket, so a second invocation a minute later is a no-op.
	again, err := sched.RunOnce(t.Context())
	require.NoError(t, err)
	assert.Empty(t, again)
	assert.Equal(t, int32(1), dumper.dumps.Load())
}

func TestDueReportsWithoutRunning(t *testing.T) {
	store := memory.New()
	dumper := &fakeDumper{}

	application, exec := replica(t, store, dumper)
	sched := &scheduler.Scheduler{Jobs: jobs(t, application), Exec: exec}

	due, err := sched.Due(t.Context())
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, "prod-pg", due[0].Target)
	assert.Zero(t, dumper.dumps.Load(), "--dry-run runs nothing")
}

// Retention runs after a scheduled backup, which is what makes rotation
// automatic rather than something an operator has to remember (objective O3).
func TestPruneRunsAfterAScheduledBackup(t *testing.T) {
	// One backup per day is kept, so the second of two taken on the same day
	// expires the first.
	yaml := strings.Replace(configYAML,
		"retention: { daily: { keep: 7 }, min_keep: 1 }",
		"retention: { daily: { keep: 1 }, min_keep: 1 }", 1)

	store := memory.New()
	application, exec := replicaOf(t, yaml, store, &fakeDumper{})
	exec.Prune = true

	job := scheduler.Manual("prod-pg", scheduler.KindBackup)
	require.NoError(t, exec.Run(t.Context(), job).Err)
	require.Equal(t, 1, storedBackups(t, application), "the first backup is the only one there is")

	require.NoError(t, exec.Run(t.Context(), job).Err)
	assert.Equal(t, 1, storedBackups(t, application),
		"the second backup of the day replaces the first, without anyone running prune")
}

// Rotation must never be able to fail the backup that triggered it: the backup
// is already in the bucket.
func TestAFailingPruneDoesNotFailTheBackup(t *testing.T) {
	store := memory.New()
	application, exec := replica(t, store, &fakeDumper{})
	exec.Prune = true

	// A store that refuses deletions is what a least-privilege credential
	// missing s3:DeleteObject looks like.
	application.SetStore("r2", &undeletable{Store: store})

	result := exec.Run(t.Context(), scheduler.Manual("prod-pg", scheduler.KindBackup))
	require.NoError(t, result.Err)
	assert.Equal(t, scheduler.OutcomeRan, result.Outcome)
}

func storedBackups(t *testing.T, application *app.App) int {
	t.Helper()

	target, ok := application.Config().Target("prod-pg")
	require.True(t, ok)

	idx, err := application.Index(t.Context(), target)
	require.NoError(t, err)

	entries, _, err := idx.Entries(t.Context())
	require.NoError(t, err)

	stored := 0
	for _, entry := range entries {
		if entry.Succeeded() {
			stored++
		}
	}
	return stored
}

type undeletable struct{ *memory.Store }

func (u *undeletable) Delete(context.Context, []string) error {
	return errors.New("access denied")
}

func verifySpec() *config.VerifySpec {
	return &config.VerifySpec{Level: config.VerifyIntegrity, Schedule: "0 5 * * 0"}
}
