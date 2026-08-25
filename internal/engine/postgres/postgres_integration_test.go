//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/postgres"
)

// defaultImage is the server these tests run against. The client on the host
// has to be at least this major version, which is the rule resolveBinary
// enforces. Both are overridable: a developer without a system-wide client can
// point VAULTD_TEST_PG_BINDIR at an unpacked one (see `make dev-pg-client`),
// and CI matches the image to the client it installed.
const defaultImage = "postgres:17-alpine"

var (
	serverDSN string
	binDir    = os.Getenv("VAULTD_TEST_PG_BINDIR")
)

func image() string {
	if custom := os.Getenv("VAULTD_TEST_PG_IMAGE"); custom != "" {
		return custom
	}
	return defaultImage
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, image(),
		tcpostgres.WithDatabase("app"),
		tcpostgres.WithUsername("backup"),
		tcpostgres.WithPassword("s3cret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting PostgreSQL:", err)
		os.Exit(1)
	}

	serverDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading the PostgreSQL DSN:", err)
		os.Exit(1)
	}

	if err := seed(ctx, serverDSN); err != nil {
		fmt.Fprintln(os.Stderr, "seeding PostgreSQL:", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintln(os.Stderr, "terminating PostgreSQL:", err)
	}
	os.Exit(code)
}

func seed(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `
		create table users (id serial primary key, email text not null, created_at timestamptz default now());
		create table sessions (id serial primary key, user_id int, token text);
		insert into users (email) select 'user' || i || '@example.com' from generate_series(1, 500) i;
		insert into sessions (user_id, token) select i, md5(i::text) from generate_series(1, 200) i;
		analyze;
	`)
	return err
}

// requireClient skips the test when no compatible pg_dump is installed. The
// dumper shells out to the vendor's client by design (SPEC §3), so this suite
// needs one on the host; the container image ships them.
func requireClient(t *testing.T, err error) {
	t.Helper()

	if err != nil && (strings.Contains(err.Error(), "need pg_dump") || strings.Contains(err.Error(), "need pg_dumpall")) {
		t.Skipf("no compatible PostgreSQL client on this host: %v", err)
	}
	require.NoError(t, err)
}

func newDumper(t *testing.T, opts postgres.Options) *postgres.Dumper {
	t.Helper()

	opts.DSN = serverDSN
	if opts.BinDir == "" {
		opts.BinDir = binDir
	}
	dumper, err := postgres.New(opts)
	require.NoError(t, err)
	return dumper
}

func TestProbeReadsTheCatalog(t *testing.T) {
	dumper := newDumper(t, postgres.Options{RowEstimate: postgres.RowsEstimate})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.EnginePostgres, info.Engine)
	assert.NotEmpty(t, info.Version)
	assert.GreaterOrEqual(t, info.VersionNum, 140000)
	assert.Equal(t, core.ConsistencySerializableSnapshot, info.Consistency)

	names := tableNames(info.Tables)
	assert.Contains(t, names, "public.users")
	assert.Contains(t, names, "public.sessions")

	// Estimates come from the planner statistics refreshed by ANALYZE.
	assert.InDelta(t, 500, rowsOf(info.Tables, "public.users"), 50)
}

func TestProbeCountsExactly(t *testing.T) {
	dumper := newDumper(t, postgres.Options{RowEstimate: postgres.RowsExact})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, int64(500), rowsOf(info.Tables, "public.users"))
	assert.Equal(t, int64(200), rowsOf(info.Tables, "public.sessions"))
	for _, table := range info.Tables {
		assert.True(t, table.RowsExact, "%s should be marked exact", table.Name)
	}
}

func TestProbeSkipsRowCounts(t *testing.T) {
	dumper := newDumper(t, postgres.Options{RowEstimate: postgres.RowsOff})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.NotEmpty(t, info.Tables, "table names are still needed for assertions")
	assert.Zero(t, rowsOf(info.Tables, "public.users"))
}

func TestDumpProducesACustomFormatArchive(t *testing.T) {
	dumper := newDumper(t, postgres.Options{})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	// PGDMP is the magic of pg_dump's custom format: the archive a restore
	// can read selectively and in parallel.
	assert.True(t, bytes.HasPrefix(out.Bytes(), []byte("PGDMP")), "not a custom-format archive")
	assert.Greater(t, out.Len(), 1000)
	assert.Equal(t, core.ConsistencySerializableSnapshot, result.Consistency)
	assert.Contains(t, result.DumperVersion, "pg_dump")
}

// TestDumpExcludesTableData covers the option that keeps a table's schema but
// drops its rows — audit logs and session tables.
func TestDumpExcludesTableData(t *testing.T) {
	full := newDumper(t, postgres.Options{})
	var withData bytes.Buffer
	_, err := full.Dump(t.Context(), &withData)
	requireClient(t, err)

	trimmed := newDumper(t, postgres.Options{ExcludeTableData: []string{"public.sessions"}})
	var withoutData bytes.Buffer
	_, err = trimmed.Dump(t.Context(), &withoutData)
	require.NoError(t, err)

	assert.Less(t, withoutData.Len(), withData.Len(), "excluding a table's data should shrink the dump")
}

func TestDumpGlobals(t *testing.T) {
	dumper := newDumper(t, postgres.Options{IncludeGlobals: true})

	require.True(t, dumper.HasGlobals())

	var out bytes.Buffer
	_, err := dumper.DumpGlobals(t.Context(), &out)
	requireClient(t, err)

	assert.Contains(t, out.String(), "ROLE backup", "the dumping role should appear in the globals")
}

func TestDumpCancellation(t *testing.T) {
	dumper := newDumper(t, postgres.Options{})

	// Probe first, so the skip below reflects a missing client rather than a
	// cancelled dump.
	_, err := dumper.Probe(t.Context())
	requireClient(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = dumper.Dump(ctx, io.Discard)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestNewRejectsAnUnusableDSN(t *testing.T) {
	_, err := postgres.New(postgres.Options{DSN: "not a dsn"})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not a dsn", "the connection string must not appear in errors")
}

func tableNames(tables []core.TableInfo) []string {
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
