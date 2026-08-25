//go:build integration

package mysql_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmariadb "github.com/testcontainers/testcontainers-go/modules/mariadb"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mysql"
)

// Both forks run for the whole package: they are the pair whose differences
// this adapter exists to handle.
var (
	mysqlDSN   string
	mariadbDSN string
	// restrictedDSN is a user with the least privileges a backup needs and
	// nothing more — no RELOAD, which is the common production setup and the
	// one that decides whether a replication position can be recorded.
	restrictedDSN string
)

const schema = `
create table users (
	id int auto_increment primary key,
	email varchar(190) not null,
	created_at timestamp default current_timestamp
) engine=InnoDB;

create table orders (
	id int auto_increment primary key,
	user_id int not null,
	total decimal(10,2)
) engine=InnoDB;

create table access_log (
	id int auto_increment primary key,
	line varchar(255)
) engine=MyISAM;
`

func TestMain(m *testing.M) {
	ctx := context.Background()

	mysqlC, err := tcmysql.Run(ctx, image("VAULTD_TEST_MYSQL_IMAGE", "mysql:8.0"),
		tcmysql.WithDatabase("app"),
		tcmysql.WithUsername("backup"),
		tcmysql.WithPassword("s3cret"),
	)
	if err != nil {
		fail("starting MySQL", err)
	}
	mysqlDSN, err = mysqlC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MySQL DSN", err)
	}

	mariadbC, err := tcmariadb.Run(ctx, image("VAULTD_TEST_MARIADB_IMAGE", "mariadb:11.4"),
		tcmariadb.WithDatabase("app"),
		tcmariadb.WithUsername("backup"),
		tcmariadb.WithPassword("s3cret"),
	)
	if err != nil {
		fail("starting MariaDB", err)
	}
	mariadbDSN, err = mariadbC.ConnectionString(ctx)
	if err != nil {
		fail("reading the MariaDB DSN", err)
	}

	for _, dsn := range []string{mysqlDSN, mariadbDSN} {
		if err := seed(ctx, dsn); err != nil {
			fail("seeding", err)
		}
		if err := grantPrivileges(ctx, rootDSN(dsn)); err != nil {
			fail("granting privileges", err)
		}
	}
	restrictedDSN = strings.Replace(mysqlDSN, "backup:s3cret", "restricted:s3cret", 1)

	code := m.Run()

	_ = testcontainers.TerminateContainer(mysqlC)
	_ = testcontainers.TerminateContainer(mariadbC)
	os.Exit(code)
}

func image(env, fallback string) string {
	if custom := os.Getenv(env); custom != "" {
		return custom
	}
	return fallback
}

func fail(what string, err error) {
	fmt.Fprintln(os.Stderr, what+":", err)
	os.Exit(1)
}

func seed(ctx context.Context, dsn string) error {
	db, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	for i := range 300 {
		if _, err := db.ExecContext(ctx,
			"insert into users (email) values (?)", fmt.Sprintf("user%d@example.com", i)); err != nil {
			return err
		}
	}
	for i := range 100 {
		if _, err := db.ExecContext(ctx,
			"insert into orders (user_id, total) values (?, ?)", i+1, float64(i)*1.5); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, "insert into access_log (line) values ('one'), ('two')")
	return err
}

// rootDSN turns the connection string of the backup user into the root one;
// the module gives both accounts the same password.
func rootDSN(dsn string) string {
	return strings.Replace(dsn, "backup:s3cret", "root:s3cret", 1)
}

// grantPrivileges gives the backup user RELOAD, and creates a second user
// deliberately without it.
func grantPrivileges(ctx context.Context, dsn string) error {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	statements := []string{
		"grant reload, replication client on *.* to 'backup'@'%'",
		"create user if not exists 'restricted'@'%' identified by 's3cret'",
		"grant select, show view, trigger, event, lock tables on app.* to 'restricted'@'%'",
		"flush privileges",
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	return nil
}

func newDumper(t *testing.T, opts mysql.Options) *mysql.Dumper {
	t.Helper()

	dumper, err := mysql.New(opts)
	require.NoError(t, err)
	return dumper
}

// requireClient skips when the host has no client of the right fork.
func requireClient(t *testing.T, err error) {
	t.Helper()

	if err != nil && strings.Contains(err.Error(), "found none installed") {
		t.Skipf("no client for this fork on the host: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "no mariadb client was found") {
		t.Skipf("no MariaDB client on the host: %v", err)
	}
	require.NoError(t, err)
}

func TestProbeMySQL(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.EngineMySQL, info.Engine)
	assert.True(t, strings.HasPrefix(info.Version, "8."), "unexpected version %q", info.Version)
	assert.Contains(t, tableNames(info.Tables), "users")

	// The MyISAM table means --single-transaction cannot cover everything, and
	// the manifest must not claim otherwise.
	assert.Equal(t, core.ConsistencyBestEffort, info.Consistency)
	assert.Contains(t, strings.Join(info.Warnings, " "), "access_log")
	assert.Equal(t, "MyISAM", storageEngineOf(info.Tables, "access_log"))
}

func TestProbeMariaDB(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mariadbDSN, Flavor: mysql.FlavorMariaDB})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.EngineMariaDB, info.Engine)
	assert.Contains(t, tableNames(info.Tables), "orders")
}

