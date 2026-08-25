package manifest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// Outcome records how a run ended. Failures are indexed too: retention has to
// know that the most recent attempt failed before it deletes anything old
// (SPEC §7, invariant 3), and an index that only records successes cannot tell
// "nothing ran" from "everything broke".
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

// Entry is one line of the per-target index: everything a listing needs
// without fetching every manifest. The index is a cache — the manifests in the
// bucket remain the source of truth, and `vaultd reindex` rebuilds it.
type Entry struct {
	ID         string      `json:"id"`
	Target     string      `json:"target"`
	Engine     core.Engine `json:"engine,omitempty"`
	Outcome    Outcome     `json:"outcome"`
	Kind       Kind        `json:"kind,omitempty"`
	Tier       string      `json:"tier,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at"`

	// Set on a successful run.
	Key            string     `json:"key,omitempty"`
	ManifestKey    string     `json:"manifest_key,omitempty"`
	GlobalsKey     string     `json:"globals_key,omitempty"`
	Bytes          int64      `json:"bytes,omitempty"`
	PlaintextBytes int64      `json:"plaintext_bytes,omitempty"`
	SHA256         string     `json:"sha256,omitempty"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	VerifyLevel    string     `json:"verify_level,omitempty"`
	VerifyOK       *bool      `json:"verify_ok,omitempty"`

	// Set on a failed run.
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
}

// Succeeded reports whether this entry describes a stored backup.
func (e Entry) Succeeded() bool { return e.Outcome == OutcomeSucceeded }

// Verified reports whether verification has confirmed this backup.
func (e Entry) Verified() bool { return e.VerifyOK != nil && *e.VerifyOK }

// Keys lists every object this backup owns, which is what a prune deletes.
func (e Entry) Keys() []string {
	keys := make([]string, 0, 3)
	for _, key := range []string{e.Key, e.ManifestKey, e.GlobalsKey} {
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// NewEntry summarizes a manifest for the index.
func NewEntry(m *Manifest, manifestKey string) Entry {
	e := Entry{
		ID:             m.ID,
		Target:         m.Target,
		Engine:         m.Engine,
		Outcome:        OutcomeSucceeded,
		Kind:           m.Kind,
		Tier:           m.Tier,
		StartedAt:      m.StartedAt,
		FinishedAt:     m.FinishedAt,
		Key:            m.Object.Key,
		ManifestKey:    manifestKey,
		Bytes:          m.Object.Bytes,
		PlaintextBytes: m.Plaintext.Bytes,
		SHA256:         m.Object.SHA256,
	}
	if m.Globals != nil {
		e.GlobalsKey = m.Globals.Key
	}
	if m.Verify != nil {
		ok := m.Verify.OK
		at := m.Verify.At
		e.VerifiedAt = &at
		e.VerifyLevel = m.Verify.Level
		e.VerifyOK = &ok
	}
	return e
}

// NewFailureEntry records a run that produced no backup.
func NewFailureEntry(target string, started, finished time.Time, phase, message string) Entry {
	return Entry{
		Target:     target,
		Outcome:    OutcomeFailed,
		StartedAt:  started,
		FinishedAt: finished,
		Phase:      phase,
		Error:      message,
	}
}

// AppendEntry returns the index with one entry appended. The index is written
// back whole under a conditional PUT: object stores have no append, so
// "append-only" is a discipline enforced by If-Match, not by the protocol.
func AppendEntry(index []byte, e Entry) ([]byte, error) {
	line, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encoding index entry: %w", err)
	}

	out := make([]byte, 0, len(index)+len(line)+1)
	out = append(out, index...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	return out, nil
}

// ParseIndex reads a JSONL index. A single corrupt line is reported rather
// than silently dropped: the index is a cache, but a cache that lies about
// which backups exist is worse than no cache.
func ParseIndex(b []byte) ([]Entry, error) {
	var entries []Entry

	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return entries, fmt.Errorf("index line %d is corrupt: %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("reading index: %w", err)
	}
	return entries, nil
}
