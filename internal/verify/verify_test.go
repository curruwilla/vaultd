package verify_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/storage/memory"
	"github.com/curruwilla/vaultd/internal/verify"
)

var at = time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)

func layout() manifest.Layout { return manifest.Layout{Prefix: "prod", Target: "prod-pg"} }

// pgDump is a plausible custom-format archive: the magic pg_dump writes,
// followed by enough bytes to compress.
func pgDump() []byte {
	return append([]byte("PGDMP"), bytes.Repeat([]byte("\x00table public.users\n"), 4096)...)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stored puts a real backup in a bucket: the payload goes through the actual
// pipeline, so what the verifier reads back is what a backup would have left.
func stored(t *testing.T, store core.Store, identity *age.X25519Identity, payload []byte) (*manifest.Manifest, string) {
	t.Helper()

	spec := pipeline.Spec{
		Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 1},
		Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}},
	}

	var object bytes.Buffer
	sums, err := pipeline.Run(t.Context(), spec,
		func(_ context.Context, w io.Writer) error { _, err := w.Write(payload); return err },
		func(_ context.Context, r io.Reader) error { _, err := io.Copy(&object, r); return err },
	)
	require.NoError(t, err)

	dataKey := layout().Data(at, manifest.KindFull, ".pgdump.zst.age")
	manifestKey := layout().Manifest(at, manifest.KindFull)

	_, err = store.Put(t.Context(), dataKey, bytes.NewReader(object.Bytes()), core.PutOptions{})
	require.NoError(t, err)

	m := &manifest.Manifest{
		Schema:        manifest.Schema,
		ID:            manifest.NewID(at),
		Target:        "prod-pg",
		Engine:        core.EnginePostgres,
		ServerVersion: "17.2",
		StartedAt:     at,
		FinishedAt:    at.Add(time.Minute),
		Kind:          manifest.KindFull,
		Tier:          "daily",
		Object:        manifest.Object{Key: dataKey, Bytes: sums.Ciphertext.Bytes, SHA256: sums.Ciphertext.SHA256},
		Plaintext:     manifest.Plaintext{Bytes: sums.Plaintext.Bytes, SHA256: sums.Plaintext.SHA256},
		Pipeline:      manifest.Pipeline{Compression: "zstd:1", Encryption: "age:x25519", Dumper: "pg_dump 17.2"},
		Consistency:   core.ConsistencySerializableSnapshot,
	}

	raw, err := m.Marshal()
	require.NoError(t, err)
	_, err = store.Put(t.Context(), manifestKey, bytes.NewReader(raw), core.PutOptions{})
	require.NoError(t, err)

	return m, manifestKey
}

func verifier(t *testing.T, store *memory.Store, identity *age.X25519Identity) *verify.Verifier {
	t.Helper()

	return &verify.Verifier{
		Store:      store,
		Index:      index.New(store, layout()),
		Identities: []age.Identity{identity},
		Now:        func() time.Time { return at },
	}
}

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return identity
}

func TestIntegrityPasses(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelIntegrity)

	require.NoError(t, err)
	assert.True(t, result.OK)
	assert.Empty(t, result.Problems)
	assert.Equal(t, verify.LevelIntegrity, result.Level)
}

func TestIntegrityCatchesAMissingObject(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	require.NoError(t, store.Delete(t.Context(), []string{m.Object.Key}))

	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelIntegrity)

	require.NoError(t, err, "a missing backup is a finding, not a tool failure")
	assert.False(t, result.OK)
	assert.Contains(t, result.Problems[0], "missing from the bucket")
}

func TestIntegrityCatchesAWrongSize(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	// Something replaced the object with a shorter one.
	_, err := store.Put(t.Context(), m.Object.Key, bytes.NewReader([]byte("truncated")), core.PutOptions{})
	require.NoError(t, err)

	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelIntegrity)

	require.NoError(t, err)
	assert.False(t, result.OK)
	assert.Contains(t, result.Problems[0], "the manifest records")
}

func TestStructuralPasses(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	payload := pgDump()
	m, _ := stored(t, store, identity, payload)

	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelStructural)

	require.NoError(t, err)
	assert.True(t, result.OK, "problems: %v", result.Problems)
	assert.Equal(t, int64(len(payload)), result.Details["plaintext_bytes"])
	assert.Equal(t, sha256hex(payload), result.Details["plaintext_sha256"])
	assert.Equal(t, "a pg_dump custom-format archive", result.Details["format"])
}

