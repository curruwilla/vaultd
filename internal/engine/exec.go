// Package engine holds what the database adapters share: capturing a client's
// stderr, resolving which client binary to run, and reporting a failed dump in
// a way an operator can act on.
//
// Every engine shells out to the vendor's own client (SPEC §3). A pure-Go
// dumper cannot match pg_dump or mysqldump on extensions, generated columns,
// triggers, charsets or GTID state, and a backup that restores incorrectly is
// worse than no backup.
package engine

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// StderrTailBytes is how much of a client's stderr is kept for the manifest
// and the failure webhook (SPEC §11). The interesting part of a dump failure
// is always at the end.
const StderrTailBytes = 64 << 10

// Tail is an io.Writer that remembers only the last StderrTailBytes written to
// it. A dumper can write megabytes of notices without growing our footprint.
type Tail struct {
	mu   sync.Mutex
	max  int
	buf  []byte
	over bool
}

// NewTail returns a Tail keeping at most max bytes.
func NewTail(max int) *Tail {
	if max <= 0 {
		max = StderrTailBytes
	}
	return &Tail{max: max}
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
		t.over = true
	}
	return len(p), nil
}

// String returns the captured tail, marked when output was dropped.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := strings.TrimRight(string(t.buf), "\n")
	if t.over {
		return "…\n" + out
	}
	return out
}

// LastLines returns the last n non-empty lines, for a one-paragraph error.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")

	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append([]string{lines[i]}, out...)
		}
	}
	return strings.Join(out, "\n")
}

// Env builds the environment a client binary runs with. It starts from a
// minimal set rather than the daemon's own environment, so an unrelated PG* or
// MYSQL_* variable on the host cannot silently redirect a backup to the wrong
// server.
func Env(vars map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		// A predictable locale keeps client diagnostics parseable.
		"LC_ALL=C",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	// A client built outside the system paths needs its libraries; LD_PRELOAD
	// and the rest of the loader knobs stay out.
	if libs := os.Getenv("LD_LIBRARY_PATH"); libs != "" {
		env = append(env, "LD_LIBRARY_PATH="+libs)
	}

	for key, value := range vars {
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// ExitError describes a client that ran but failed. It carries the stderr tail
// so the reason travels with the error, into the manifest and the webhook.
type ExitError struct {
	Binary   string
	Code     int
	Stderr   string
	Original error
}

func (e *ExitError) Error() string {
	detail := LastLines(e.Stderr, 3)
	if detail == "" {
		return fmt.Sprintf("%s exited with code %d", e.Binary, e.Code)
	}
	return fmt.Sprintf("%s exited with code %d: %s", e.Binary, e.Code, detail)
}

func (e *ExitError) Unwrap() error { return e.Original }
