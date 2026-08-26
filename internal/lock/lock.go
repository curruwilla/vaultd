// Package lock is the per-target mutual exclusion vaultd runs under
// (SPEC §11).
//
// It is built on one object rather than on anything local, because the
// processes it has to keep apart are not on the same machine: two replicas of
// the daemon, a daemon and somebody's manual `vaultd backup`, a Kubernetes
// CronJob and the operator debugging it. The bucket is the only thing all of
// them can see.
//
// The design rests on two properties the store must have, both checked by
// `vaultd doctor`: a conditional create (If-None-Match), which is what makes
// acquisition a race exactly one caller wins, and a conditional overwrite
// (If-Match), which is what lets a heartbeat renew a lock without being able
// to steal one that has already changed hands.
//
// A lock is a lease, not a mutex. A holder that dies takes its lock with it
// only until the lease expires; without that, one OOM-killed dump would stop a
// target backing up until a human noticed.
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/buildinfo"
	"github.com/curruwilla/vaultd/internal/core"
)

// DefaultTTL is how long a lease lives without a heartbeat. It is long enough
// to survive a slow store or a paused process, and short enough that a crashed
// holder does not block tonight's backup.
const DefaultTTL = 5 * time.Minute

// renewDivisor sets the heartbeat interval to TTL/3, so a lease survives two
// consecutive failed renewals before anyone else can take it.
const renewDivisor = 3

