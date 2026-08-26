package mongodb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseURI(t *testing.T) {
	info, err := parseURI("mongodb://backup:hunter2@mongo.internal:27017/app?replicaSet=rs0")
	require.NoError(t, err)

	assert.Equal(t, "mongo.internal:27017", info.Hosts)
	assert.Equal(t, "backup", info.User)
	assert.Equal(t, "hunter2", info.Password)
	assert.Equal(t, "app", info.Database)
}

func TestParseURIWithoutADatabase(t *testing.T) {
	info, err := parseURI("mongodb://mongo1:27017,mongo2:27017/?replicaSet=rs0")
	require.NoError(t, err)

	// No database means the whole deployment, which is also the only shape
	// that can carry an oplog.
	assert.Empty(t, info.Database)
	assert.Contains(t, info.Hosts, "mongo1:27017")
}

func TestParseURIRejectsOtherSchemes(t *testing.T) {
	_, err := parseURI("postgres://pg:5432/app")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "mongodb://")
}

func TestParseURIRejectsAMissingHost(t *testing.T) {
	_, err := parseURI("mongodb:///app")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no host")
}

// TestRedact matters because mongodump echoes the URI it was given — password
// included — into its own error output, and that output travels into manifests
// and webhooks.
func TestRedact(t *testing.T) {
	info, err := parseURI("mongodb://backup:hunter2@mongo.internal:27017/app")
	require.NoError(t, err)

	out := info.redact("Failed: can't create session: failed to connect to mongodb://backup:hunter2@mongo.internal:27017/app: connection refused")

	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, "mongodb://***:***@")
	assert.Contains(t, out, "connection refused")
}

func TestRedactHandlesOtherCredentialsInTheSameLine(t *testing.T) {
	info, err := parseURI("mongodb://mongo:27017/")
	require.NoError(t, err)

	out := info.redact("connecting to mongodb+srv://someone:secret@cluster.example.net/")

	assert.NotContains(t, out, "secret")
}

func TestVersionNum(t *testing.T) {
	assert.Equal(t, 70014, versionNum("7.0.14"))
	assert.Equal(t, 80000, versionNum("8.0.0"))
	assert.Equal(t, 60016, versionNum("6.0.16-rc0"))
	assert.Zero(t, versionNum("unknown"))
}

func TestNewDefaults(t *testing.T) {
	dumper, err := New(Options{URI: "mongodb://mongo:27017/app"})
	require.NoError(t, err)

	assert.Equal(t, RowsEstimate, dumper.opts.RowEstimate)
	assert.Equal(t, defaultParallelCollections, dumper.opts.NumParallelCollections)
}

func TestNewRejectsABadURI(t *testing.T) {
	_, err := New(Options{URI: "mongodb://backup:hunter2@"})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

// TestWithDatabase: MongoDB authenticates against the database the URI names
// unless told otherwise, so moving the path has to carry the original one
// along as authSource — or the sandbox would fail to authenticate.
func TestWithDatabase(t *testing.T) {
	info, err := parseURI("mongodb://vaultd:hunter2@staging:27017/admin?replicaSet=rs0")
	require.NoError(t, err)

	sandbox, err := info.withDatabase("vaultd_verify_01j")
	require.NoError(t, err)

	assert.Equal(t, "vaultd_verify_01j", sandbox.Database)
	assert.Contains(t, sandbox.Raw, "authSource=admin")
	assert.Contains(t, sandbox.Raw, "replicaSet=rs0")
	assert.Equal(t, "admin", info.Database, "withDatabase must not mutate the original")

	reparsed, err := parseURI(sandbox.Raw)
	require.NoError(t, err)
	assert.Equal(t, "vaultd_verify_01j", reparsed.Database)
	assert.Equal(t, "hunter2", reparsed.Password)
}

// TestWithDatabaseKeepsAnExplicitAuthSource: an operator who named one means
// it, whatever the path says.
func TestWithDatabaseKeepsAnExplicitAuthSource(t *testing.T) {
	info, err := parseURI("mongodb://vaultd:hunter2@staging:27017/app?authSource=admin")
	require.NoError(t, err)

	sandbox, err := info.withDatabase("")
	require.NoError(t, err)

	assert.Equal(t, 1, strings.Count(sandbox.Raw, "authSource="))
	assert.Contains(t, sandbox.Raw, "authSource=admin")
	assert.Empty(t, sandbox.Database)
}

// TestWithDatabaseWithoutCredentials: nothing authenticates, so nothing needs
// an authSource.
func TestWithDatabaseWithoutCredentials(t *testing.T) {
	info, err := parseURI("mongodb://staging:27017/app")
	require.NoError(t, err)

	sandbox, err := info.withDatabase("vaultd_verify_01j")
	require.NoError(t, err)

	assert.NotContains(t, sandbox.Raw, "authSource")
	assert.Equal(t, "vaultd_verify_01j", sandbox.Database)
}
