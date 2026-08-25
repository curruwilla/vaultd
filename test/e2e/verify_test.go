//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

// identityFile writes the private key to a file, which is how vaultd accepts
// it: never as a flag value, because argv is world-readable.
func identityFile(t *testing.T, identity *age.X25519Identity) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "key.age")
	require.NoError(t, os.WriteFile(path, []byte(identity.String()+"\n"), 0o600))
	return path
}

// latestBackup returns the id of the newest backup of a target.
func latestBackup(t *testing.T, store *s3.Store, indexKey string) manifest.Entry {
	t.Helper()

	entries, err := manifest.ParseIndex(download(t, store, indexKey))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	latest := entries[0]
	for _, entry := range entries[1:] {
		if entry.Succeeded() && entry.FinishedAt.After(latest.FinishedAt) {
			latest = entry
		}
	}
	require.True(t, latest.Succeeded())
	return latest
}

func TestVerifyIntegrityAndStructural(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-verify")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	// L0 costs one HEAD and no egress, so it needs no key at all.
	stdout, stderr, err := run(t, "verify", "--target", "prod-pg", "--latest", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "passed integrity verification")

	// L1 reads the object back in full, which is what needs the private key.
	stdout, stderr, err = run(t, "verify", "--target", "prod-pg", "--latest",
		"--level", "structural", "--identity-file", identityFile(t, identity), "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "passed structural verification")
	assert.Contains(t, stdout, "a pg_dump custom-format archive")

	// The outcome lands on the manifest and in the index, which is what
	// retention reads.
	store := newStore(t, "vaultd-e2e-verify")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")
	require.True(t, entry.Verified())
	assert.Equal(t, "structural", entry.VerifyLevel)

	m := fetchManifest(t, store, entry.ManifestKey)
	require.NotNil(t, m.Verify)
	assert.True(t, m.Verify.OK)

	stdout, _, err = run(t, "list", "prod-pg", "-c", configPath)
	require.NoError(t, err)
	assert.Contains(t, stdout, "structural ok")
}

// TestVerifyCatchesCorruption is the acceptance criterion for this milestone:
// flip a byte in the bucket and verification has to fail, loudly and with a
// non-zero exit code.
func TestVerifyCatchesCorruption(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-corruption")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-corruption")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")

	object := bytes.Clone(download(t, store, entry.Key))
	require.Greater(t, len(object), 1000)
	object[len(object)/2] ^= 0x40

	_, err = store.Put(t.Context(), entry.Key, bytes.NewReader(object), core.PutOptions{})
	require.NoError(t, err)

	// The size is unchanged, so the cheap check still passes: this is exactly
	// the corruption L0 cannot see.
	_, _, err = run(t, "verify", "--target", "prod-pg", "--latest", "-c", configPath)
	require.NoError(t, err)

	_, stderr, err = run(t, "verify", "--target", "prod-pg", "--latest",
		"--level", "structural", "--identity-file", identityFile(t, identity), "-c", configPath)

	require.Error(t, err, "a corrupted backup must fail verification")
	assert.Contains(t, stderr, "failed structural verification")

	// And the failure is recorded, so nothing later mistakes it for verified.
	entry = latestBackup(t, store, "e2e/_index/prod-pg.jsonl")
	require.NotNil(t, entry.VerifyOK)
	assert.False(t, *entry.VerifyOK)
	assert.False(t, entry.Verified())
}

