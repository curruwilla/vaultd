package server

import (
	"sync"
	"time"
)

const (
	// failureBudget is how many wrong tokens one address may present before it
	// has to wait. It is generous enough that a mistyped token is not a
	// lockout, and tight enough that guessing is hopeless.
	failureBudget = 5
	// lockout is how long a source waits once it is over budget. It doubles
	// per further failure, up to maxLockout.
	lockout    = 5 * time.Second
	maxLockout = 5 * time.Minute
	// forget is how long an address is remembered after its last failure.
	forget = 15 * time.Minute
)

// throttle rate limits failed authentication per source address (SPEC §13).
//
// It is per address rather than global so one misconfigured scraper cannot
// lock the operator out of their own UI.
type throttle struct {
	mu    sync.Mutex
	seen  map[string]*attempts
	now   func() time.Time
	swept time.Time
}

type attempts struct {
	count int
	last  time.Time
	until time.Time
}

func newThrottle() *throttle {
	return &throttle{seen: map[string]*attempts{}, now: time.Now}
}

// blocked reports whether this address must wait, and for how long.
func (t *throttle) blocked(host string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.sweep()

	record, ok := t.seen[host]
	if !ok {
		return 0, false
	}

	wait := record.until.Sub(t.now())
	if wait <= 0 {
		return 0, false
	}
	return wait, true
}

func (t *throttle) failed(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, ok := t.seen[host]
	if !ok {
		record = &attempts{}
		t.seen[host] = record
	}

	record.count++
	record.last = t.now()

	if record.count > failureBudget {
		penalty := lockout << min(record.count-failureBudget-1, 6)
		if penalty > maxLockout {
			penalty = maxLockout
		}
		record.until = record.last.Add(penalty)
	}
}

// succeeded clears the record: a correct token means whoever it is belongs
// here, and their earlier typos should not follow them around.
func (t *throttle) succeeded(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, host)
}

// sweep drops records nobody has touched for a while, so the map cannot grow
// without bound under a spray of forged source addresses.
func (t *throttle) sweep() {
	now := t.now()
	if now.Sub(t.swept) < forget {
		return
	}
	t.swept = now

	for host, record := range t.seen {
		if now.Sub(record.last) > forget {
			delete(t.seen, host)
		}
	}
}
