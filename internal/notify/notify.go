package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// Subscription is one configured notifier and the events it asked for.
type Subscription struct {
	Name     string
	Notifier core.Notifier
	Events   []core.Event
	// Dedup suppresses a repeat of the same event for the same target inside
	// this window. Zero means every event is delivered.
	Dedup time.Duration
}

// wants reports whether this subscription asked for an event.
func (s Subscription) wants(event core.Event) bool {
	for _, subscribed := range s.Events {
		if subscribed == event {
			return true
		}
	}
	return false
}

// Fanout delivers a notification to every subscription that asked for it.
//
// It implements core.Notifier so that a runner holds one field rather than a
// list, and it is the layer where the "a webhook never fails a backup" rule
// lives: it returns the delivery failures so its caller can log them, and
// every caller does exactly that.
type Fanout struct {
	subs []Subscription
	log  *slog.Logger
	now  func() time.Time

	mu   sync.Mutex
	sent map[dedupKey]time.Time
}

type dedupKey struct {
	notifier string
	target   string
	event    core.Event
}

// NewFanout returns a dispatcher over the given subscriptions.
func NewFanout(subs []Subscription, log *slog.Logger) *Fanout {
	if log == nil {
		log = slog.Default()
	}
	return &Fanout{
		subs: subs,
		log:  log,
		now:  time.Now,
		sent: map[dedupKey]time.Time{},
	}
}

// SetClock replaces the dedup clock. Tests use it; nothing else should.
func (f *Fanout) SetClock(now func() time.Time) { f.now = now }

// Subscriptions returns how many notifiers this fanout holds.
func (f *Fanout) Subscriptions() int { return len(f.subs) }

// Notify delivers to everyone who asked, and keeps going past a failure: one
// broken endpoint must not cost the others their copy of the event.
func (f *Fanout) Notify(ctx context.Context, n core.Notification) error {
	if f == nil {
		return nil
	}

	var failures []error
	for _, sub := range f.subs {
		if !sub.wants(n.Event) {
			continue
		}
		if f.suppress(sub, n) {
			f.log.DebugContext(ctx, "notification deduplicated",
				"notifier", sub.Name, "event", string(n.Event), "target", n.Target)
			continue
		}

		if err := sub.Notifier.Notify(ctx, n); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// suppress applies the dedup window, recording the delivery it lets through.
func (f *Fanout) suppress(sub Subscription, n core.Notification) bool {
	if sub.Dedup <= 0 {
		return false
	}

	key := dedupKey{notifier: sub.Name, target: n.Target, event: n.Event}
	now := f.now()

	f.mu.Lock()
	defer f.mu.Unlock()

	if last, ok := f.sent[key]; ok && now.Sub(last) < sub.Dedup {
		return true
	}
	f.sent[key] = now
	return false
}

// Emit delivers a notification and turns any delivery failure into a log line.
//
// It is the only way the rest of the codebase sends an event, which is what
// makes SPEC §12's rule structural rather than a convention: there is no
// return value to accidentally propagate into a backup's exit code.
func Emit(ctx context.Context, to core.Notifier, log *slog.Logger, n core.Notification) {
	if to == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	// A notification about a run that was cancelled still has to go out; the
	// cancelled context it inherited would abort the POST before it started.
	ctx = context.WithoutCancel(ctx)

	if err := to.Notify(ctx, n); err != nil {
		log.WarnContext(ctx, "the notification was not delivered; the backup is unaffected",
			"event", string(n.Event), "target", n.Target, "error", err)
	}
}

// Notification builds the payload of an event, filling in what is always the
// same: the severity of the event and the time it happened.
func Notification(event core.Event, at time.Time, target, summary string) core.Notification {
	return core.Notification{
		Event:    event,
		At:       at.UTC(),
		Target:   target,
		Severity: core.SeverityOf(event),
		Summary:  summary,
	}
}
