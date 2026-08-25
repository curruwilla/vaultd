package backup_test

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

	"github.com/curruwilla/vaultd/internal/backup"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

var at = time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)

// fakeDumper stands in for pg_dump: it produces known bytes, and can fail the
// way a real client fails.
type fakeDumper struct {
	info       core.ServerInfo
	data       []byte
	globals    []byte
	hasGlobals bool
	probeErr   error
	dumpErr    error
	stderr     string
	dumpDelay  time.Duration
}

func (f *fakeDumper) Probe(context.Context) (core.ServerInfo, error) {
	if f.probeErr != nil {
		return core.ServerInfo{}, f.probeErr
	}
	return f.info, nil
}

func (f *fakeDumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	if f.dumpDelay > 0 {
		select {
		case <-time.After(f.dumpDelay):
		case <-ctx.Done():
			return core.DumpResult{StderrTail: f.stderr}, ctx.Err()
		}
	}
	if _, err := w.Write(f.data); err != nil {
		return core.DumpResult{}, err
	}
	if f.dumpErr != nil {
		return core.DumpResult{StderrTail: f.stderr}, f.dumpErr
	}
	return core.DumpResult{
		Consistency:   core.ConsistencySerializableSnapshot,
		Tables:        f.info.Tables,
		DumperVersion: "pg_dump 17.2",
		StderrTail:    f.stderr,
	}, nil
}

func (f *fakeDumper) HasGlobals() bool { return f.hasGlobals }

func (f *fakeDumper) DumpGlobals(_ context.Context, w io.Writer) (core.DumpResult, error) {
	if _, err := w.Write(f.globals); err != nil {
		return core.DumpResult{}, err
	}
	return core.DumpResult{DumperVersion: "pg_dumpall 17.2"}, nil
}

func newDumper() *fakeDumper {
	return &fakeDumper{
		info: core.ServerInfo{
			Engine:      core.EnginePostgres,
			Version:     "17.2",
			VersionNum:  170002,
			Consistency: core.ConsistencySerializableSnapshot,
			Tables:      []core.TableInfo{{Name: "public.users", Rows: 42}},
		},
		data: bytes.Repeat([]byte("PGDMP fake dump payload\n"), 4096),
	}
}

func newSpec(t *testing.T, identity *age.X25519Identity) backup.Spec {
	t.Helper()

	return backup.Spec{
		Target: "prod-pg",
		Engine: core.EnginePostgres,
		Tier:   "daily",
		Layout: manifest.Layout{Prefix: "prod", Target: "prod-pg"},
		Pipeline: pipeline.Spec{
			Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 3},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}},
		},
	}
}

func newRunner(store core.Store, dumper core.Dumper) *backup.Runner {
	return &backup.Runner{Store: store, Dumper: dumper, Now: func() time.Time { return at }}
}

func TestRunStoresObjectAndManifest(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	store := memory.New()
	spec := newSpec(t, identity)

	m, err := newRunner(store, dumper).Run(t.Context(), spec)
	require.NoError(t, err)

	const dataKey = "prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.pgdump.zst.age"
	const manifestKey = "prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.manifest.json"

	objects := store.Objects()
	require.Contains(t, objects, dataKey)
	require.Contains(t, objects, manifestKey)

	// The manifest describes exactly what landed in the bucket.
	assert.Equal(t, dataKey, m.Object.Key)
	assert.Equal(t, int64(len(objects[dataKey])), m.Object.Bytes)
	assert.Equal(t, sha256hex(objects[dataKey]), m.Object.SHA256)
	assert.Equal(t, int64(len(dumper.data)), m.Plaintext.Bytes)
	assert.Equal(t, sha256hex(dumper.data), m.Plaintext.SHA256)

	assert.Equal(t, "prod-pg", m.Target)
	assert.Equal(t, core.EnginePostgres, m.Engine)
	assert.Equal(t, "17.2", m.ServerVersion)
	assert.Equal(t, "daily", m.Tier)
	assert.Equal(t, manifest.KindFull, m.Kind)
	assert.Equal(t, core.ConsistencySerializableSnapshot, m.Consistency)
	assert.Equal(t, "zstd:3", m.Pipeline.Compression)
	assert.Equal(t, "age:x25519", m.Pipeline.Encryption)
	assert.Equal(t, "pg_dump 17.2", m.Pipeline.Dumper)
	assert.Equal(t, manifest.Schema, m.Schema)
	assert.NotEmpty(t, m.ID)

	// The stored manifest parses and matches the returned one.
	stored, err := manifest.Unmarshal(objects[manifestKey])
	require.NoError(t, err)
	assert.Equal(t, m.ID, stored.ID)

	// And the object really is the dump: decrypt, decompress, compare.
	restored := readBack(t, spec.Pipeline, objects[dataKey], identity)
	assert.True(t, bytes.Equal(dumper.data, restored))
}

