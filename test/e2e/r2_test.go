//go:build integration

package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

// TestBackupToRealR2 runs the same backup against a real Cloudflare R2 bucket.
//
// MinIO is faithful to the protocol but not to the provider: R2's checksum
// handling, conditional writes and multipart behaviour are what actually bite
// in production, and the SPEC makes a real R2 run part of M1's acceptance.
// The test is skipped unless credentials are present, so it costs nothing
// locally and runs from the nightly pipeline.
//
// Required environment:
//
//	VAULTD_E2E_R2_ENDPOINT           https://<account>.r2.cloudflarestorage.com
//	VAULTD_E2E_R2_BUCKET             a bucket dedicated to tests
//	VAULTD_E2E_R2_ACCESS_KEY_ID
//	VAULTD_E2E_R2_SECRET_ACCESS_KEY
func TestBackupToRealR2(t *testing.T) {
	endpoint := os.Getenv("VAULTD_E2E_R2_ENDPOINT")
	bucket := os.Getenv("VAULTD_E2E_R2_BUCKET")
	accessKey := os.Getenv("VAULTD_E2E_R2_ACCESS_KEY_ID")
	secretKey := os.Getenv("VAULTD_E2E_R2_SECRET_ACCESS_KEY")

	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		t.Skip("no R2 credentials in the environment; set VAULTD_E2E_R2_* to run this")
	}
	if !env.pgClientOK {
		t.Skip("no pg_dump on this host")
	}

	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	// A prefix per run keeps concurrent pipelines from colliding, and makes
	// cleanup a single subtree delete.
	prefix := fmt.Sprintf("e2e/%d", time.Now().UTC().UnixNano())

	store, err := s3.New(t.Context(), s3.Config{
		Provider:        s3.ProviderR2,
		Bucket:          bucket,
		Endpoint:        endpoint,
		Region:          "auto",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	})
	require.NoError(t, err)

	// Cleanup runs even when the assertions fail: a test bucket that
	// accumulates dumps is both a cost and a liability.
	t.Cleanup(func() { cleanup(t, store, prefix) })

	t.Setenv("E2E_AGE_RECIPIENT", identity.Recipient().String())
	t.Setenv("E2E_PROVIDER", "r2")
	t.Setenv("E2E_BUCKET", bucket)
	t.Setenv("E2E_ENDPOINT", endpoint)
	t.Setenv("E2E_REGION", "auto")
	t.Setenv("E2E_ACCESS_KEY_ID", accessKey)
	t.Setenv("E2E_SECRET_ACCESS_KEY", secretKey)
	t.Setenv("E2E_PG_DSN", env.pgDSN)
	t.Setenv("E2E_PREFIX", prefix)

	configPath := filepath.Join(t.TempDir(), "vaultd.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(configTemplate), 0o600))

	stdout, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ok: prod-pg backed up")

	var dataKey, manifestKey string
	for object, err := range store.List(t.Context(), prefix+"/prod-pg/") {
		require.NoError(t, err)
		if manifest.IsManifestKey(object.Key) {
			manifestKey = object.Key
		} else if !strings.Contains(object.Key, "-globals.") {
			dataKey = object.Key
		}
	}
	require.NotEmpty(t, dataKey)
	require.NotEmpty(t, manifestKey)

	m := fetchManifest(t, store, manifestKey)
	assert.Equal(t, core.EnginePostgres, m.Engine)

	stored := download(t, store, dataKey)
	assert.Equal(t, m.Object.SHA256, sha256hex(stored), "R2 stored different bytes than we uploaded")
	assert.Equal(t, m.Object.Bytes, int64(len(stored)))

	plaintext := decrypt(t, stored, identity)
	assert.Equal(t, m.Plaintext.SHA256, sha256hex(plaintext))
	assert.True(t, bytes.HasPrefix(plaintext, []byte("PGDMP")))

	// The lock primitive depends on R2 honouring If-None-Match.
	lockKey := prefix + "/_locks/prod-pg.lock"
	created, err := store.PutIfAbsent(t.Context(), lockKey, []byte(`{"holder":"e2e"}`))
	require.NoError(t, err)
	assert.True(t, created)

	created, err = store.PutIfAbsent(t.Context(), lockKey, []byte(`{"holder":"other"}`))
	require.NoError(t, err)
	assert.False(t, created, "R2 accepted a conditional write that should have failed")
}

func cleanup(t *testing.T, store *s3.Store, prefix string) {
	t.Helper()

	var keys []string
	for object, err := range store.List(t.Context(), prefix) {
		if err != nil {
			t.Logf("cleanup listing failed: %v", err)
			return
		}
		keys = append(keys, object.Key)
	}
	if len(keys) == 0 {
		return
	}
	if err := store.Delete(t.Context(), keys); err != nil {
		t.Logf("cleanup delete failed, %d objects may remain under %s: %v", len(keys), prefix, err)
	}
}
