// Package manifest defines the metadata document written next to every backup
// and the object key layout it lives in. The bucket is the source of truth
// (SPEC §6): a manifest is enough to find, verify and restore a backup with no
// local state at all.
package manifest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/curruwilla/vaultd/internal/core"
)

// Schema is the version of the manifest document this build writes. Readers
// must refuse a schema they do not understand rather than guess.
const Schema = 1

// Kind distinguishes a full dump from the incremental kinds planned for v2.
type Kind string

const KindFull Kind = "full"

// Manifest describes one backup. It is stored unencrypted beside the data
// object: it holds no secrets, and a restore has to be able to read it before
// it can decrypt anything.
type Manifest struct {
	Schema        int              `json:"schema"`
	ID            string           `json:"id"`
	Target        string           `json:"target"`
	Engine        core.Engine      `json:"engine"`
	ServerVersion string           `json:"server_version"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	DurationMS    int64            `json:"duration_ms"`
	Kind          Kind             `json:"kind"`
	Tier          string           `json:"tier"`
	Object        Object           `json:"object"`
	Plaintext     Plaintext        `json:"plaintext"`
	Globals       *Object          `json:"globals,omitempty"`
	Pipeline      Pipeline         `json:"pipeline"`
	Consistency   core.Consistency `json:"consistency"`
	Binlog        *string          `json:"binlog"`
	OplogEnd      *string          `json:"oplog_end"`
	Tables        []core.TableInfo `json:"tables"`
	Verify        *Verify          `json:"verify"`
	Warnings      []string         `json:"warnings,omitempty"`
	VaultdVersion string           `json:"vaultd_version"`
}

// Object is what actually sits in the bucket: the ciphertext, and the checksum
// of the ciphertext, so integrity can be checked without decrypting anything.
type Object struct {
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Plaintext is what the database produced, before compression and encryption.
// Its checksum is what a restore verifies against.
type Plaintext struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Pipeline records how the object was produced, in the order the stages ran.
type Pipeline struct {
	Compression string `json:"compression"`
	Encryption  string `json:"encryption"`
	Dumper      string `json:"dumper"`
}

// Verify is the outcome of the last verification, written back onto the
// manifest by `vaultd verify` (SPEC §8).
type Verify struct {
	Level   string         `json:"level"`
	At      time.Time      `json:"at"`
	OK      bool           `json:"ok"`
	Details map[string]any `json:"details,omitempty"`
}

// NewID returns a fresh ULID: sortable by time, which makes a listing of ids
// chronological on its own.
func NewID(at time.Time) string {
	return ulid.MustNew(ulid.Timestamp(at), rand.Reader).String()
}

// Marshal renders a manifest for storage, indented so a human reading it out
// of the bucket does not need a formatter.
func (m *Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// Unmarshal parses a manifest and refuses a schema this build cannot read.
func Unmarshal(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("manifest schema %d is not supported by this build (expected %d)", m.Schema, Schema)
	}
	return &m, nil
}

// Age reports how old the backup is at time now.
func (m *Manifest) Age(now time.Time) time.Duration { return now.Sub(m.FinishedAt) }