// TestProbeRejectsAFlavorMismatch: the two forks need different clients and
// different flags, so guessing is not an option.
func TestProbeRejectsAFlavorMismatch(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMariaDB})

	_, err := dumper.Probe(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "set engine: mysql")
}

func TestProbeFailsOnNonTransactionalTables(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL, OnNonInnoDB: mysql.NonInnoDBFail})

	_, err := dumper.Probe(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-transactional storage engine")
	assert.Contains(t, err.Error(), "on_non_innodb: lock")
}

func TestProbeLockModeReportsLockedConsistency(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL, OnNonInnoDB: mysql.NonInnoDBLock})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, core.ConsistencyLockedTables, info.Consistency)
}

func TestProbeCountsExactly(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL, RowEstimate: mysql.RowsExact})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)

	assert.Equal(t, int64(300), rowsOf(info.Tables, "users"))
	assert.Equal(t, int64(100), rowsOf(info.Tables, "orders"))
}

func TestDumpMySQL(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	dump := out.String()
	assert.Contains(t, dump, "CREATE TABLE `users`")
	assert.Contains(t, dump, "user1@example.com")
	assert.Contains(t, result.DumperVersion, "mysqldump")
	assert.Equal(t, core.ConsistencyBestEffort, result.Consistency)

	// Binary logging is on in the official image and this user holds RELOAD
	// and REPLICATION CLIENT, so the dump carries the replication position
	// that point-in-time recovery will need. The statement is spelled
	// CHANGE MASTER TO on 8.0 and CHANGE REPLICATION SOURCE TO on 8.4, so
	// match the part that does not move.
	assert.Regexp(t, `(MASTER_LOG_POS|SOURCE_LOG_POS)=\d+`, dump)
}

// TestDumpWithoutReloadPrivilege is the least-privilege case: a user with only
// the read grants can still take a backup, it just cannot record a replication
// position. Asking for one anyway makes mysqldump abort partway through, so
// the probe has to notice first.
func TestDumpWithoutReloadPrivilege(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: restrictedDSN, Flavor: mysql.FlavorMySQL})

	info, err := dumper.Probe(t.Context())
	requireClient(t, err)
	assert.Contains(t, strings.Join(info.Warnings, " "), "no RELOAD privilege")

	var out bytes.Buffer
	_, err = dumper.Dump(t.Context(), &out)
	require.NoError(t, err)

	dump := out.String()
	assert.Contains(t, dump, "CREATE TABLE `users`")
	assert.NotRegexp(t, `(MASTER_LOG_POS|SOURCE_LOG_POS)=\d+`, dump)
}

// TestLockModeNeedsReload refuses before the dump starts rather than failing
// halfway: locking every table is FLUSH TABLES WITH READ LOCK.
func TestLockModeNeedsReload(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: restrictedDSN, Flavor: mysql.FlavorMySQL, OnNonInnoDB: mysql.NonInnoDBLock})

	_, err := dumper.Probe(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs the RELOAD privilege")
}

func TestDumpMariaDB(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mariadbDSN, Flavor: mysql.FlavorMariaDB})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	dump := out.String()
	assert.Contains(t, dump, "CREATE TABLE `users`")
	assert.Contains(t, dump, "user1@example.com")
	assert.Contains(t, result.DumperVersion, "dump")
}

func TestDumpLocking(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL, OnNonInnoDB: mysql.NonInnoDBLock})

	var out bytes.Buffer
	result, err := dumper.Dump(t.Context(), &out)
	requireClient(t, err)

	assert.Equal(t, core.ConsistencyLockedTables, result.Consistency)
	assert.Contains(t, out.String(), "CREATE TABLE `access_log`")
}

func TestDumpCancellation(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: mysqlDSN, Flavor: mysql.FlavorMySQL})

	_, err := dumper.Probe(t.Context())
	requireClient(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = dumper.Dump(ctx, io.Discard)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestProbeReportsAWrongPassword(t *testing.T) {
	dumper := newDumper(t, mysql.Options{DSN: strings.Replace(mysqlDSN, "s3cret", "wrong-password", 1), Flavor: mysql.FlavorMySQL})

	_, err := dumper.Probe(t.Context())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "wrong-password", "the password must not appear in errors")
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

func storageEngineOf(tables []core.TableInfo, name string) string {
	for _, t := range tables {
		if t.Name == name {
			return t.StorageEngine
		}
	}
	return ""
}
