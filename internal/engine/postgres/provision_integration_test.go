//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/postgres"
)

const verifyPrefix = "vaultd_verify_"

func newProvisioner(t *testing.T) *postgres.Provisioner {
	t.Helper()

	// The administrative connection points at the maintenance database:
	// CREATE DATABASE cannot run from inside the database being created.
	provisioner, err := postgres.NewProvisioner(postgres.ProvisionOptions{
		DSN:    strings.Replace(serverDSN, "/app?", "/postgres?", 1),
		Prefix: verifyPrefix,
		BinDir: binDir,
	})
	require.NoError(t, err)
	return provisioner
}

func TestProvisionerProbeReadsTheStagingServer(t *testing.T) {
	info, err := newProvisioner(t).Probe(t.Context())

	require.NoError(t, err)
	assert.Equal(t, core.EnginePostgres, info.Engine)
	assert.NotEmpty(t, info.Version)
	assert.Greater(t, info.VersionNum, 140000)
}

// TestProvisionerCreatesAndDropsASandbox is the contract restore verification
// depends on: an empty database of its own, readable afterwards, and gone when
// the check is done (SPEC §8, decision D3).
func TestProvisionerCreatesAndDropsASandbox(t *testing.T) {
	provisioner := newProvisioner(t)
	name := verifyPrefix + "sandbox"

	// A leftover from an earlier failed run would make this test lie.
	require.NoError(t, provisioner.Drop(t.Context(), name))

	box, err := provisioner.Create(t.Context(), core.SandboxSpec{Name: name})
	require.NoError(t, err)
	assert.Equal(t, name, box.Name())

	empty, err := box.IsEmpty(t.Context())
	require.NoError(t, err)
	assert.True(t, empty, "a freshly created database holds nothing")

	names, err := provisioner.List(t.Context())
	require.NoError(t, err)
	assert.Contains(t, names, name)

	// Something a restore would have put there.
	seedSandbox(t, name)

	tables, err := box.Tables(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"public.invoices"}, tables)

	rows, err := box.CountRows(t.Context(), "public.invoices")
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows)

	// An assertion written without the schema means public, as it does
	// everywhere else in PostgreSQL.
	rows, err = box.CountRows(t.Context(), "invoices")
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows)

	value, err := box.Scalar(t.Context(), "select count(*) from invoices where total is null")
	require.NoError(t, err)
	assert.EqualValues(t, 1, value)

	require.NoError(t, box.Drop(t.Context()))

	names, err = provisioner.List(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, names, name)
}

// TestProvisionerRefusesADatabaseOutsideThePrefix is the guard that stands
// between a verification and a staging database somebody cares about.
func TestProvisionerRefusesADatabaseOutsideThePrefix(t *testing.T) {
	provisioner := newProvisioner(t)

	_, err := provisioner.Create(t.Context(), core.SandboxSpec{Name: "app"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	err = provisioner.Drop(t.Context(), "app")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	// And the database is still there.
	conn, err := pgx.Connect(t.Context(), serverDSN)
	require.NoError(t, err)
	defer conn.Close(context.Background())

	var tables int
	require.NoError(t, conn.QueryRow(t.Context(),
		"select count(*) from information_schema.tables where table_schema = 'public'").Scan(&tables))
	assert.Positive(t, tables)
}

func TestProvisionerNeedsAPrefix(t *testing.T) {
	_, err := postgres.NewProvisioner(postgres.ProvisionOptions{DSN: serverDSN})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "database_prefix")
}

// seedSandbox writes into a sandbox the way a restore would.
func seedSandbox(t *testing.T, database string) {
	t.Helper()

	conn, err := pgx.Connect(t.Context(), strings.Replace(serverDSN, "/app?", "/"+database+"?", 1))
	require.NoError(t, err)
	defer conn.Close(context.Background())

	_, err = conn.Exec(t.Context(), `
		create table invoices (id serial primary key, total numeric(10,2));
		insert into invoices (total) values (1.5), (2.5), (null);
	`)
	require.NoError(t, err)
}
