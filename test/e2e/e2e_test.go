//go:build integration

// Package e2e drives vaultd the way an operator does: a config file, a CLI
// invocation, and a bucket to check afterwards. It is the acceptance test for
// milestone M1 — `vaultd backup prod-pg`, end to end, against a real
// PostgreSQL server and a real S3-compatible bucket.
package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmariadb "github.com/testcontainers/testcontainers-go/modules/mariadb"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongooptions "go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/cli"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

const configTemplate = `
version: 1

defaults:
  compression: { algo: zstd, level: 1 }
  encryption:  { mode: age, recipients: ["${E2E_AGE_RECIPIENT}"] }
  retention:   { daily: { keep: 7 }, min_keep: 1 }
  timeout: 5m
  row_estimate: estimate

destinations:
  - name: bucket
    provider: ${E2E_PROVIDER}
    bucket: ${E2E_BUCKET}
    endpoint: ${E2E_ENDPOINT}
    region: ${E2E_REGION}
    access_key_id: ${E2E_ACCESS_KEY_ID}
    secret_access_key: ${E2E_SECRET_ACCESS_KEY}
    prefix: ${E2E_PREFIX}

targets:
  - name: prod-pg
    engine: postgres
    dsn: ${E2E_PG_DSN}
    destination: bucket
    schedule: "0 3 * * *"
    options:
      include_globals: true
      exclude_table_data: ["public.sessions"]

  - name: prod-mysql
    engine: mysql
    dsn: ${E2E_MYSQL_DSN}
    destination: bucket
    schedule: "30 3 * * *"
    options: { on_non_innodb: warn }

  - name: prod-mariadb
    engine: mariadb
    dsn: ${E2E_MARIADB_DSN}
    destination: bucket
    schedule: "0 4 * * *"

  - name: prod-mongo
    engine: mongodb
    uri: ${E2E_MONGO_URI}
    destination: bucket
    schedule: "30 4 * * *"
    options: { oplog: true }
`

var env struct {
	pgDSN      string
	mysqlDSN   string
	mariadbDSN string
	mongoURI   string
	endpoint   string
	accessKey  string
	secretKey  string

	// Which clients this host actually has. A missing one skips the tests
	// that need it rather than failing the suite.
	pgClientOK      bool
	mysqlClientOK   bool
	mariadbClientOK bool
	mongoClientOK   bool
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	// A client unpacked outside the system paths is put on PATH, so the
	// dumper resolves it the same way it would resolve a packaged one.
	if binDir := os.Getenv("VAULTD_TEST_PG_BINDIR"); binDir != "" {
		os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	env.pgClientOK = hasClient("pg_dump")
	env.mysqlClientOK = hasClient("mysqldump")
	env.mariadbClientOK = hasClient("mariadb-dump")
	env.mongoClientOK = hasClient("mongodump")

	postgresC, err := tcpostgres.Run(ctx, pgImage(),
		tcpostgres.WithDatabase("app"),
		tcpostgres.WithUsername("backup"),
		tcpostgres.WithPassword("s3cret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		fail("starting PostgreSQL", err)
	}
	env.pgDSN, err = postgresC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fail("reading the PostgreSQL DSN", err)
	}
	if err := seed(ctx, env.pgDSN); err != nil {
		fail("seeding PostgreSQL", err)
	}

	mysqlC, err := tcmysql.Run(ctx, containerImage("VAULTD_TEST_MYSQL_IMAGE", "mysql:8.0"),
		tcmysql.WithDatabase("app"), tcmysql.WithUsername("backup"), tcmysql.WithPassword("s3cret"))
	if err != nil {
		fail("starting MySQL", err)
	}
	env.mysqlDSN, err = mysqlC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MySQL DSN", err)
	}

	mariadbC, err := tcmariadb.Run(ctx, containerImage("VAULTD_TEST_MARIADB_IMAGE", "mariadb:11.4"),
		tcmariadb.WithDatabase("app"), tcmariadb.WithUsername("backup"), tcmariadb.WithPassword("s3cret"))
	if err != nil {
		fail("starting MariaDB", err)
	}
	env.mariadbDSN, err = mariadbC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MariaDB DSN", err)
	}

	mongoC, err := tcmongo.Run(ctx, containerImage("VAULTD_TEST_MONGO_IMAGE", "mongo:7"),
		tcmongo.WithUsername("backup"), tcmongo.WithPassword("s3cret"))
	if err != nil {
		fail("starting MongoDB", err)
	}
	env.mongoURI, err = mongoC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MongoDB URI", err)
	}

	if err := seedSQL(ctx, env.mysqlDSN); err != nil {
		fail("seeding MySQL", err)
	}
	if err := seedSQL(ctx, env.mariadbDSN); err != nil {
		fail("seeding MariaDB", err)
	}
	if err := seedMongo(ctx, env.mongoURI); err != nil {
		fail("seeding MongoDB", err)
	}

	minioC, err := tcminio.Run(ctx, "minio/minio:RELEASE.2025-04-22T22-12-26Z")
	if err != nil {
		fail("starting MinIO", err)
	}
	endpoint, err := minioC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MinIO endpoint", err)
	}
	env.endpoint = "http://" + endpoint
	env.accessKey = minioC.Username
	env.secretKey = minioC.Password

	code := m.Run()

	_ = testcontainers.TerminateContainer(postgresC)
	_ = testcontainers.TerminateContainer(mysqlC)
	_ = testcontainers.TerminateContainer(mariadbC)
	_ = testcontainers.TerminateContainer(mongoC)
	_ = testcontainers.TerminateContainer(minioC)
	os.Exit(code)
}

