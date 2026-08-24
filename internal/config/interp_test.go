package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/config"
)

func TestInterpolation(t *testing.T) {
	env := map[string]string{
		"PG_DSN":   "postgres://backup@pg:5432/app",
		"BUCKET":   "db-backups",
		"ACCOUNT":  "acc123",
		"EMPTYVAR": "",
	}

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "r2_secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("file-secret\n"), 0o600))

	yaml := strings.NewReplacer(
		"bucket: db-backups", "bucket: ${BUCKET}",
		"endpoint: https://acc.r2.cloudflarestorage.com", "endpoint: https://${ACCOUNT}.r2.cloudflarestorage.com",
		"secret_access_key: s3cret-value", "secret_access_key: ${file:"+secretFile+"}",
		"dsn: postgres://backup@pg:5432/app", "dsn: ${PG_DSN}",
		"prefix: prod", "prefix: ${PREFIX:-fallback}",
	).Replace(baseYAML)

	cfg, diags, err := config.Parse([]byte(yaml), config.LoadOptions{
		Lookup: func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)

	assert.Equal(t, "db-backups", cfg.Destinations[0].Bucket)
	assert.Equal(t, "https://acc123.r2.cloudflarestorage.com", cfg.Destinations[0].Endpoint)
	// A trailing newline in a secret file is stripped: it would otherwise
	// travel into an HTTP header or a DSN.
	assert.Equal(t, "file-secret", cfg.Destinations[0].SecretAccessKey.Reveal())
	assert.Equal(t, "postgres://backup@pg:5432/app", cfg.Targets[0].DSN.Reveal())
}

func TestInterpolationDefaultValue(t *testing.T) {
	yaml := strings.Replace(baseYAML, "bucket: db-backups", "bucket: ${BUCKET:-fallback-bucket}", 1)

	cfg, _ := parse(t, yaml)

	assert.Equal(t, "fallback-bucket", cfg.Destinations[0].Bucket)
}

func TestInterpolationUnreadableFile(t *testing.T) {
	yaml := strings.Replace(baseYAML, "secret_access_key: s3cret-value", "secret_access_key: ${file:/nope/missing}", 1)

	_, diags := parse(t, yaml)

	require.True(t, diags.HasErrors())
	assert.Contains(t, render(diags), "${file:/nope/missing} cannot be read")
}

// TestAllowUnsetEnv covers validating a config where the secrets are not
// present — a CI check on a pull request, for instance.
func TestAllowUnsetEnv(t *testing.T) {
	yaml := strings.Replace(baseYAML, "secret_access_key: s3cret-value", "secret_access_key: ${R2_SECRET}", 1)

	cfg, diags, err := config.Parse([]byte(yaml), config.LoadOptions{
		AllowUnsetEnv: true,
		Lookup:        func(string) (string, bool) { return "", false },
	})
	require.NoError(t, err)

	assert.Contains(t, render(diags), "substituted with an empty string")
	assert.False(t, cfg.Destinations[0].SecretAccessKey.Set())
	// The credential pair is now half-set, which is itself an error worth
	// reporting rather than a silent pass.
	assert.True(t, diags.HasErrors())
}

func TestInterpolationLeavesPlainStringsAlone(t *testing.T) {
	cfg, _ := parse(t, baseYAML)

	assert.Equal(t, "0 3 * * *", cfg.Targets[0].Schedule)
}
