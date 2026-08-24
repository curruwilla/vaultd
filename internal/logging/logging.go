// Package logging sets up the structured logger every command shares.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Format selects the log encoding.
type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// Setup builds a logger and installs it as the default. Secrets never reach it
// by accident: config.Secret redacts itself through slog.LogValuer, and DSNs
// are passed through config.RedactDSN at the call site (SPEC §15).
func Setup(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		return nil, fmt.Errorf("invalid log level %q: use debug, info, warn or error", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch Format(strings.ToLower(format)) {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case FormatText:
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q: use json or text", format)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}