func pgImage() string { return containerImage("VAULTD_TEST_PG_IMAGE", "postgres:17-alpine") }

func containerImage(env, fallback string) string {
	if custom := os.Getenv(env); custom != "" {
		return custom
	}
	return fallback
}

func hasClient(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fail(what string, err error) {
	fmt.Fprintln(os.Stderr, what+":", err)
	os.Exit(1)
}

func seed(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `
		create table users (id serial primary key, email text not null, created_at timestamptz default now());
		create table orders (id serial primary key, user_id int not null, total numeric(10,2));
		create table sessions (id serial primary key, token text);
		insert into users (email) select 'user' || i || '@example.com' from generate_series(1, 2000) i;
		insert into orders (user_id, total) select (i % 2000) + 1, (i * 1.5)::numeric from generate_series(1, 5000) i;
		insert into sessions (token) select md5(i::text) from generate_series(1, 1000) i;
		analyze;
	`)
	return err
}

// setup writes a config file wired to the running containers and returns its
// path along with the identity able to decrypt what the run produces.
func setup(t *testing.T, bucket string) (configPath string, identity *age.X25519Identity) {
	t.Helper()

	return setupWith(t, bucket, configTemplate)
}

// setupWith is setup with a config of the test's choosing, for the suites that
// need a different retention policy or a deliberately broken target.
func setupWith(t *testing.T, bucket, template string) (configPath string, identity *age.X25519Identity) {
	t.Helper()

	if !env.pgClientOK {
		t.Skip("no pg_dump on this host; install postgresql-client or run `make dev-clients`")
	}

	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	store := newStore(t, bucket)
	require.NoError(t, store.CreateBucket(t.Context()))

	t.Setenv("E2E_AGE_RECIPIENT", identity.Recipient().String())
	t.Setenv("E2E_PROVIDER", "minio")
	t.Setenv("E2E_BUCKET", bucket)
	t.Setenv("E2E_ENDPOINT", env.endpoint)
	t.Setenv("E2E_REGION", "us-east-1")
	t.Setenv("E2E_ACCESS_KEY_ID", env.accessKey)
	t.Setenv("E2E_SECRET_ACCESS_KEY", env.secretKey)
	t.Setenv("E2E_PG_DSN", env.pgDSN)
	t.Setenv("E2E_MYSQL_DSN", env.mysqlDSN)
	t.Setenv("E2E_MARIADB_DSN", env.mariadbDSN)
	t.Setenv("E2E_MONGO_URI", env.mongoURI)
	t.Setenv("E2E_PREFIX", "e2e")

	configPath = filepath.Join(t.TempDir(), "vaultd.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(template), 0o600))
	return configPath, identity
}

func newStore(t *testing.T, bucket string) *s3.Store {
	t.Helper()

	store, err := s3.New(t.Context(), s3.Config{
		Provider:        s3.ProviderMinIO,
		Bucket:          bucket,
		Endpoint:        env.endpoint,
		Region:          "us-east-1",
		AccessKeyID:     env.accessKey,
		SecretAccessKey: env.secretKey,
	})
	require.NoError(t, err)
	return store
}

// run invokes the CLI in-process, exactly as main does.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errOut bytes.Buffer
	root := cli.NewRootCommand(&out, &errOut)
	root.SetArgs(args)

	err = root.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// TestBackupEndToEnd is the M1 acceptance criterion.
func TestBackupEndToEnd(t *testing.T) {
	configPath, identity := setup(t, "vaultd-e2e-backup")

	stdout, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ok: prod-pg backed up")

	store := newStore(t, "vaultd-e2e-backup")

	var dataKey, manifestKey, globalsKey string
	for object, err := range store.List(t.Context(), "e2e/prod-pg/") {
		require.NoError(t, err)
		switch {
		case manifest.IsManifestKey(object.Key):
			manifestKey = object.Key
		case strings.Contains(object.Key, "-globals."):
			globalsKey = object.Key
		default:
			dataKey = object.Key
		}
	}
	require.NotEmpty(t, dataKey, "no data object was stored")
	require.NotEmpty(t, manifestKey, "no manifest was stored")
	require.NotEmpty(t, globalsKey, "include_globals was set but no globals object was stored")

	// The key says what it is and how to read it.
	assert.Regexp(t, `^e2e/prod-pg/\d{4}/\d{2}/\d{2}/prod-pg-\d{8}T\d{6}Z-full\.pgdump\.zst\.age$`, dataKey)

	m := fetchManifest(t, store, manifestKey)
	assert.Equal(t, "prod-pg", m.Target)
	assert.Equal(t, core.EnginePostgres, m.Engine)
	assert.Equal(t, dataKey, m.Object.Key)
	assert.Equal(t, "zstd:1", m.Pipeline.Compression)
	assert.Equal(t, "age:x25519", m.Pipeline.Encryption)
	assert.Contains(t, m.Pipeline.Dumper, "pg_dump")
	assert.Equal(t, core.ConsistencySerializableSnapshot, m.Consistency)
	assert.NotZero(t, m.DurationMS)
	assert.Greater(t, m.Plaintext.Bytes, m.Object.Bytes, "the dump should have compressed")

	names := tableNames(m.Tables)
	assert.Contains(t, names, "public.users")
	assert.Contains(t, names, "public.orders")

	// What the manifest claims about the stored object has to be true.
	stored := download(t, store, dataKey)
	assert.Equal(t, m.Object.Bytes, int64(len(stored)))
	assert.Equal(t, m.Object.SHA256, sha256hex(stored))

	head, err := store.Head(t.Context(), dataKey)
	require.NoError(t, err)
	assert.Equal(t, m.Object.Bytes, head.Bytes)

	// And the object has to be the dump: decrypt, decompress, check.
	plaintext := decrypt(t, stored, identity)
	assert.Equal(t, m.Plaintext.Bytes, int64(len(plaintext)))
	assert.Equal(t, m.Plaintext.SHA256, sha256hex(plaintext))
	assert.True(t, bytes.HasPrefix(plaintext, []byte("PGDMP")), "the decrypted object is not a pg_dump archive")

	assertRestorable(t, plaintext)

	globals := decrypt(t, download(t, store, globalsKey), identity)
	assert.Contains(t, string(globals), "ROLE backup")
}

// TestListShowsTheBackup checks the other half of the loop: a backup taken by
// one command is found by another, reading only the bucket.
func TestListShowsTheBackup(t *testing.T) {
	configPath, _ := setup(t, "vaultd-e2e-list")

	_, stderr, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	stdout, _, err := run(t, "list", "prod-pg", "-c", configPath)
	require.NoError(t, err)

	assert.Contains(t, stdout, "prod-pg")
	assert.Contains(t, stdout, "daily")
	assert.Contains(t, stdout, "TARGET")

	jsonOut, _, err := run(t, "list", "prod-pg", "-c", configPath, "--json")
	require.NoError(t, err)
	assert.Contains(t, jsonOut, `"engine": "postgres"`)
	assert.NotContains(t, jsonOut, "s3cret", "the manifest must not carry credentials")
}

// TestDryRunWritesNothing keeps the promise the flag makes.
func TestDryRunWritesNothing(t *testing.T) {
	configPath, _ := setup(t, "vaultd-e2e-dryrun")

	stdout, stderr, err := run(t, "backup", "prod-pg", "--dry-run", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	assert.Contains(t, stdout, "dry run: prod-pg would be backed up")
	assert.Contains(t, stdout, "e2e/prod-pg/")

	store := newStore(t, "vaultd-e2e-dryrun")
	for object, err := range store.List(t.Context(), "e2e/") {
		require.NoError(t, err)
		t.Fatalf("a dry run wrote %s", object.Key)
	}
}

// TestBackupFailsOnUnreachableDatabase checks that a failed run is loud and
// leaves nothing behind that could be mistaken for a backup.
func TestBackupFailsOnUnreachableDatabase(t *testing.T) {
	configPath, _ := setup(t, "vaultd-e2e-unreachable")
	t.Setenv("E2E_PG_DSN", "postgres://backup:s3cret@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	_, _, err := run(t, "backup", "prod-pg", "-c", configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed during probe")
	assert.NotContains(t, err.Error(), "s3cret", "the password must not leak into an error")

	store := newStore(t, "vaultd-e2e-unreachable")

	// No backup object, no manifest: a failed run leaves nothing that could be
	// mistaken for a backup.
	for object, err := range store.List(t.Context(), "e2e/prod-pg/") {
		require.NoError(t, err)
		t.Fatalf("a failed backup left %s behind", object.Key)
	}

	// The failure itself is recorded, because retention has to know that the
	// most recent attempt did not produce a backup.
	entries, err := manifest.ParseIndex(download(t, store, "e2e/_index/prod-pg.jsonl"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].Succeeded())
	assert.Equal(t, "probe", entries[0].Phase)
	assert.NotContains(t, entries[0].Error, "s3cret", "the password must not reach the index either")
}

// assertRestorable reads the archive's table of contents with pg_restore,
// which is the structural check verification will formalize in M4.
func assertRestorable(t *testing.T, archive []byte) {
	t.Helper()

	if _, err := exec.LookPath("pg_restore"); err != nil {
		t.Log("pg_restore not available; skipping the archive structure check")
		return
	}

	path := filepath.Join(t.TempDir(), "dump.pgdump")
	require.NoError(t, os.WriteFile(path, archive, 0o600))

	out, err := exec.CommandContext(t.Context(), "pg_restore", "--list", path).CombinedOutput()
	require.NoError(t, err, "pg_restore --list failed: %s", out)

	assert.Contains(t, string(out), "TABLE DATA public users")
	// The excluded table keeps its definition but not its rows.
	assert.Contains(t, string(out), "TABLE public sessions")
	assert.NotContains(t, string(out), "TABLE DATA public sessions")
}

func fetchManifest(t *testing.T, store *s3.Store, key string) *manifest.Manifest {
	t.Helper()

	m, err := manifest.Unmarshal(download(t, store, key))
	require.NoError(t, err)
	return m
}

func download(t *testing.T, store *s3.Store, key string) []byte {
	t.Helper()

	body, err := store.Get(t.Context(), key)
	require.NoError(t, err)
	defer body.Close()

	data, err := io.ReadAll(body)
	require.NoError(t, err)
	return data
}

func decrypt(t *testing.T, object []byte, identity *age.X25519Identity) []byte {
	t.Helper()

	spec := pipeline.Spec{
		Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 1},
		Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge},
	}

	r, err := spec.Reader(bytes.NewReader(object), identity)
	require.NoError(t, err)
	defer r.Close()

	plaintext, err := io.ReadAll(r)
	require.NoError(t, err)
	return plaintext
}