// TestStructuralCatchesCorruption is the acceptance criterion for this
// milestone: flip a byte in the bucket, and verification has to say so. The
// object is encrypted, so a flipped byte breaks authentication before it can
// ever reach a checksum comparison — which is exactly the point.
func TestStructuralCatchesCorruption(t *testing.T) {
	for _, offset := range []string{"in the header", "in the middle", "at the end"} {
		t.Run(offset, func(t *testing.T) {
			store := memory.New()
			identity := newIdentity(t)
			m, _ := stored(t, store, identity, pgDump())

			object := store.Objects()[m.Object.Key]
			require.NotEmpty(t, object)

			corrupted := bytes.Clone(object)
			switch offset {
			case "in the header":
				corrupted[8] ^= 0x01
			case "in the middle":
				corrupted[len(corrupted)/2] ^= 0x40
			case "at the end":
				corrupted[len(corrupted)-1] ^= 0x80
			}
			require.NotEqual(t, object, corrupted)

			_, err := store.Put(t.Context(), m.Object.Key, bytes.NewReader(corrupted), core.PutOptions{})
			require.NoError(t, err)

			result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelStructural)

			require.NoError(t, err, "corruption is a finding, not a tool failure")
			assert.False(t, result.OK, "a flipped byte must fail verification")
			assert.NotEmpty(t, result.Problems)
		})
	}
}

func TestStructuralCatchesTruncation(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	object := store.Objects()[m.Object.Key]
	truncated := object[:len(object)-64]

	_, err := store.Put(t.Context(), m.Object.Key, bytes.NewReader(truncated), core.PutOptions{})
	require.NoError(t, err)

	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelStructural)

	require.NoError(t, err)
	assert.False(t, result.OK)
	assert.NotEmpty(t, result.Problems)
}

// TestStructuralCatchesTheWrongContents covers the case the checksums alone
// would not explain: the object decrypts cleanly but is not a dump.
func TestStructuralCatchesTheWrongContents(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, []byte("this is not a pg_dump archive at all"))

	// The manifest is honest about the bytes; only the format is wrong.
	result, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelStructural)

	require.NoError(t, err)
	assert.False(t, result.OK)
	assert.Contains(t, result.Problems[0], "does not start like a pg_dump custom-format archive")
}

// TestStructuralNeedsTheKey: without the private key the backup cannot be
// judged at all, and reporting it as broken would be a lie.
func TestStructuralNeedsTheKey(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	keyless := &verify.Verifier{Store: store, Index: index.New(store, layout()), Now: func() time.Time { return at }}

	_, err := keyless.Manifest(t.Context(), m, verify.LevelStructural)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--identity-file")
}

// TestStructuralWithTheWrongKeyIsNotAFinding: a key that matches nothing says
// something about the invocation, not about the backup.
func TestStructuralWithTheWrongKeyFails(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	wrong := &verify.Verifier{
		Store:      store,
		Index:      index.New(store, layout()),
		Identities: []age.Identity{newIdentity(t)},
		Now:        func() time.Time { return at },
	}

	_, err := wrong.Manifest(t.Context(), m, verify.LevelStructural)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of the supplied identities")
}

// TestBackupRecordsTheOutcome: the result has to land on the manifest and in
// the index, because that is what stops prune from deleting the most recent
// verified backup.
func TestBackupRecordsTheOutcome(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, manifestKey := stored(t, store, identity, pgDump())

	idx := index.New(store, layout())
	require.NoError(t, idx.Append(t.Context(), manifest.NewEntry(m, manifestKey)))

	entry := manifest.NewEntry(m, manifestKey)
	result, err := verifier(t, store, identity).Backup(t.Context(), entry, verify.LevelStructural)
	require.NoError(t, err)
	require.True(t, result.OK, "problems: %v", result.Problems)

	stored := storedManifest(t, store, manifestKey)
	require.NotNil(t, stored.Verify)
	assert.True(t, stored.Verify.OK)
	assert.Equal(t, "structural", stored.Verify.Level)
	assert.Equal(t, at, stored.Verify.At.UTC())

	loaded, err := idx.Load(t.Context())
	require.NoError(t, err)
	require.Len(t, loaded.Entries, 1)
	assert.True(t, loaded.Entries[0].Verified())
	assert.Equal(t, "structural", loaded.Entries[0].VerifyLevel)
}

func TestBackupRecordsAFailure(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, manifestKey := stored(t, store, identity, pgDump())

	idx := index.New(store, layout())
	require.NoError(t, idx.Append(t.Context(), manifest.NewEntry(m, manifestKey)))

	require.NoError(t, store.Delete(t.Context(), []string{m.Object.Key}))

	result, err := verifier(t, store, identity).Backup(t.Context(), manifest.NewEntry(m, manifestKey), verify.LevelIntegrity)
	require.NoError(t, err)
	assert.False(t, result.OK)

	loaded, err := idx.Load(t.Context())
	require.NoError(t, err)
	require.NotNil(t, loaded.Entries[0].VerifyOK)
	assert.False(t, *loaded.Entries[0].VerifyOK, "a failed verification must be recorded as such")
	assert.False(t, loaded.Entries[0].Verified())
}

func storedManifest(t *testing.T, store *memory.Store, key string) *manifest.Manifest {
	t.Helper()

	m, err := manifest.Unmarshal(store.Objects()[key])
	require.NoError(t, err)
	return m
}
