package notify_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/notify"
)

// spy is a notifier that records what it was asked to deliver.
type spy struct {
	got []core.Notification
	err error
}

func (s *spy) Notify(_ context.Context, n core.Notification) error {
	s.got = append(s.got, n)
	return s.err
}

func event(name core.Event, target string) core.Notification {
	return notify.Notification(name, time.Now(), target, string(name))
}

func TestFanoutDeliversOnlySubscribedEvents(t *testing.T) {
	t.Parallel()

	failures, everything := &spy{}, &spy{}
	fanout := notify.NewFanout([]notify.Subscription{
		{Name: "pager", Notifier: failures, Events: []core.Event{core.EventBackupFailed}},
		{Name: "chat", Notifier: everything, Events: core.Events},
	}, slog.Default())

	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupSucceeded, "prod-pg")))
	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-pg")))

	assert.Len(t, failures.got, 1, "the pager only asked for failures")
	assert.Len(t, everything.got, 2)
}

// One broken endpoint must not cost the others their copy of the event.
func TestFanoutKeepsGoingPastAFailure(t *testing.T) {
	t.Parallel()

	broken := &spy{err: errors.New("connection refused")}
	working := &spy{}

	fanout := notify.NewFanout([]notify.Subscription{
		{Name: "broken", Notifier: broken, Events: core.Events},
		{Name: "working", Notifier: working, Events: core.Events},
	}, slog.Default())

	err := fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-pg"))
	require.Error(t, err)
	assert.Len(t, working.got, 1)
}

func TestDedupSuppressesARepeatInsideTheWindow(t *testing.T) {
	t.Parallel()

	target := &spy{}
	fanout := notify.NewFanout([]notify.Subscription{
		{Name: "chat", Notifier: target, Events: core.Events, Dedup: time.Hour},
	}, slog.Default())

	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	fanout.SetClock(func() time.Time { return now })

	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-pg")))
	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-pg")))
	assert.Len(t, target.got, 1, "the same failure inside the window is suppressed")

	// A different target is a different problem, however similar it looks.
	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-mysql")))
	assert.Len(t, target.got, 2)

	// A recovery is a different event, so it is never suppressed by the
	// failure that preceded it — which is the whole point of an alert.
	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupSucceeded, "prod-pg")))
	assert.Len(t, target.got, 3)

	now = now.Add(2 * time.Hour)
	require.NoError(t, fanout.Notify(context.Background(), event(core.EventBackupFailed, "prod-pg")))
	assert.Len(t, target.got, 4, "past the window it goes out again")
}

func TestSeverityComesFromTheEvent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, core.SeverityCritical, core.SeverityOf(core.EventBackupFailed))
	assert.Equal(t, core.SeverityCritical, core.SeverityOf(core.EventVerifyFailed))
	assert.Equal(t, core.SeverityWarning, core.SeverityOf(core.EventRetentionBlocked))
	assert.Equal(t, core.SeverityInfo, core.SeverityOf(core.EventBackupSucceeded))
}

// A delivery has to survive the cancellation of the run that produced it: the
// notification an operator most needs is the one about the run that was killed.
func TestEmitDeliversOnACancelledContext(t *testing.T) {
	t.Parallel()

	target := &spy{}
	fanout := notify.NewFanout([]notify.Subscription{
		{Name: "chat", Notifier: target, Events: core.Events},
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	notify.Emit(ctx, fanout, slog.Default(), event(core.EventBackupFailed, "prod-pg"))
	assert.Len(t, target.got, 1)
}

// Every call site holds a possibly-nil notifier, so this must be a no-op
// rather than the panic that would take a backup down with it.
func TestEmitToNobodyIsHarmless(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		notify.Emit(context.Background(), nil, slog.Default(), event(core.EventBackupFailed, "prod-pg"))
	})

	var fanout *notify.Fanout
	assert.NotPanics(t, func() {
		notify.Emit(context.Background(), fanout, slog.Default(), event(core.EventBackupFailed, "prod-pg"))
	})
}
