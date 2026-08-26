package lock_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/lock"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

const key = "prod/_locks/prod-pg.lock"

func locker(store core.Store, now func() time.Time) *lock.Locker {
	return &lock.Locker{Store: store, Key: key, TTL: time.Minute, Now: now}
}

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

// The whole point: two processes, one lock.
func TestOnlyOneCallerGetsTheLock(t *testing.T) {
	t.Parallel()

	store := memory.New()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	first, err := locker(store, fixed(now)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	defer first.Release(t.Context()) //nolint:errcheck // the test asserts on the second caller

	_, err = locker(store, fixed(now)).Acquire(t.Context(), "prod-pg", "backup")
	require.Error(t, err)

	var held *lock.ErrLocked
	require.ErrorAs(t, err, &held, "the error has to name the holder, not just say busy")
	assert.Equal(t, "prod-pg", held.Owner.Target)
	assert.Contains(t, err.Error(), "already running")
}

func TestReleaseFreesTheLock(t *testing.T) {
	t.Parallel()

	store := memory.New()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	first, err := locker(store, fixed(now)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	require.NoError(t, first.Release(t.Context()))

	assert.NotContains(t, store.Objects(), key, "a released lock leaves no object behind")

	second, err := locker(store, fixed(now)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	require.NoError(t, second.Release(t.Context()))
}

// A holder that dies takes its lock with it only until the lease expires.
// Without that, one OOM-killed dump stops a target backing up until a human
// notices.
func TestAnExpiredLeaseIsTakenOver(t *testing.T) {
	t.Parallel()

	store := memory.New()
	start := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	dead, err := locker(store, fixed(start)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	assert.NotNil(t, dead)
	// No Release: this stands in for a process that was killed.

	later := start.Add(2 * time.Minute)
	next, err := locker(store, fixed(later)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	require.NoError(t, next.Release(t.Context()))
}

func TestALeaseThatIsStillLiveIsNotTakenOver(t *testing.T) {
	t.Parallel()

	store := memory.New()
	start := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	first, err := locker(store, fixed(start)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	defer first.Release(t.Context()) //nolint:errcheck // not what this asserts

	_, err = locker(store, fixed(start.Add(59*time.Second))).Acquire(t.Context(), "prod-pg", "backup")
	require.Error(t, err)
}

// The lock object is readable without vaultd: "what is holding this" has to be
// answerable with one GET from any machine.
func TestTheLockSaysWhoHoldsIt(t *testing.T) {
	t.Parallel()

	store := memory.New()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	held, err := locker(store, fixed(now)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)
	defer held.Release(t.Context()) //nolint:errcheck // not what this asserts

	body, err := store.Get(t.Context(), key)
	require.NoError(t, err)
	defer body.Close()

	raw, err := io.ReadAll(body)
	require.NoError(t, err)

	var owner lock.Owner
	require.NoError(t, json.Unmarshal(raw, &owner))
	assert.Equal(t, "prod-pg", owner.Target)
	assert.Equal(t, "backup", owner.Kind)
	assert.NotZero(t, owner.PID)
	assert.Equal(t, now.Add(time.Minute), owner.ExpiresAt.UTC())
}

// Releasing has to be conditional: a lease that was taken over while we worked
// belongs to somebody else now, and deleting it would hand the target to a
// third caller while the second is still dumping.
func TestReleaseLeavesALockThatChangedHands(t *testing.T) {
	t.Parallel()

	store := memory.New()
	start := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)

	first, err := locker(store, fixed(start)).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)

	// The lease expires and somebody else takes it while the first holder is
	// still (obliviously) working.
	second, err := locker(store, fixed(start.Add(2*time.Minute))).Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)

	require.NoError(t, first.Release(t.Context()))
	assert.Contains(t, store.Objects(), key, "the second holder still has its lock")

	require.NoError(t, second.Release(t.Context()))
	assert.NotContains(t, store.Objects(), key)
}

// A run holds the lock's context, so losing the lease stops the work instead
// of letting two dumps race.
func TestTheContextIsCancelledWhenTheLeaseIsLost(t *testing.T) {
	t.Parallel()

	store := memory.New()
	l := &lock.Locker{Store: store, Key: key, TTL: 90 * time.Millisecond, Now: time.Now}

	held, err := l.Acquire(t.Context(), "prod-pg", "backup")
	require.NoError(t, err)

	// Somebody overwrites the lock object, which is what a takeover looks like
	// from this side: the next heartbeat's If-Match no longer matches.
	_, err = store.Put(t.Context(), key, readerOf(`{"target":"prod-pg"}`), core.PutOptions{})
	require.NoError(t, err)

	select {
	case <-held.Context().Done():
		require.ErrorIs(t, context.Cause(held.Context()), lock.ErrLost)
	case <-time.After(3 * time.Second):
		t.Fatal("the run was not stopped after its lease was taken")
	}
}

func TestAnUnparseableLockIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()

	store := memory.New()
	_, err := store.Put(t.Context(), key, readerOf("not json"), core.PutOptions{})
	require.NoError(t, err)

	_, err = locker(store, time.Now).Acquire(t.Context(), "prod-pg", "backup")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not readable")

	var held *lock.ErrLocked
	assert.NotErrorAs(t, err, &held)
}

func readerOf(s string) io.Reader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
