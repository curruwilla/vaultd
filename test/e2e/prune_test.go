//go:build integration

package e2e_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

// pruneTemplate keeps a single day of backups, so three runs in a row leave
// two of them expendable — the shape a retention test needs without waiting a
// week for the calendar.
const pruneTemplate = `
version: 1

defaults:
  compression: { algo: zstd, level: 1 }
  encryption:  { mode: age, recipients: ["${E2E_AGE_RECIPIENT}"] }
  timeout: 5m

destinations:
  - name: bucket
    provider: ${E2E_PROVIDER}
    bucket: ${E2E_BUCKET}
    endpoint: ${E2E_ENDPOINT}
    region: ${E2E_REGION}
    access_key_id: ${E2E_ACCESS_KEY_ID}
    secret_access_key: ${E2E_SECRET_ACCESS_KEY}
    prefix: ${E2E_PREFIX}

targets:
  - name: prod-pg
    engine: postgres
    dsn: ${E2E_PG_DSN}
    destination: bucket
    retention:
      daily: { keep: 1 }
      min_keep: MIN_KEEP

  - name: broken-pg
    engine: postgres
    dsn: ${E2E_BROKEN_DSN}
    destination: bucket
    retention: { daily: { keep: 1 }, min_keep: 1 }
`

// setupPrune prepares a config whose retention is tight enough to prune.
func setupPrune(t *testing.T, bucket, minKeep string) string {
	t.Helper()

	// min_keep is a number, and ${VAR} references are expanded after parsing,
	// so this one is substituted in the text before the file is written.
	template := strings.ReplaceAll(pruneTemplate, "MIN_KEEP", minKeep)

	t.Setenv("E2E_BROKEN_DSN", "postgres://backup:s3cret@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	configPath, _ := setupWith(t, bucket, template)
	return configPath
}

// backupThrice takes three backups of the same target. They land in the same
// day, which is exactly what `daily: {keep: 1}` is meant to collapse.
func backupThrice(t *testing.T, configPath string) {
	t.Helper()

	for range 3 {
		_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
		require.NoError(t, err, "stderr: %s", stderr)
	}
}

func objectKeys(t *testing.T, store *s3.Store, prefix string) []string {
	t.Helper()

	var keys []string
	for object, err := range store.List(t.Context(), prefix) {
		require.NoError(t, err)
		keys = append(keys, object.Key)
	}
	return keys
}

func indexEntries(t *testing.T, store *s3.Store, key string) []manifest.Entry {
	t.Helper()

	entries, err := manifest.ParseIndex(download(t, store, key))
	require.NoError(t, err)
	return entries
}

// TestBackupWritesTheIndex: every run appends to the per-target index, and a
// listing is served from it rather than from one GET per manifest.
func TestBackupWritesTheIndex(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-index", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-index")

	entries := indexEntries(t, store, "e2e/_index/prod-pg.jsonl")
	require.Len(t, entries, 3)
	for _, entry := range entries {
		assert.True(t, entry.Succeeded())
		assert.Equal(t, core.EnginePostgres, entry.Engine)
		assert.NotEmpty(t, entry.Key)
		assert.NotEmpty(t, entry.ManifestKey)
	}

	stdout, stderr, err := run(t, "list", "prod-pg", "-c", configPath)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(stdout, "prod-pg"))
	assert.NotContains(t, stderr, "has no index")
}

// TestPruneDryRunChangesNothing is the default, and the promise the flag makes.
func TestPruneDryRunChangesNothing(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-prune-dry", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-prune-dry")
	before := objectKeys(t, store, "e2e/prod-pg/")

	stdout, stderr, err := run(t, "prune", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	assert.Contains(t, stdout, "dry run")
	assert.Contains(t, stdout, "2 backups")
	assert.Contains(t, stdout, "--apply")
	assert.Equal(t, before, objectKeys(t, store, "e2e/prod-pg/"), "a dry run must not touch the bucket")
}

func TestPruneApplyDeletesTheExpiredBackups(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-prune-apply", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-prune-apply")
	before := objectKeys(t, store, "e2e/prod-pg/")
	require.Len(t, before, 6, "three backups, each an object and a manifest")

	stdout, stderr, err := run(t, "prune", "prod-pg", "-c", configPath, "--apply")
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 2 backups")

	after := objectKeys(t, store, "e2e/prod-pg/")
	assert.Len(t, after, 2, "only the newest backup and its manifest survive")

	// The index now describes exactly what is left, and the survivor is the
	// newest one.
	entries := indexEntries(t, store, "e2e/_index/prod-pg.jsonl")
	require.Len(t, entries, 1)
	assert.Contains(t, after, entries[0].Key)
	assert.Contains(t, after, entries[0].ManifestKey)

	// And the objects really are gone, not just unlisted.
	for _, key := range before {
		if key == entries[0].Key || key == entries[0].ManifestKey {
			continue
		}
		_, err := store.Head(t.Context(), key)
		assert.ErrorIs(t, err, core.ErrNotFound, "%s should have been deleted", key)
	}
}