// TestVerifiedBackupSurvivesPrune ties M3 and M4 together: the most recent
// verified backup is the only one anything has evidence restores, so retention
// keeps it whatever the tiers say (SPEC §7, invariant 2).
func TestVerifiedBackupSurvivesPrune(t *testing.T) {
	configPath := setupPrune(t, "vaultd-e2e-verified-prune", "1")
	_, identity := setupWith(t, "vaultd-e2e-verified-prune", pruneTemplate)

	// Two backups in the same day; `daily: {keep: 1}` would drop the older.
	for range 2 {
		_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
		require.NoError(t, err, "stderr: %s", stderr)
	}

	store := newStore(t, "vaultd-e2e-verified-prune")
	entries, err := manifest.ParseIndex(download(t, store, "e2e/_index/prod-pg.jsonl"))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	older := entries[0]

	_, stderr, err := run(t, "verify", older.ID,
		"--level", "structural", "--identity-file", identityFile(t, identity), "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	stdout, _, err := run(t, "prune", "prod-pg", "-c", configPath, "--apply")
	require.NoError(t, err)

	assert.Contains(t, stdout, "most recent verified backup")
	assert.Contains(t, stdout, "nothing to delete")

	_, err = store.Head(t.Context(), older.Key)
	assert.NoError(t, err, "the verified backup must still be there")
}

func TestRestoreIntoAFreshDatabase(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-restore")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")

	restoredDSN := createDatabase(t, "restored_here")

	stdout, stderr, err := run(t, "restore", entry.ID, "--to", restoredDSN, "--confirm",
		"--identity-file", identityFile(t, identity), "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	assert.Contains(t, stdout, "ok: restored prod-pg")
	assert.Contains(t, stdout, "matching the manifest")

	// The data is really there, and the excluded table came back empty —
	// exclude_table_data keeps the schema and drops the rows.
	assert.Equal(t, int64(2000), countRows(t, restoredDSN, "users"))
	assert.Equal(t, int64(5000), countRows(t, restoredDSN, "orders"))
	assert.Equal(t, int64(0), countRows(t, restoredDSN, "sessions"))
}

func TestRestoreNeedsConfirmation(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-restore-confirm")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore-confirm")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")
	restoredDSN := createDatabase(t, "restore_unconfirmed")

	_, _, err = run(t, "restore", entry.ID, "--to", restoredDSN,
		"--identity-file", identityFile(t, identity), "-c", configPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
	assert.Equal(t, int64(0), countTables(t, restoredDSN), "nothing may be written without confirmation")
}

// TestRestoreRefusesANonEmptyDatabase: restoring twice into the same place is
// how a careless operator overwrites something that mattered.
func TestRestoreRefusesANonEmptyDatabase(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-restore-nonempty")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore-nonempty")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")
	restoredDSN := createDatabase(t, "restore_twice")
	key := identityFile(t, identity)

	_, stderr, err = run(t, "restore", entry.ID, "--to", restoredDSN, "--confirm", "--identity-file", key, "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	_, _, err = run(t, "restore", entry.ID, "--to", restoredDSN, "--confirm", "--identity-file", key, "-c", configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")
	assert.Contains(t, err.Error(), "--force")

	// With --force and --clean it goes through, and the data is intact.
	_, stderr, err = run(t, "restore", entry.ID, "--to", restoredDSN, "--confirm", "--force", "--clean",
		"--identity-file", key, "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Equal(t, int64(2000), countRows(t, restoredDSN, "users"))
}

// TestRestoreRefusesToOverwriteAConfiguredTarget: pointing --to at a database
// this very config backs up is almost always a mistake, and never a silent
// one.
func TestRestoreRefusesToOverwriteAConfiguredTarget(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-restore-guard")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore-guard")
	entry := latestBackup(t, store, "e2e/_index/prod-pg.jsonl")

	_, _, err = run(t, "restore", entry.ID, "--to", env.pgDSN, "--confirm",
		"--identity-file", identityFile(t, identity), "-c", configPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "a database this config backs up")
	assert.Contains(t, err.Error(), "--force")
}

// createDatabase makes an empty database on the test server and returns a DSN
// pointing at it.
func createDatabase(t *testing.T, name string) string {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, env.pgDSN)
	require.NoError(t, err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)

	return replaceDatabase(env.pgDSN, name)
}

// replaceDatabase swaps the database in a postgres URL.
func replaceDatabase(dsn, name string) string {
	head, query, hasQuery := strings.Cut(dsn, "?")

	slash := strings.LastIndex(head, "/")
	out := head[:slash+1] + name
	if hasQuery {
		out += "?" + query
	}
	return out
}

func countRows(t *testing.T, dsn, table string) int64 {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	var rows int64
	require.NoError(t, conn.QueryRow(ctx, "select count(*) from "+pgx.Identifier{"public", table}.Sanitize()).Scan(&rows))
	return rows
}

func countTables(t *testing.T, dsn string) int64 {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	var tables int64
	require.NoError(t, conn.QueryRow(ctx,
		"select count(*) from information_schema.tables where table_schema = 'public'").Scan(&tables))
	return tables
}
