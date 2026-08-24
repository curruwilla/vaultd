package config

import (
	"fmt"
	"strings"
)

// Severity classifies a diagnostic. Only SeverityError fails `vaultd validate`.
type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warning"
)

// Diagnostic is one problem found in a config. Message is self-contained — it
// names the offending target or destination — so it reads correctly on its own
// line; Path locates it in the document for machine consumers.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

func (d Diagnostic) String() string { return string(d.Severity) + ": " + d.Message }

// Diagnostics is an ordered set of findings; validation collects all of them
// rather than stopping at the first, so one run fixes one round of problems.
type Diagnostics []Diagnostic

func (ds *Diagnostics) errorf(path, format string, args ...any) {
	*ds = append(*ds, Diagnostic{Severity: SeverityError, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (ds *Diagnostics) warnf(path, format string, args ...any) {
	*ds = append(*ds, Diagnostic{Severity: SeverityWarn, Path: path, Message: fmt.Sprintf(format, args...)})
}

// HasErrors reports whether any diagnostic is fatal.
func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Count returns how many diagnostics have the given severity.
func (ds Diagnostics) Count(s Severity) int {
	n := 0
	for _, d := range ds {
		if d.Severity == s {
			n++
		}
	}
	return n
}

// Err returns an error summarizing the fatal diagnostics, or nil if there are
// none. The full list is meant to be printed separately, line by line.
func (ds Diagnostics) Err() error {
	n := ds.Count(SeverityError)
	if n == 0 {
		return nil
	}
	return fmt.Errorf("config has %s", plural(n, "error", "errors"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// oneOf renders an enumeration for an error message: `s, r2, minio`.
func oneOf[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// valid reports whether value is in values.
func valid[T ~string](value T, values []T) bool {
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}