func TestRunStoresGlobals(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.hasGlobals = true
	dumper.globals = []byte("CREATE ROLE app;\n")

	store := memory.New()
	spec := newSpec(t, identity)

	m, err := newRunner(store, dumper).Run(t.Context(), spec)
	require.NoError(t, err)

	require.NotNil(t, m.Globals)
	const globalsKey = "prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-globals.sql.zst.age"
	assert.Equal(t, globalsKey, m.Globals.Key)

	objects := store.Objects()
	require.Contains(t, objects, globalsKey)
	assert.Equal(t, dumper.globals, readBack(t, spec.Pipeline, objects[globalsKey], identity))
}

func TestRunWithoutGlobals(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	store := memory.New()
	m, err := newRunner(store, newDumper()).Run(t.Context(), newSpec(t, identity))
	require.NoError(t, err)

	assert.Nil(t, m.Globals)
	assert.Len(t, store.Objects(), 2, "only the object and its manifest")
}

func TestRunReportsDumpFailure(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.dumpErr = errors.New("pg_dump exited with code 1")
	dumper.stderr = "pg_dump: error: query failed: ERROR:  permission denied for table users"

	store := memory.New()

	_, err = newRunner(store, dumper).Run(t.Context(), newSpec(t, identity))
	require.Error(t, err)

	var failure *backup.Error
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, backup.PhaseDump, failure.Phase)
	assert.Equal(t, "prod-pg", failure.Target)
	assert.Contains(t, failure.Stderr, "permission denied for table users")
	assert.Contains(t, failure.Error(), "backup of prod-pg failed during dump")

	// A failed dump must not leave a manifest behind: a manifest is a claim
	// that a restorable backup exists.
	for key := range store.Objects() {
		assert.False(t, manifest.IsManifestKey(key), "manifest written for a failed backup: %s", key)
	}
}

// TestRunRejectsAnEmptyDump covers a client that exits 0 having written
// nothing: without this check the bucket would hold a manifest promising a
// backup that restores to an empty database.
func TestRunRejectsAnEmptyDump(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.data = nil

	store := memory.New()

	_, err = newRunner(store, dumper).Run(t.Context(), newSpec(t, identity))

	var failure *backup.Error
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, backup.PhaseDump, failure.Phase)
	assert.Contains(t, failure.Error(), "produced no data")
	assert.Empty(t, store.Objects(), "the empty object should have been cleaned up")
}

func TestRunReportsUploadFailure(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	store := memory.New()
	store.FailPut = errors.New("503 from the bucket")

	_, err = newRunner(store, newDumper()).Run(t.Context(), newSpec(t, identity))

	var failure *backup.Error
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, backup.PhaseUpload, failure.Phase)
	assert.Empty(t, store.Objects())
}

func TestRunReportsProbeFailure(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.probeErr = errors.New("server is PG 17, need pg_dump >= 17, found 16.4")

	_, err = newRunner(memory.New(), dumper).Run(t.Context(), newSpec(t, identity))

	var failure *backup.Error
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, backup.PhaseProbe, failure.Phase)
	assert.Contains(t, failure.Error(), "need pg_dump >= 17")
}

func TestRunHonorsTimeout(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.dumpDelay = 5 * time.Second

	spec := newSpec(t, identity)
	spec.Timeout = 50 * time.Millisecond

	start := time.Now()
	_, err = newRunner(memory.New(), dumper).Run(t.Context(), spec)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "the timeout must cut the dump short")
}

func TestPlanWritesNothing(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	dumper := newDumper()
	dumper.hasGlobals = true

	store := memory.New()

	plan, err := newRunner(store, dumper).Plan(t.Context(), newSpec(t, identity))
	require.NoError(t, err)

	assert.Empty(t, store.Objects(), "a dry run must not touch the bucket")
	assert.Equal(t, "prod-pg", plan.Target)
	assert.Equal(t, "17.2", plan.ServerVersion)
	assert.Equal(t, 1, plan.Tables)
	assert.Equal(t, int64(42), plan.Rows)
	assert.Equal(t, "prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.pgdump.zst.age", plan.DataKey)
	assert.NotEmpty(t, plan.GlobalsKey)
	assert.Equal(t, "zstd:3", plan.Compression)
}

func readBack(t *testing.T, spec pipeline.Spec, object []byte, identity *age.X25519Identity) []byte {
	t.Helper()

	r, err := spec.Reader(bytes.NewReader(object), identity)
	require.NoError(t, err)
	defer r.Close()

	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestErrorUnwraps(t *testing.T) {
	inner := errors.New("boom")
	err := &backup.Error{Phase: backup.PhaseUpload, Target: "t", Err: inner}

	require.ErrorIs(t, err, inner)
	assert.Contains(t, err.Error(), "during upload")
}
