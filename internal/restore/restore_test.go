package restore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/restore"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

var at = time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)

// fakeRestorer stands in for pg_restore: it records what it was fed, and can
// misbehave the way a real client does.
type fakeRestorer struct {
	empty   bool
	fail    error
	partial bool

	received bytes.Buffer
	called   bool
}

func (f *fakeRestorer) IsEmpty(context.Context) (bool, error) { return f.empty, nil }

func (f *fakeRestorer) Restore(_ context.Context, r io.Reader) error {
	f.called = true

	if f.partial {
		// A client that stops early — an error mid-stream, a killed process.
		_, err := io.CopyN(&f.received, r, 128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return f.fail
	}

	if _, err := io.Copy(&f.received, r); err != nil {
		return err
	}
	return f.fail
}

func payload() []byte {
	return append([]byte("PGDMP"), bytes.Repeat([]byte("\x00public.users\n"), 2048)...)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stored puts a real backup in a bucket, through the real pipeline.
func stored(t *testing.T, store core.Store, identity *age.X25519Identity, data []byte) *manifest.Manifest {
	t.Helper()

	spec := pipeline.Spec{
		Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 1},
		Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}},
	}

	var object bytes.Buffer
	sums, err := pipeline.Run(t.Context(), spec,
		func(_ context.Context, w io.Writer) error { _, err := w.Write(data); return err },
		func(_ context.Context, r io.Reader) error { _, err := io.Copy(&object, r); return err },
	)
	require.NoError(t, err)

	const key = "prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.pgdump.zst.age"
	_, err = store.Put(t.Context(), key, bytes.NewReader(object.Bytes()), core.PutOptions{})
	require.NoError(t, err)

	return &manifest.Manifest{
		Schema:     manifest.Schema,
		ID:         manifest.NewID(at),
		Target:     "prod-pg",
		Engine:     core.EnginePostgres,
		FinishedAt: at,
		Object:     manifest.Object{Key: key, Bytes: sums.Ciphertext.Bytes, SHA256: sums.Ciphertext.SHA256},
		Plaintext:  manifest.Plaintext{Bytes: sums.Plaintext.Bytes, SHA256: sums.Plaintext.SHA256},
		Pipeline:   manifest.Pipeline{Compression: "zstd:1", Encryption: "age:x25519"},
	}
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return identity
}

func TestRestoreFeedsThePlaintextToTheClient(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	data := payload()
	m := stored(t, store, identity, data)

	restorer := &fakeRestorer{empty: true}
	runner := &restore.Runner{Store: store, Restorer: restorer, Now: func() time.Time { return at }}

	result, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})
	require.NoError(t, err)

	assert.True(t, bytes.Equal(data, restorer.received.Bytes()), "the client must receive the original dump")
	assert.Equal(t, int64(len(data)), result.Bytes)
	assert.Equal(t, sha256hex(data), result.SHA256)
	assert.True(t, result.Matched, "the restored stream matches the manifest")
}

// TestRestoreRefusesANonEmptyDestination is what stands between a restore and
// an overwritten production database.
func TestRestoreRefusesANonEmptyDestination(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	restorer := &fakeRestorer{empty: false}
	runner := &restore.Runner{Store: store, Restorer: restorer}

	_, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})

	require.ErrorIs(t, err, restore.ErrDestinationNotEmpty)
	assert.False(t, restorer.called, "nothing may be written to a destination that was refused")
}

func TestForceAllowsANonEmptyDestination(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	restorer := &fakeRestorer{empty: false}
	runner := &restore.Runner{Store: store, Restorer: restorer}

	_, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}, Force: true})

	require.NoError(t, err)
	assert.True(t, restorer.called)
}

func TestRestorePropagatesAClientFailure(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	wantErr := errors.New("pg_restore exited with code 1")
	runner := &restore.Runner{Store: store, Restorer: &fakeRestorer{empty: true, fail: wantErr}}

	_, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})

	require.ErrorIs(t, err, wantErr)
}

// TestRestoreCatchesACorruptedObject: the corruption surfaces as a read error
// inside the client, because age authenticates what it decrypts.
func TestRestoreCatchesACorruptedObject(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	object := bytes.Clone(store.Objects()[m.Object.Key])
	object[len(object)/2] ^= 0x20
	_, err := store.Put(t.Context(), m.Object.Key, bytes.NewReader(object), core.PutOptions{})
	require.NoError(t, err)

	runner := &restore.Runner{Store: store, Restorer: &fakeRestorer{empty: true}}

	_, err = runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})

	require.Error(t, err)
}

// TestRestoreReportsAShortRead: a client that stopped early leaves a database
// holding part of a backup, and saying "ok" to that would be the worst
// possible outcome.
func TestRestoreReportsAShortRead(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	runner := &restore.Runner{Store: store, Restorer: &fakeRestorer{empty: true, partial: true}}

	result, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not what was backed up")
	assert.False(t, result.Matched)
}

func TestRestoreNeedsTheKey(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())

	restorer := &fakeRestorer{empty: true}
	runner := &restore.Runner{Store: store, Restorer: restorer}

	_, err := runner.Run(t.Context(), m, restore.Spec{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--identity-file")
	assert.False(t, restorer.called)
}

func TestRestoreRejectsAPipelineItCannotRead(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m := stored(t, store, identity, payload())
	m.Pipeline.Compression = "brotli:5"

	runner := &restore.Runner{Store: store, Restorer: &fakeRestorer{empty: true}}

	_, err := runner.Run(t.Context(), m, restore.Spec{Identities: []age.Identity{identity}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot read")
}