// Owner describes who holds a lock. It is stored as the object's body, so the
// answer to "what is holding this" is one GET away — from any machine, without
// vaultd running.
type Owner struct {
	ID         string    `json:"id"`
	Target     string    `json:"target"`
	Host       string    `json:"host"`
	PID        int       `json:"pid"`
	Kind       string    `json:"kind,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Version    string    `json:"vaultd_version"`
}

// String renders the holder for an error an operator reads.
func (o Owner) String() string {
	where := o.Host
	if where == "" {
		where = "an unknown host"
	}
	return fmt.Sprintf("%s (pid %d), since %s, expires %s",
		where, o.PID, o.AcquiredAt.Format(time.RFC3339), o.ExpiresAt.Format(time.RFC3339))
}

// Expired reports whether the lease has run out.
func (o Owner) Expired(now time.Time) bool { return !now.Before(o.ExpiresAt) }

// ErrLocked is returned when somebody else holds the lock. It carries the
// holder so the caller can say who, which is the difference between a useful
// message and "resource busy".
type ErrLocked struct {
	Key   string
	Owner Owner
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("%s is already running: held by %s", e.Owner.Target, e.Owner)
}

// ErrLost is what a run sees when its lease was taken away underneath it. It
// means the run has to stop: something else believes it owns this target now.
var ErrLost = errors.New("the lock was lost")

// Locker acquires the lock of one target.
type Locker struct {
	Store core.Store
	// Key is the lock object, from manifest.Layout.Lock().
	Key string
	// TTL is the lease length; zero means DefaultTTL.
	TTL time.Duration
	Now func() time.Time
	Log *slog.Logger
}

// Held is an acquired lock. It renews itself until Release is called.
type Held struct {
	locker *Locker
	owner  Owner
	etag   string

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	released bool
	done     chan struct{}
}

// acquireAttempts bounds the retries. The only thing worth retrying here is
// the narrow window where the previous holder releases between our failed
// create and our read of who has it: a couple of attempts closes it, and
// looping forever would turn a wedged store into a hung backup.
const acquireAttempts = 3

// Acquire takes the lock for kind, or reports who has it.
//
// An expired lease is taken over with a conditional overwrite rather than a
// plain one: if two processes notice the same expiry at the same moment, the
// etag makes exactly one of them the new holder.
func (l *Locker) Acquire(ctx context.Context, target, kind string) (*Held, error) {
	var last error

	for range acquireAttempts {
		held, err := l.attempt(ctx, target, kind)
		if err == nil {
			return held, nil
		}

		// Somebody released between our create and our read: the lock is free
		// again, so try for it rather than reporting a holder that is gone.
		if errors.Is(err, core.ErrNotFound) {
			last = err
			continue
		}
		return nil, err
	}

	return nil, fmt.Errorf("the lock on %s kept changing hands; try again: %w", target, last)
}

func (l *Locker) attempt(ctx context.Context, target, kind string) (*Held, error) {
	now := l.now()
	owner := Owner{
		ID:         newID(),
		Target:     target,
		Host:       hostname(),
		PID:        os.Getpid(),
		Kind:       kind,
		AcquiredAt: now,
		ExpiresAt:  now.Add(l.ttl()),
		Version:    buildinfo.Short(),
	}

	body, err := json.Marshal(owner)
	if err != nil {
		return nil, fmt.Errorf("encoding the lock: %w", err)
	}

	created, err := l.Store.PutIfAbsent(ctx, l.Key, body)
	if err != nil {
		return nil, fmt.Errorf("taking the lock on %s: %w", target, err)
	}
	if created {
		return l.hold(ctx, owner, "")
	}

	// Somebody has it, or had it. Reading the body is what turns "busy" into
	// a message naming the host and the process.
	current, etag, err := l.read(ctx)
	if err != nil {
		return nil, err
	}
	if !current.Expired(now) {
		return nil, &ErrLocked{Key: l.Key, Owner: current}
	}

	updated, written, err := l.Store.PutIfMatch(ctx, l.Key, body, etag)
	if err != nil {
		return nil, fmt.Errorf("taking over the expired lock on %s: %w", target, err)
	}
	if !written {
		// Another process took the expired lease first. Re-read so the error
		// names whoever actually has it now.
		current, _, err := l.read(ctx)
		if err != nil {
			return nil, err
		}
		return nil, &ErrLocked{Key: l.Key, Owner: current}
	}

	l.log().WarnContext(ctx, "took over an expired lock; the previous holder probably died",
		"target", target, "previous_host", current.Host, "previous_pid", current.PID)

	return l.hold(ctx, owner, updated.ETag)
}

// hold starts the heartbeat and returns the handle. An empty etag means the
// write that took the lock did not report one, so it is read back: the
// heartbeat has nothing to condition on without it.
func (l *Locker) hold(ctx context.Context, owner Owner, etag string) (*Held, error) {
	if etag == "" {
		info, err := l.Store.Head(ctx, l.Key)
		if err != nil {
			return nil, fmt.Errorf("reading back the lock on %s: %w", owner.Target, err)
		}
		etag = info.ETag
	}

	held := &Held{
		locker: l,
		owner:  owner,
		etag:   etag,
		done:   make(chan struct{}),
	}
	held.ctx, held.cancel = context.WithCancelCause(ctx)

	// The heartbeat runs on the handle's own context, which is derived from
	// ctx: it must outlive this call and stop when the lock is released.
	go held.heartbeat(held.ctx) //nolint:contextcheck
	return held, nil
}

// read returns the current holder and the etag it was read at.
func (l *Locker) read(ctx context.Context) (Owner, string, error) {
	info, err := l.Store.Head(ctx, l.Key)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// It was released between the failed create and this read; the
			// caller retries rather than guessing.
			return Owner{}, "", err
		}
		return Owner{}, "", fmt.Errorf("reading the lock: %w", err)
	}

	body, err := l.Store.Get(ctx, l.Key)
	if err != nil {
		return Owner{}, "", fmt.Errorf("reading the lock: %w", err)
	}
	defer body.Close()

	var owner Owner
	if err := json.NewDecoder(body).Decode(&owner); err != nil {
		// A lock object we cannot parse is worse than no lock: refuse rather
		// than assume it is free.
		return Owner{}, "", fmt.Errorf("the lock object %s is not readable: %w", l.Key, err)
	}
	return owner, info.ETag, nil
}

// Context is cancelled when the lease is lost. A run holds it so that losing
// the lock stops the work rather than letting two dumps race.
func (h *Held) Context() context.Context { return h.ctx }

// Owner is who this handle says holds the lock.
func (h *Held) Owner() Owner { return h.owner }

// Release gives the lock up.
//
// It deletes the object only when it still carries the etag this holder last
// wrote, so a lease that was taken over while we were working is left to its
// new owner rather than deleted from under them.
func (h *Held) Release(ctx context.Context) error {
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return nil
	}
	h.released = true
	etag := h.etag
	h.mu.Unlock()

	h.cancel(nil)
	<-h.done

	// A released lock is released even if the run was cancelled: leaving it
	// behind would block the target until the lease expired.
	ctx = context.WithoutCancel(ctx)

	info, err := h.locker.Store.Head(ctx, h.locker.Key)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("releasing the lock: %w", err)
	}
	if info.ETag != etag {
		h.locker.log().WarnContext(ctx, "the lock changed hands while this run held it; leaving it alone",
			"target", h.owner.Target, "key", h.locker.Key)
		return nil
	}

	if err := h.locker.Store.Delete(ctx, []string{h.locker.Key}); err != nil {
		return fmt.Errorf("releasing the lock: %w", err)
	}
	return nil
}

// heartbeat renews the lease until the handle is released. It runs on the
// handle's own context, so a released or lost lock stops it.
func (h *Held) heartbeat(ctx context.Context) {
	defer close(h.done)

	interval := h.locker.ttl() / renewDivisor
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.renew(ctx); err != nil {
				h.locker.log().ErrorContext(ctx, "the lock could not be renewed; stopping this run",
					"target", h.owner.Target, "error", err)
				h.cancel(fmt.Errorf("%w: %w", ErrLost, err))
				return
			}
		}
	}
}

// renew extends the lease, conditioned on nobody having changed it.
func (h *Held) renew(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	owner := h.owner
	owner.ExpiresAt = h.locker.now().Add(h.locker.ttl())

	body, err := json.Marshal(owner)
	if err != nil {
		return err
	}

	// The renewal is bounded by the heartbeat interval: a store that hangs
	// must not hold the ticker up past the point where the lease expires. It
	// also outlives a cancellation of the run, so the lease survives long
	// enough for the run to shut down and release it properly.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.locker.ttl()/renewDivisor)
	defer cancel()

	info, written, err := h.locker.Store.PutIfMatch(ctx, h.locker.Key, body, h.etag)
	if err != nil {
		return err
	}
	if !written {
		return errors.New("another process holds it now")
	}

	h.owner = owner
	h.etag = info.ETag
	return nil
}

func (l *Locker) ttl() time.Duration {
	if l.TTL > 0 {
		return l.TTL
	}
	return DefaultTTL
}

func (l *Locker) now() time.Time {
	if l.Now == nil {
		return time.Now().UTC()
	}
	return l.Now().UTC()
}

func (l *Locker) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