// TestPruneRespectsMinKeep: the floor overrides the tiers, always.
func TestPruneRespectsMinKeep(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-prune-floor", "3")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-prune-floor")

	stdout, stderr, err := run(t, "prune", "prod-pg", "-c", configPath, "--apply")
	require.NoError(t, err, "stderr: %s", stderr)

	assert.Contains(t, stdout, "min_keep floor of 3")
	assert.Contains(t, stdout, "nothing to delete")
	assert.Len(t, objectKeys(t, store, "e2e/prod-pg/"), 6)
}

// TestPruneIsFrozenAfterAFailedRun: a backup that just failed is exactly when
// the old ones matter most (SPEC §7, invariant 3).
func TestPruneIsFrozenAfterAFailedRun(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-prune-frozen", "1")

	// Three good backups of the same target, then a run that fails.
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-prune-frozen")
	before := objectKeys(t, store, "e2e/prod-pg/")

	// Point the good target at an unreachable server for one run.
	t.Setenv("E2E_PG_DSN", "postgres://backup:s3cret@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")
	_, _, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.Error(t, err, "the run was supposed to fail")

	entries := indexEntries(t, store, "e2e/_index/prod-pg.jsonl")
	require.Len(t, entries, 4)
	assert.False(t, entries[3].Succeeded())
	assert.Equal(t, "probe", entries[3].Phase)

	stdout, stderr, err := run(t, "prune", "prod-pg", "-c", configPath, "--apply")
	require.NoError(t, err)

	assert.Contains(t, stderr, "the most recent run of this target failed")
	assert.Contains(t, stdout, "nothing to delete")
	assert.Equal(t, before, objectKeys(t, store, "e2e/prod-pg/"), "a frozen prune deletes nothing")
}

// TestListShowsFailedRuns: a failure has to be visible, not merely absent.
func TestListShowsFailedRuns(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-list-failures", "1")

	_, _, err := run(t, "backup", "broken-pg", "-c", configPath)
	require.Error(t, err)

	stdout, _, err := run(t, "list", "broken-pg", "-c", configPath)
	require.NoError(t, err)

	assert.Contains(t, stdout, "FAILED (probe)")
}

func TestReindexRebuildsALostIndex(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-reindex", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-reindex")
	const indexKey = "e2e/_index/prod-pg.jsonl"

	require.NoError(t, store.Delete(t.Context(), []string{indexKey}))

	// Without an index, a listing still works — it reads the manifests — and
	// says what to do about it.
	stdout, stderr, err := run(t, "list", "prod-pg", "-c", configPath)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(stdout, "prod-pg"))
	assert.Contains(t, stderr, "vaultd reindex")

	stdout, _, err = run(t, "reindex", "prod-pg", "-c", configPath)
	require.NoError(t, err)
	assert.Contains(t, stdout, "rebuilt")
	assert.Contains(t, stdout, "3 manifests")

	entries := indexEntries(t, store, indexKey)
	require.Len(t, entries, 3)
	assert.True(t, entries[0].FinishedAt.Before(entries[2].FinishedAt), "oldest first")
}

// TestPruneOrphans: objects with no manifest are only ever removed on an
// explicit request (SPEC §7, invariant 4).
func TestPruneOrphans(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-orphans", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-orphans")

	// The residue of an interrupted run: an object nothing refers to.
	const stray = "e2e/prod-pg/2026/01/01/prod-pg-20260101T030000Z-full.pgdump.zst.age"
	_, err := store.Put(t.Context(), stray, bytes.NewReader([]byte("interrupted")), core.PutOptions{})
	require.NoError(t, err)

	// A plain prune leaves it alone, however old it looks.
	stdout, _, err := run(t, "prune", "prod-pg", "-c", configPath, "--apply")
	require.NoError(t, err)
	assert.NotContains(t, stdout, stray)
	_, err = store.Head(t.Context(), stray)
	require.NoError(t, err, "prune must not delete an orphan without being asked")

	// Asking reports it, and only --apply removes it. The grace period is
	// zeroed here because the object was written seconds ago.
	stdout, _, err = run(t, "prune", "prod-pg", "-c", configPath, "--orphans", "--orphan-grace=0")
	require.NoError(t, err)
	assert.Contains(t, stdout, stray)
	_, err = store.Head(t.Context(), stray)
	require.NoError(t, err, "a dry run must not delete it either")

	_, _, err = run(t, "prune", "prod-pg", "-c", configPath, "--orphans", "--orphan-grace=0", "--apply")
	require.NoError(t, err)

	_, err = store.Head(t.Context(), stray)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

// TestOrphanGraceProtectsAnInFlightUpload: a backup that is still uploading
// has an object and no manifest yet, and must not be swept away.
func TestOrphanGraceProtectsAnInFlightUpload(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-orphan-grace", "1")
	backupThrice(t, configPath)

	store := newStore(t, "vaultd-e2e-orphan-grace")

	const inFlight = "e2e/prod-pg/2026/08/24/prod-pg-20260824T030000Z-full.pgdump.zst.age"
	_, err := store.Put(t.Context(), inFlight, bytes.NewReader([]byte("still uploading")), core.PutOptions{})
	require.NoError(t, err)

	stdout, _, err := run(t, "prune", "prod-pg", "-c", configPath, "--orphans", "--apply")
	require.NoError(t, err)

	assert.Contains(t, stdout, "no orphaned objects")
	_, err = store.Head(t.Context(), inFlight)
	assert.NoError(t, err, "an object written moments ago is not an orphan")
}
