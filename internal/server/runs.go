package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// runHistory is how many finished runs the daemon remembers. It is a window
// for the UI, not a record: the index in the bucket is where a run's outcome
// actually lives, and this is gone on restart.
const runHistory = 50

// RunState is where a UI-started run has got to.
type RunState string

const (
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	// RunSkipped covers the outcomes that are neither: another process held
	// the lock, or there was nothing to do.
	RunSkipped RunState = "skipped"
)

// Run is one execution started from the UI, with the log it produced. The log
// is what makes the screen worth opening: an operator who pressed "back up
// now" wants to watch it, not poll the bucket.
type Run struct {
	ID         string    `json:"id"`
	Target     string    `json:"target"`
	Kind       string    `json:"kind"`
	State      RunState  `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Detail     string    `json:"detail,omitempty"`
	Error      string    `json:"error,omitempty"`
	BackupID   string    `json:"backup_id,omitempty"`
	Log        []LogLine `json:"log,omitempty"`
}

// LogLine is one captured log record.
type LogLine struct {
	At      time.Time      `json:"at"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// runs is the in-memory registry of UI-started executions.
type runs struct {
	mu   sync.Mutex
	byID map[string]*Run
	// order holds ids newest last, so trimming is a slice of the front.
	order []string
	// active names the targets with a run in flight, so the UI can grey the
	// button out rather than queueing work nobody watched start.
	active map[string]string
}

func newRuns() *runs {
	return &runs{byID: map[string]*Run{}, active: map[string]string{}}
}

// begin registers a run, refusing a second one on a target this daemon is
// already running.
//
// It is not the lock — the lock is in the bucket and covers every process —
// it is the local half, so two clicks in the same UI do not become two runs
// that then fight over the lease.
func (r *runs) begin(target, kind string) (*Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if id, ok := r.active[target]; ok {
		return r.byID[id], false
	}

	run := &Run{
		ID:        newRunID(),
		Target:    target,
		Kind:      kind,
		State:     RunRunning,
		StartedAt: time.Now().UTC(),
	}

	r.byID[run.ID] = run
	r.order = append(r.order, run.ID)
	r.active[target] = run.ID
	r.trim()

	return run, true
}

// finish records the outcome and releases the target.
func (r *runs) finish(id string, state RunState, detail, backupID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, ok := r.byID[id]
	if !ok {
		return
	}

	run.State = state
	run.FinishedAt = time.Now().UTC()
	run.Detail = detail
	run.BackupID = backupID
	if err != nil {
		run.Error = err.Error()
	}
	delete(r.active, run.Target)
}

// append adds a log line to a run.
func (r *runs) append(id string, line LogLine) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, ok := r.byID[id]
	if !ok {
		return
	}
	run.Log = append(run.Log, line)
}

// get returns a copy of one run.
func (r *runs) get(id string) (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	run, ok := r.byID[id]
	if !ok {
		return Run{}, false
	}
	return *run, true
}

// list returns every remembered run, newest first.
func (r *runs) list(target string) []Run {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Run, 0, len(r.order))
	for _, id := range r.order {
		run := r.byID[id]
		if target != "" && run.Target != target {
			continue
		}
		out = append(out, *run)
	}

	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// trim drops the oldest finished runs. Caller must hold the mutex.
func (r *runs) trim() {
	for len(r.order) > runHistory {
		oldest := r.order[0]
		r.order = r.order[1:]

		// A run still in flight is never dropped, however old: its target is
		// still marked active and the UI is still watching it.
		if run, ok := r.byID[oldest]; ok && run.State == RunRunning {
			r.order = append(r.order, oldest)
			continue
		}
		delete(r.byID, oldest)
	}
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// base is the context a background run inherits: the daemon's, so a shutdown
// stops it, but never the HTTP request's, which ends the moment the browser
// gets its 202.
func (s *Server) base() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
