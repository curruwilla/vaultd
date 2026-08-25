package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDSNURL(t *testing.T) {
	info, err := parseDSN("postgres://backup:hunter2@pg.internal:5433/app?sslmode=require&connect_timeout=5")
	require.NoError(t, err)

	assert.Equal(t, "pg.internal", info.Host)
	assert.Equal(t, uint16(5433), info.Port)
	assert.Equal(t, "backup", info.User)
	assert.Equal(t, "hunter2", info.Password)
	assert.Equal(t, "app", info.Database)

	env := info.env()
	assert.Equal(t, "pg.internal", env["PGHOST"])
	assert.Equal(t, "5433", env["PGPORT"])
	assert.Equal(t, "hunter2", env["PGPASSWORD"])
	assert.Equal(t, "app", env["PGDATABASE"])
	// TLS settings have to reach the client, or it would connect differently
	// than the probe did.
	assert.Equal(t, "require", env["PGSSLMODE"])
	assert.Equal(t, "5", env["PGCONNECT_TIMEOUT"])
}

func TestParseDSNKeyValue(t *testing.T) {
	info, err := parseDSN("host=pg.internal port=5432 user=backup password=hunter2 dbname=app sslmode=verify-full")
	require.NoError(t, err)

	assert.Equal(t, "pg.internal", info.Host)
	assert.Equal(t, "app", info.Database)
	assert.Equal(t, "verify-full", info.env()["PGSSLMODE"])
}

func TestParseDSNRejectsGarbage(t *testing.T) {
	_, err := parseDSN("://nope")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "nope", "the connection string must not appear in the error")
}

func TestWithDatabase(t *testing.T) {
	info, err := parseDSN("postgres://backup@pg.internal:5432/app")
	require.NoError(t, err)

	maintenance := info.withDatabase("postgres")

	assert.Equal(t, "postgres", maintenance.Database)
	assert.Equal(t, "app", info.Database, "withDatabase must not mutate the original")
}
