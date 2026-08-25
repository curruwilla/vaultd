package manifest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Entry is one line of the per-target index: everything a listing needs
// without fetching every manifest. The index is a cache — the manifests in the
// bucket remain the source of truth, and `vaultd reindex` rebuilds it.
type Entry struct {
	ID             string     `json:"id"`
	Target         string     `json:"target"`
	Kind           Kind       `json:"kind"`
	Tier           string     `json:"tier"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     time.Time  `json:"finished_at"`
	Key            string     `json:"key"`
	ManifestKey    string     `json:"manifest_key"`
	Bytes          int64      `json:"bytes"`
	PlaintextBytes int64      `json:"plaintext_bytes"`
	SHA256         string     `json:"sha256"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	VerifyLevel    string     `json:"verify_level,omitempty"`
	VerifyOK       *bool      `json:"verify_ok,omitempty"`
}

// NewEntry summarizes a manifest for the index.
func NewEntry(m *Manifest, manifestKey string) Entry {
	e := Entry{
		ID:             m.ID,
		Target:         m.Target,
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
	if m.Verify != nil {
		ok := m.Verify.OK
		at := m.Verify.At
		e.VerifiedAt = &at
		e.VerifyLevel = m.Verify.Level
		e.VerifyOK = &ok
	}
	return e
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
