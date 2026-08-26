// Package restore writes a stored backup back into a database.
//
// It is the reverse of the backup pipeline and just as streaming: the object
// is read from the bucket, decrypted, decompressed and handed straight to the
// engine's client, so a restore costs bandwidth rather than disk. On the way
// past, the plaintext is checksummed and compared with the manifest — a
// restore that applied cleanly but produced different bytes than were backed
// up is a failure worth shouting about.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
)

// Spec is one restore to perform.
type Spec struct {
	// Identities decrypt an age-encrypted backup.
	Identities []age.Identity
	// Passphrase decrypts a backup taken in passphrase mode.
	Passphrase string
	// Force allows a destination that already holds data.
	Force bool
	// Timeout bounds the whole restore. Zero means no limit beyond the context.
	Timeout time.Duration
}

// Result reports what was written.
type Result struct {
	Bytes    int64
	SHA256   string
	Duration time.Duration
	// Matched says whether what was restored is byte-for-byte what was backed
	// up.
	Matched bool
}

// Runner performs restores into one destination.
type Runner struct {
	Store    core.Store
	Restorer core.Restorer
	Now      func() time.Time
	Log      *slog.Logger
}

// ErrDestinationNotEmpty is returned when the destination already holds data
// and the caller has not insisted.
var ErrDestinationNotEmpty = errors.New("the destination database is not empty")

// ErrChecksumMismatch is returned when the restore applied but what reached
// the database is not what was backed up. It is a sentinel because restore
// verification has to tell it apart from a client that would not run at all:
// one is a broken backup, the other is a broken environment.
var ErrChecksumMismatch = errors.New("the restored stream does not match the manifest")

// Run restores the backup a manifest describes.
func (r *Runner) Run(ctx context.Context, m *manifest.Manifest, spec Spec) (Result, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	empty, err := r.Restorer.IsEmpty(ctx)
	if err != nil {
		return Result{}, err
	}
	if !empty && !spec.Force {
		return Result{}, ErrDestinationNotEmpty
	}

	readSpec, err := pipeline.ParseSpec(m.Pipeline.Compression, m.Pipeline.Encryption)
	if err != nil {
		return Result{}, err
	}
	readSpec.Encryption.Passphrase = spec.Passphrase

	switch readSpec.Encryption.Mode {
	case pipeline.ModeAge:
		if len(spec.Identities) == 0 {
			return Result{}, errors.New("this backup is age-encrypted; pass the private key with --identity-file")
		}
	case pipeline.ModePassphrase:
		if spec.Passphrase == "" {
			return Result{}, errors.New("this backup is encrypted with a passphrase; the config must supply it")
		}
	}

	started := r.now()
	log := r.log().With("target", m.Target, "backup", m.ID, "key", m.Object.Key)
	log.InfoContext(ctx, "restore started", "engine", string(m.Engine), "bytes", m.Object.Bytes)

	body, err := r.Store.Get(ctx, m.Object.Key)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	reader, err := readSpec.Reader(body, spec.Identities...)
	if err != nil {
		return Result{}, err
	}
	defer reader.Close()

	digest := sha256.New()
	counter := &countingWriter{}

	if err := r.Restorer.Restore(ctx, io.TeeReader(reader, io.MultiWriter(digest, counter))); err != nil {
		return Result{}, err
	}

	result := Result{
		Bytes:    counter.n,
		SHA256:   hex.EncodeToString(digest.Sum(nil)),
		Duration: r.now().Sub(started),
	}
	result.Matched = result.SHA256 == m.Plaintext.SHA256 && result.Bytes == m.Plaintext.Bytes

	if !result.Matched {
		return result, fmt.Errorf(
			"%w: the restore applied, but what was written is not what was backed up: %d bytes with checksum %s, against %d and %s in the manifest",
			ErrChecksumMismatch, result.Bytes, short(result.SHA256), m.Plaintext.Bytes, short(m.Plaintext.SHA256))
	}

	log.InfoContext(ctx, "restore finished", "bytes", result.Bytes, "duration_ms", result.Duration.Milliseconds())
	return result, nil
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12] + "…"
}
