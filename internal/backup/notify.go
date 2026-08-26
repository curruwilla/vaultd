package backup

import (
	"errors"
	"fmt"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
)

// startedEvent announces a run before anything can go wrong with it. It is
// sent before the probe on purpose: a target whose server is unreachable
// should still show up as having tried.
func startedEvent(spec Spec, at time.Time) core.Notification {
	n := notify.Notification(core.EventBackupStarted, at, spec.Target,
		spec.Target+" backup started")
	n.Details = map[string]any{"engine": string(spec.Engine)}
	return n
}

// succeededEvent describes a stored backup. The numbers in it are the ones an
// operator compares against yesterday's: if the size halved, something was
// dropped from the database and nobody said so.
func succeededEvent(m *manifest.Manifest) core.Notification {
	duration := time.Duration(m.DurationMS) * time.Millisecond

	n := notify.Notification(core.EventBackupSucceeded, m.FinishedAt, m.Target,
		fmt.Sprintf("%s backup succeeded in %s, %s stored",
			m.Target, duration.Round(time.Second), humanBytes(m.Object.Bytes)))
	n.BackupID = m.ID
	n.DurationMS = m.DurationMS
	n.Details = map[string]any{
		"engine":          string(m.Engine),
		"server_version":  m.ServerVersion,
		"object":          m.Object.Key,
		"bytes":           m.Object.Bytes,
		"plaintext_bytes": m.Plaintext.Bytes,
		"consistency":     string(m.Consistency),
		"tables":          len(m.Tables),
	}
	if len(m.Warnings) > 0 {
		n.Details["warnings"] = m.Warnings
	}
	return n
}

// failedEvent describes a run that produced nothing. It carries the phase, a
// machine-readable code and the tail of the client's stderr, which between
// them are usually enough to act without opening a shell (SPEC §12).
func failedEvent(spec Spec, started, at time.Time, err error) core.Notification {
	duration := at.Sub(started)

	phase := PhaseDump
	stderr := ""
	var failure *Error
	if errors.As(err, &failure) {
		phase = failure.Phase
		stderr = failure.Stderr
	}

	n := notify.Notification(core.EventBackupFailed, at, spec.Target,
		fmt.Sprintf("%s backup failed after %s during %s",
			spec.Target, duration.Round(time.Second), phase))
	n.DurationMS = duration.Milliseconds()
	n.Error = &core.Failure{
		Phase:      string(phase),
		Code:       failureCode(phase, err),
		Message:    err.Error(),
		StderrTail: stderr,
	}
	n.Details = map[string]any{"engine": string(spec.Engine)}
	return n
}

// failureCode is the stable identifier a receiver can route on. A client that
// exited non-zero reports its exit code, because "pg_dump exited 1" and
// "pg_dump was killed by the OOM killer" call for different responses.
func failureCode(phase Phase, err error) string {
	var exit *engine.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("%s_EXIT_%d", upper(string(phase)), exit.Code)
	}
	return upper(string(phase)) + "_FAILED"
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return string(out)
}

// humanBytes renders a size for a one-line summary a human reads in chat.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