func tableNames(tables []core.TableInfo) []string {
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		names = append(names, table.Name)
	}
	return names
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// seedSQL fills a MySQL or MariaDB server with the same shape of data.
func seedSQL(ctx context.Context, dsn string) error {
	db, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		create table users (id int auto_increment primary key, email varchar(190) not null) engine=InnoDB;
		create table orders (id int auto_increment primary key, user_id int, total decimal(10,2)) engine=InnoDB;
	`); err != nil {
		return err
	}
	for i := range 300 {
		if _, err := db.ExecContext(ctx, "insert into users (email) values (?)", fmt.Sprintf("user%d@example.com", i)); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, "insert into orders (user_id, total) values (1, 10.50), (2, 99.99)")
	return err
}

func seedMongo(ctx context.Context, uri string) error {
	client, err := mongo.Connect(mongooptions.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	docs := make([]any, 0, 200)
	for i := range 200 {
		docs = append(docs, bson.D{{Key: "email", Value: fmt.Sprintf("user%d@example.com", i)}})
	}
	_, err = client.Database("app").Collection("users").InsertMany(ctx, docs)
	return err
}

// TestBackupEveryEngine is the M2 acceptance criterion: every supported engine
// backed up through the same pipeline into the same bucket, each producing an
// object whose name says what it is and whose contents are what they claim.
func TestBackupEveryEngine(t *testing.T) {
	tests := []struct {
		target      string
		available   bool
		client      string
		keySuffix   string
		wantWarning string
		wantContent func(t *testing.T, plaintext []byte)
	}{
		{
			target: "prod-mysql", available: env.mysqlClientOK, client: "mysqldump",
			keySuffix: ".sql.zst.age",
			wantContent: func(t *testing.T, plaintext []byte) {
				assert.Contains(t, string(plaintext), "CREATE TABLE `users`")
				assert.Contains(t, string(plaintext), "user1@example.com")
			},
		},
		{
			target: "prod-mariadb", available: env.mariadbClientOK, client: "mariadb-dump",
			keySuffix: ".sql.zst.age",
			wantContent: func(t *testing.T, plaintext []byte) {
				assert.Contains(t, string(plaintext), "CREATE TABLE `users`")
			},
		},
		{
			target: "prod-mongo", available: env.mongoClientOK, client: "mongodump",
			keySuffix: ".archive.zst.age",
			// The config asks for an oplog, and this deployment is a
			// standalone: vaultd degrades and says so rather than failing.
			wantWarning: "standalone",
			wantContent: func(t *testing.T, plaintext []byte) {
				// The mongodump archive magic number.
				require.Greater(t, len(plaintext), 4)
				assert.Equal(t, []byte{0x6d, 0xe2, 0x99, 0x81}, plaintext[:4])
				assert.Contains(t, string(plaintext), "users")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if !tt.available {
				t.Skipf("%s is not installed on this host; run `make dev-clients`", tt.client)
			}

			bucket := "vaultd-e2e-" + tt.target
			configPath, identity := setup(t, bucket)

			stdout, stderr, err := run(t, "backup", tt.target, "-c", configPath)
			require.NoError(t, err, "stderr: %s", stderr)
			assert.Contains(t, stdout, "ok: "+tt.target+" backed up")
			if tt.wantWarning != "" {
				assert.Contains(t, stderr, tt.wantWarning, "the operator must be told what the server could not give")
			}

			store := newStore(t, bucket)

			var dataKey, manifestKey string
			for object, err := range store.List(t.Context(), "e2e/"+tt.target+"/") {
				require.NoError(t, err)
				if manifest.IsManifestKey(object.Key) {
					manifestKey = object.Key
				} else {
					dataKey = object.Key
				}
			}
			require.NotEmpty(t, dataKey)
			require.NotEmpty(t, manifestKey)

			// The extension states the engine's format and the pipeline that
			// wrapped it.
			assert.True(t, strings.HasSuffix(dataKey, tt.keySuffix), "unexpected key %s", dataKey)

			m := fetchManifest(t, store, manifestKey)
			assert.Equal(t, tt.target, m.Target)
			assert.NotEmpty(t, m.ServerVersion)
			assert.NotEmpty(t, m.Tables, "the manifest should list what was dumped")
			assert.Contains(t, m.Pipeline.Dumper, "dump")

			stored := download(t, store, dataKey)
			assert.Equal(t, m.Object.SHA256, sha256hex(stored))

			plaintext := decrypt(t, stored, identity)
			assert.Equal(t, m.Plaintext.SHA256, sha256hex(plaintext))
			tt.wantContent(t, plaintext)

			// Every engine's backup must also read back through the verifier,
			// including the format check that knows what that engine's dump
			// looks like.
			stdout, stderr, err = run(t, "verify", "--target", tt.target, "--latest",
				"--level", "structural", "--identity-file", identityFile(t, identity), "-c", configPath)
			require.NoError(t, err, "stderr: %s", stderr)
			assert.Contains(t, stdout, "passed structural verification")
		})
	}
}

// TestRestoreMySQL is the round trip for the other SQL engine: back up, then
// restore into a database that did not exist, and count what arrived.
func TestRestoreMySQL(t *testing.T) {
	if !env.mysqlClientOK {
		t.Skip("mysqldump is not installed on this host; run `make dev-clients`")
	}

	configPath, identity := setup(t, "vaultd-e2e-restore-mysql")

	_, stderr, err := run(t, "backup", "prod-mysql", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore-mysql")
	entry := latestBackup(t, store, "e2e/_index/prod-mysql.jsonl")

	restoredDSN := createMySQLDatabase(t, "restored_here")

	stdout, stderr, err := run(t, "restore", entry.ID, "--to", restoredDSN, "--confirm",
		"--identity-file", identityFile(t, identity), "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ok: restored prod-mysql")

	assert.Equal(t, int64(300), countMySQLRows(t, restoredDSN, "users"))
	assert.Equal(t, int64(2), countMySQLRows(t, restoredDSN, "orders"))
}

// TestRestoreMongoDB restores into the deployment the archive came from, which
// is what mongorestore does: the namespaces travel with the archive, so the
// URI selects the server rather than a new name for the data. That makes the
// destination non-empty, which is exactly the guard --force exists for.
func TestRestoreMongoDB(t *testing.T) {
	if !env.mongoClientOK {
		t.Skip("mongorestore is not installed on this host; run `make dev-clients`")
	}

	configPath, identity := setup(t, "vaultd-e2e-restore-mongo")

	_, stderr, err := run(t, "backup", "prod-mongo", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)

	store := newStore(t, "vaultd-e2e-restore-mongo")
	entry := latestBackup(t, store, "e2e/_index/prod-mongo.jsonl")
	key := identityFile(t, identity)

	_, _, err = run(t, "restore", entry.ID, "--to", env.mongoURI, "--confirm", "--identity-file", key, "-c", configPath)
	require.Error(t, err, "the deployment already holds these collections")
	assert.Contains(t, err.Error(), "--force")

	stdout, stderr, err := run(t, "restore", entry.ID, "--to", env.mongoURI, "--confirm", "--force", "--clean",
		"--identity-file", key, "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ok: restored prod-mongo")

	assert.Equal(t, int64(200), countMongoDocuments(t, env.mongoURI, "app", "users"))
}

// createMySQLDatabase makes an empty database on the test server and returns a
// DSN pointing at it. The backup user is scoped to its own database — which is
// the point of a least-privilege account — so the database is created as root
// and then granted.
func createMySQLDatabase(t *testing.T, name string) string {
	t.Helper()

	root, err := sql.Open("mysql", strings.Replace(env.mysqlDSN, "backup:s3cret", "root:s3cret", 1))
	require.NoError(t, err)
	defer root.Close()

	for _, statement := range []string{
		"drop database if exists " + name,
		"create database " + name,
		"grant all privileges on `" + name + "`.* to 'backup'@'%'",
		"flush privileges",
	} {
		_, err := root.ExecContext(t.Context(), statement)
		require.NoError(t, err, statement)
	}

	return replaceMySQLDatabase(env.mysqlDSN, name)
}

// replaceMySQLDatabase swaps the database in a driver-form MySQL DSN.
func replaceMySQLDatabase(dsn, name string) string {
	head, query, hasQuery := strings.Cut(dsn, "?")

	slash := strings.LastIndex(head, "/")
	out := head[:slash+1] + name
	if hasQuery {
		out += "?" + query
	}
	return out
}

func countMySQLRows(t *testing.T, dsn, table string) int64 {
	t.Helper()

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()

	var rows int64
	require.NoError(t, db.QueryRowContext(t.Context(), "select count(*) from `"+table+"`").Scan(&rows))
	return rows
}

func countMongoDocuments(t *testing.T, uri, database, collection string) int64 {
	t.Helper()

	client, err := mongo.Connect(mongooptions.Client().ApplyURI(uri))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(t.Context()) }()

	count, err := client.Database(database).Collection(collection).CountDocuments(t.Context(), bson.D{})
	require.NoError(t, err)
	return count
}
