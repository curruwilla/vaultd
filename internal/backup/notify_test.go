package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

// collector records the events a run emitted, in order.
type collector struct {
	got []core.Notification
	err error
}

func (c *collector) Notify(_ context.Context, n core.Notification) error {
	c.got = append(c.got, n)
	return c.err
}

func (c *collector) events() []core.Event {
	out := make([]core.Event, 0, len(c.got))
	for _, n := range c.got {
		out = append(out, n.Event)
	}
	return out
}

func TestSuccessfulRunAnnouncesItself(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	events := &collector{}
	runner := newRunner(memory.New(), newDumper())
	runner.Notify = events

	m, err := runner.Run(t.Context(), newSpec(t, identity))
	require.NoError(t, err)

	assert.Equal(t, []core.Event{core.EventBackupStarted, core.EventBackupSucceeded}, events.events())

	done := events.got[1]
	assert.Equal(t, "prod-pg", done.Target)
	assert.Equal(t, m.ID, done.BackupID)
	assert.Equal(t, core.SeverityInfo, done.Severity)
	assert.Equal(t, m.Object.Key, done.Details["object"])
	assert.Contains(t, done.Summary, "prod-pg backup succeeded")
}

// The payload of a failure is the whole point of the feature: the phase, a
// routable code and the tail of what the client said (SPEC §12).
func TestFailedRunCarriesThePhaseAndTheStderr(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.dumpErr = &engine.ExitError{Binary: "pg_dump", Code: 1, Stderr: "pg_dump: error: connection lost"}
	dumper.stderr = "pg_dump: error: connection lost"

	events := &collector{}
	runner := newRunner(memory.New(), dumper)
	runner.Notify = events

	_, err = runner.Run(t.Context(), newSpec(t, identity))
	require.Error(t, err)

	assert.Equal(t, []core.Event{core.EventBackupStarted, core.EventBackupFailed}, events.events())

	failed := events.got[1]
	assert.Equal(t, core.SeverityCritical, failed.Severity)
	require.NotNil(t, failed.Error)
	assert.Equal(t, "dump", failed.Error.Phase)
	assert.Equal(t, "DUMP_EXIT_1", failed.Error.Code, "the exit code routes: exit 1 and an OOM kill need different responses")
	assert.Contains(t, failed.Error.StderrTail, "connection lost")
	assert.Contains(t, failed.Summary, "during dump")
}

// A probe that never reaches the database still reports a failure, and it is
// attributed to the probe rather than to a dump that never started.
func TestProbeFailureIsAttributedToTheProbe(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.probeErr = errors.New("connection refused")

	events := &collector{}
	runner := newRunner(memory.New(), dumper)
	runner.Notify = events

	_, err = runner.Run(t.Context(), newSpec(t, identity))
	require.Error(t, err)

	failed := events.got[1]
	require.NotNil(t, failed.Error)
	assert.Equal(t, "probe", failed.Error.Phase)
	assert.Equal(t, "PROBE_FAILED", failed.Error.Code)
}

// The rule the whole notify layer exists to keep: a webhook that is down does
// not make a stored backup any less stored.
func TestADeadNotifierDoesNotFailTheBackup(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	events := &collector{err: errors.New("connection refused")}
	store := memory.New()
	runner := newRunner(store, newDumper())
	runner.Notify = events

	m, err := runner.Run(t.Context(), newSpec(t, identity))
	require.NoError(t, err, "delivery failures are logged, never returned")
	assert.Contains(t, store.Objects(), m.Object.Key)
}
