//go:build integration

package mongodb_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mongodb"
)

// Two deployments, because the interesting behaviour differs between them: a
// replica set has an oplog and can be dumped to a point in time, a standalone
// cannot, and authentication is what exercises the credential handoff.
var (
	standaloneURI string
	replicaURI    string
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	standalone, err := tcmongo.Run(ctx, image(),
		tcmongo.WithUsername("backup"),
		tcmongo.WithPassword("s3cret"),
	)
	if err != nil {
		fail("starting MongoDB", err)
	}
	standaloneURI, err = standalone.ConnectionString(ctx)
	if err != nil {
		fail("reading the MongoDB URI", err)
	}

	replica, err := tcmongo.Run(ctx, image(), tcmongo.WithReplicaSet("rs0"))
	if err != nil {
		fail("starting a MongoDB replica set", err)
	}
	replicaURI, err = replica.ConnectionString(ctx)
	if err != nil {
		fail("reading the replica set URI", err)
	}

	for _, uri := range []string{standaloneURI, replicaURI} {
		if err := seed(ctx, uri); err != nil {
			fail("seeding MongoDB", err)
		}
	}

	code := m.Run()

	_ = testcontainers.TerminateContainer(standalone)
	_ = testcontainers.TerminateContainer(replica)
	os.Exit(code)
}

func image() string {
	if custom := os.Getenv("VAULTD_TEST_MONGO_IMAGE"); custom != "" {
		return custom
	}
	return "mongo:7"
}

func fail(what string, err error) {
	fmt.Fprintln(os.Stderr, what+":", err)
	os.Exit(1)
}

func seed(ctx context.Context, uri string) error {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	users := make([]any, 0, 250)
	for i := range 250 {
		users = append(users, bson.D{{Key: "email", Value: fmt.Sprintf("user%d@example.com", i)}})
	}
	if _, err := client.Database("app").Collection("users").InsertMany(ctx, users); err != nil {
		return err
	}

	orders := make([]any, 0, 80)
	for i := range 80 {
		orders = append(orders, bson.D{{Key: "total", Value: float64(i) * 1.5}})
	}
	_, err = client.Database("app").Collection("orders").InsertMany(ctx, orders)
	return err
}

func newDumper(t *testing.T, opts mongodb.Options) *mongodb.Dumper {
	t.Helper()

	dumper, err := mongodb.New(opts)
	require.NoError(t, err)
	return dumper
}

func requireClient(t *testing.T, err error) {
	t.Helper()

	if err != nil && strings.Contains(err.Error(), "mongodump is not installed") {
		t.Skip("mongodump is not on this host; install mongodb-database-tools or run `make dev-clients`")
	}
	require.NoError(t, err)
}

func TestProbeStandalone(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: standaloneURI})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.EngineMongoDB, info.Engine)
	assert.NotEmpty(t, info.Version)
	assert.Greater(t, info.VersionNum, 60000)
	assert.Contains(t, collectionNames(info.Tables), "app.users")
	assert.Equal(t, core.ConsistencyBestEffort, info.Consistency)
}

// TestProbeDegradesOplogOnStandalone is the documented degradation (SPEC §4.1):
// asking for an oplog on a server that has none is a warning, not a failure.
func TestProbeDegradesOplogOnStandalone(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: standaloneURI, Oplog: true})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.ConsistencyBestEffort, info.Consistency)
	assert.Contains(t, strings.Join(info.Warnings, " "), "standalone")
}

func TestProbeReplicaSetWithOplog(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: replicaURI, Oplog: true})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.ConsistencyPointInTime, info.Consistency)
	assert.Empty(t, info.Warnings)
}

func TestProbeSuggestsOplogOnAReplicaSet(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: replicaURI})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Contains(t, strings.Join(info.Warnings, " "), "options.oplog: true")
}

func TestProbeCounts(t *testing.T) {
	estimated := newDumper(t, mongodb.Options{URI: standaloneURI, RowEstimate: mongodb.RowsEstimate})
	info, err := estimated.Probe(t.Context())
	requireClient(t, err)
	assert.Equal(t, int64(250), rowsOf(info.Tables, "app.users"))

	exact := newDumper(t, mongodb.Options{URI: standaloneURI, RowEstimate: mongodb.RowsExact})
	info, err = exact.Probe(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(80), rowsOf(info.Tables, "app.orders"))
	for _, collection := range info.Tables {
		assert.True(t, collection.RowsExact)
	}
}

// TestDumpWithAuthentication exercises the credential handoff: the URI, with
// its password, reaches mongodump through an inherited pipe rather than argv
// or a file on disk.
func TestDumpWithAuthentication(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: standaloneURI})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	// The archive format starts with its own magic number.
	require.Greater(t, out.Len(), 100)
	assert.Equal(t, []byte{0x6d, 0xe2, 0x99, 0x81}, out.Bytes()[:4], "not a mongodump archive")
	assert.Contains(t, out.String(), "users")
	assert.Contains(t, result.DumperVersion, "mongodump")
}

func TestDumpReplicaSetWithOplog(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: replicaURI, Oplog: true})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	assert.Equal(t, core.ConsistencyPointInTime, result.Consistency)
	// Where the oplog stood when the dump finished: the point a replay would
	// start from.
	assert.Regexp(t, `^\d+,\d+$`, result.OplogEnd)
	assert.Greater(t, out.Len(), 100)
}

// TestDumpRedactsCredentials guards the path from a client's stderr into a
// manifest and a webhook.
func TestDumpRedactsCredentials(t *testing.T) {
	wrong := strings.Replace(standaloneURI, "s3cret", "wrong-password", 1)
	dumper := newDumper(t, mongodb.Options{URI: wrong})

	_, err := dumper.Probe(t.Context())
	if err == nil {
		t.Skip("this deployment accepted a wrong password; nothing to redact")
	}
	assert.NotContains(t, err.Error(), "wrong-password")
}

func TestDumpCancellation(t *testing.T) {
	dumper := newDumper(t, mongodb.Options{URI: standaloneURI})

	_, err := dumper.Probe(t.Context())
	requireClient(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = dumper.Dump(ctx, io.Discard)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func collectionNames(tables []core.TableInfo) []string {
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Name)
	}
	return names
}

func rowsOf(tables []core.TableInfo, name string) int64 {
	for _, t := range tables {
		if t.Name == name {
			return t.Rows
		}
	}
	return -1
}
