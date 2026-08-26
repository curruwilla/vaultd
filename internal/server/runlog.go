package server

import (
	"context"
	"log/slog"
	"time"
)

// maxRunLogLines bounds what one run keeps. A dump that logs a line per table
// on a database with fifty thousand of them must not become the daemon's
// memory profile.
const maxRunLogLines = 500

// runHandler is a slog.Handler that copies every record into a run's log on
// the way to the real handler.
//
// It exists so the UI can show what a run did without vaultd growing a second
// logging path: the same records that reach stderr reach the screen.
type runHandler struct {
	base  slog.Handler
	runs  *runs
	runID string
	attrs []slog.Attr
	group string
}

func newRunHandler(base slog.Handler, registry *runs, runID string) slog.Handler {
	if base == nil {
		base = slog.Default().Handler()
	}
	return &runHandler{base: base, runs: registry, runID: runID}
}

func (h *runHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level) || level >= slog.LevelInfo
}

func (h *runHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelInfo {
		h.capture(record)
	}
	if h.base.Enabled(ctx, record.Level) {
		return h.base.Handle(ctx, record)
	}
	return nil
}

func (h *runHandler) capture(record slog.Record) {
	run, ok := h.runs.get(h.runID)
	if !ok || len(run.Log) >= maxRunLogLines {
		return
	}

	line := LogLine{
		At:      record.Time.UTC(),
		Level:   record.Level.String(),
		Message: record.Message,
		Fields:  map[string]any{},
	}
	if line.At.IsZero() {
		line.At = time.Now().UTC()
	}

	for _, attr := range h.attrs {
		line.Fields[attr.Key] = attr.Value.Resolve().Any()
	}
	record.Attrs(func(attr slog.Attr) bool {
		line.Fields[attr.Key] = attr.Value.Resolve().Any()
		return true
	})
	if len(line.Fields) == 0 {
		line.Fields = nil
	}

	h.runs.append(h.runID, line)
}

func (h *runHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.base = h.base.WithAttrs(attrs)
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *runHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.base = h.base.WithGroup(name)
	clone.group = name
	return &clone
}
