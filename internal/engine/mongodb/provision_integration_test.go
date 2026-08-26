//go:build integration

package mongodb_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mongodb"
)

const verifyPrefix = "vaultd_verify_"

func newProvisioner(t *testing.T) *mongodb.Provisioner {
	t.Helper()

	provisioner, err := mongodb.NewProvisioner(mongodb.ProvisionOptions{
		URI:    standaloneURI,
		Prefix: verifyPrefix,
	})
	require.NoError(t, err)
	return provisioner
}

// appTables is what a manifest of the seeded deployment records: collections
// qualified with the database they came from, which is what tells the adapter
// which namespace to rename on the way in.
var appTables = []core.TableInfo{{Name: "app.users"}, {Name: "app.orders"}}

func TestProvisionerProbeReadsTheStagingServer(t *testing.T) {
	info, err := newProvisioner(t).Probe(t.Context())

	require.NoError(t, err)
	assert.Equal(t, core.EngineMongoDB, info.Engine)
	assert.NotEmpty(t, info.Version)
}

// TestProvisionerCreatesAndDropsASandbox is the contract restore verification
// depends on: a database of its own, readable afterwards under the names the
// manifest uses, and gone when the check is done (SPEC §8, decision D3).
func TestProvisionerCreatesAndDropsASandbox(t *testing.T) {
	provisioner := newProvisioner(t)
	name := verifyPrefix + "sandbox"

	require.NoError(t, provisioner.Drop(t.Context(), name))

	box, err := provisioner.Create(t.Context(), core.SandboxSpec{Name: name, Tables: appTables})
	require.NoError(t, err)
	assert.Equal(t, name, box.Name())

	empty, err := box.IsEmpty(t.Context())
	require.NoError(t, err)
	assert.True(t, empty, "a database that holds no collections is empty")

	seedSandbox(t, name)

	names, err := provisioner.List(t.Context())
	require.NoError(t, err)
	assert.Contains(t, names, name)

	tables, err := box.Tables(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{name + ".invoices"}, tables)

	// The manifest calls it app.invoices; here it lives under the sandbox's
	// name, and the adapter is what knows that.
	rows, err := box.CountRows(t.Context(), "app.invoices")
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows)

	_, err = box.Scalar(t.Context(), "select 1")
	require.ErrorIs(t, err, core.ErrQueryUnsupported)

	require.NoError(t, box.Drop(t.Context()))

	names, err = provisioner.List(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, names, name)
}

// TestProvisionerRefusesWhatItCannotRename: an archive of several databases
// cannot become one ephemeral database, and saying so as a skip beats
// restoring onto the staging server's own names.
func TestProvisionerRefusesWhatItCannotRename(t *testing.T) {
	provisioner := newProvisioner(t)

	_, err := provisioner.Create(t.Context(), core.SandboxSpec{
		Name:   verifyPrefix + "multi",
		Tables: []core.TableInfo{{Name: "app.users"}, {Name: "analytics.events"}},
	})

	require.ErrorIs(t, err, core.ErrSandboxUnsupported)
	assert.Contains(t, err.Error(), "point the target's uri at a single database")
}

// TestProvisionerRefusesADatabaseOutsideThePrefix is the guard that stands
// between a verification and a staging database somebody cares about.
func TestProvisionerRefusesADatabaseOutsideThePrefix(t *testing.T) {
	provisioner := newProvisioner(t)

	_, err := provisioner.Create(t.Context(), core.SandboxSpec{Name: "app", Tables: appTables})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	err = provisioner.Drop(t.Context(), "app")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only ever creates and drops")

	client, err := mongo.Connect(options.Client().ApplyURI(standaloneURI))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(t.Context()) }()

	collections, err := client.Database("app").ListCollectionNames(t.Context(), bson.D{})
	require.NoError(t, err)
	assert.NotEmpty(t, collections)
}

func TestProvisionerNeedsAPrefix(t *testing.T) {
	_, err := mongodb.NewProvisioner(mongodb.ProvisionOptions{URI: standaloneURI})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "database_prefix")
}

// seedSandbox writes into a sandbox the way a restore would.
func seedSandbox(t *testing.T, database string) {
	t.Helper()

	client, err := mongo.Connect(options.Client().ApplyURI(standaloneURI))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(t.Context()) }()

	_, err = client.Database(database).Collection("invoices").InsertMany(t.Context(), []any{
		bson.D{{Key: "total", Value: 1.5}},
		bson.D{{Key: "total", Value: 2.5}},
		bson.D{{Key: "total", Value: nil}},
	})
	require.NoError(t, err)
}
