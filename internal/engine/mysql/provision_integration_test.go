//go:build integration

package mysql_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mysql"
)

const verifyPrefix = "vaultd_verify_"

func newProvisioner(t *testing.T, dsn string, flavor mysql.Flavor) *mysql.Provisioner {
	t.Helper()

	// Creating and dropping databases is what a verify target's credential is
	// for; the backup user deliberately cannot.
	provisioner, err := mysql.NewProvisioner(mysql.ProvisionOptions{
		DSN:    rootDSN(dsn),
		Flavor: flavor,
		Prefix: verifyPrefix,
	})
	require.NoError(t, err)
	return provisioner
}

func TestProvisionerProbeReadsTheStagingServer(t *testing.T) {
	info, err := newProvisioner(t, mysqlDSN, mysql.FlavorMySQL).Probe(t.Context())

	require.NoError(t, err)
	assert.Equal(t, core.EngineMySQL, info.Engine)
	assert.NotEmpty(t, info.Version)
}

// TestProvisionerProbeRefusesTheOtherFork: the dump was written by one fork's
// client, and the two diverge exactly where a restore breaks.
func TestProvisionerProbeRefusesTheOtherFork(t *testing.T) {
	_, err := newProvisioner(t, mariadbDSN, mysql.FlavorMySQL).Probe(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server reports mariadb")
}

// TestProvisionerCreatesAndDropsASandbox is the contract restore verification
// depends on: an empty database of its own, readable afterwards, and gone when
// the check is done (SPEC §8, decision D3).
func TestProvisionerCreatesAndDropsASandbox(t *testing.T) {
	provisioner := newProvisioner(t, mysqlDSN, mysql.FlavorMySQL)
	name := verifyPrefix + "sandbox"

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

	seedSandbox(t, name)

	tables, err := box.Tables(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"invoices"}, tables)

	// A MySQL manifest names tables bare; an assertion written by hand may
	// still qualify one, and the qualifier can only be the database itself.
	for _, table := range []string{"invoices", name + ".invoices"} {
		rows, err := box.CountRows(t.Context(), table)
		require.NoError(t, err)
		assert.Equal(t, int64(3), rows, "counting %s", table)
	}

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
	provisioner := newProvisioner(t, mysqlDSN, mysql.FlavorMySQL)

	_, err := provisioner.Create(t.Context(), core.SandboxSpec{Name: "app"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	err = provisioner.Drop(t.Context(), "app")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	db, err := sql.Open("mysql", rootDSN(mysqlDSN))
	require.NoError(t, err)
	defer db.Close()

	var tables int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"select count(*) from information_schema.tables where table_schema = 'app'").Scan(&tables))
	assert.Positive(t, tables)
}

func TestProvisionerNeedsAPrefix(t *testing.T) {
	_, err := mysql.NewProvisioner(mysql.ProvisionOptions{DSN: rootDSN(mysqlDSN)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "database_prefix")
}

// seedSandbox writes into a sandbox the way a restore would.
func seedSandbox(t *testing.T, database string) {
	t.Helper()

	db, err := sql.Open("mysql", rootDSN(mysqlDSN))
	require.NoError(t, err)
	defer db.Close()

	for _, statement := range []string{
		"create table `" + database + "`.invoices (id int primary key auto_increment, total decimal(10,2))",
		"insert into `" + database + "`.invoices (total) values (1.5), (2.5), (null)",
	} {
		_, err := db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
}
