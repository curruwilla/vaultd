package mysql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
)

func mysqlCaps(major, minor, patch int) capabilities {
	return capabilities{Version: serverVersion{Major: major, Minor: minor, Patch: patch, Flavor: FlavorMySQL}}
}

func mariaCaps(major, minor, patch int) capabilities {
	return capabilities{Version: serverVersion{Major: major, Minor: minor, Patch: patch, Flavor: FlavorMariaDB}}
}

// TestDumpArgs pins down the flag matrix. Each of these is conditional on
// something the probe measured, because the client refuses to start when a
// flag does not match the server it is talking to.
func TestDumpArgs(t *testing.T) {
	tests := []struct {
		name            string
		caps            capabilities
		onNonInnoDB     NonInnoDB
		want            []string
		absent          []string
		wantConsistency core.Consistency
	}{
		{
			name:            "modern MySQL with binary logging and GTIDs",
			caps:            withBinlog(withGTID(mysqlCaps(8, 0, 46))),
			onNonInnoDB:     NonInnoDBWarn,
			want:            []string{"--single-transaction", "--skip-lock-tables", "--source-data=2", "--set-gtid-purged=ON", "--no-tablespaces"},
			wantConsistency: core.ConsistencySingleTransaction,
		},
		{
			name:            "MySQL before the flag was renamed",
			caps:            withBinlog(mysqlCaps(8, 0, 20)),
			onNonInnoDB:     NonInnoDBWarn,
			want:            []string{"--master-data=2"},
			absent:          []string{"--source-data=2", "--set-gtid-purged=ON"},
			wantConsistency: core.ConsistencySingleTransaction,
		},
		{
			name:            "binary logging on but the user lacks the privileges",
			caps:            capabilities{Version: serverVersion{Major: 8, Minor: 0, Patch: 46, Flavor: FlavorMySQL}, LogBin: true, CanFlush: true},
			onNonInnoDB:     NonInnoDBWarn,
			absent:          []string{"--source-data=2", "--master-data=2"},
			wantConsistency: core.ConsistencySingleTransaction,
		},
		{
			name:            "binary logging off",
			caps:            mysqlCaps(8, 4, 0),
			onNonInnoDB:     NonInnoDBWarn,
			absent:          []string{"--source-data=2", "--master-data=2"},
			wantConsistency: core.ConsistencySingleTransaction,
		},
		{
			name:            "MariaDB keeps the old flag and has no GTID purge",
			caps:            withBinlog(withGTID(mariaCaps(11, 4, 2))),
			onNonInnoDB:     NonInnoDBWarn,
			want:            []string{"--master-data=2"},
			absent:          []string{"--source-data=2", "--set-gtid-purged=ON", "--no-tablespaces"},
			wantConsistency: core.ConsistencySingleTransaction,
		},
		{
			name:            "locking mode trades availability for consistency",
			caps:            mysqlCaps(8, 0, 46),
			onNonInnoDB:     NonInnoDBLock,
			want:            []string{"--lock-all-tables"},
			absent:          []string{"--single-transaction", "--skip-lock-tables"},
			wantConsistency: core.ConsistencyLockedTables,
		},
		{
			name:            "non-transactional tables downgrade the claim",
			caps:            withNonInnoDB(mysqlCaps(8, 0, 46), "logs (MyISAM)"),
			onNonInnoDB:     NonInnoDBWarn,
			want:            []string{"--single-transaction"},
			wantConsistency: core.ConsistencyBestEffort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, consistency := dumpArgs(tt.caps, "app", tt.onNonInnoDB)

			for _, flag := range tt.want {
				assert.Contains(t, args, flag)
			}
			for _, flag := range tt.absent {
				assert.NotContains(t, args, flag)
			}
			assert.Equal(t, tt.wantConsistency, consistency)
			assert.Equal(t, "app", args[len(args)-1], "the database must be the final argument")
		})
	}
}

func TestDumpArgsAlwaysIncludeTheEssentials(t *testing.T) {
	args, _ := dumpArgs(mysqlCaps(8, 0, 46), "app", NonInnoDBWarn)

	// --quick streams rows instead of buffering a table; the rest are the
	// objects a schema-only dump would silently lose.
	for _, flag := range []string{"--quick", "--routines", "--triggers", "--events", "--hex-blob"} {
		assert.Contains(t, args, flag)
	}
}

func TestClientNames(t *testing.T) {
	assert.Equal(t, []string{"mysqldump"}, clientNames(FlavorMySQL))
	assert.Equal(t, []string{"mariadb-dump", "mysqldump"}, clientNames(FlavorMariaDB))
}

func TestParseServerVersion(t *testing.T) {
	tests := []struct {
		raw        string
		comment    string
		wantFlavor Flavor
		wantString string
		wantNum    int
	}{
		{"8.0.46-0ubuntu0.24.04.3", "(Ubuntu)", FlavorMySQL, "8.0.46", 80046},
		{"8.4.3", "MySQL Community Server - GPL", FlavorMySQL, "8.4.3", 80403},
		{"11.4.2-MariaDB-ubu2404", "mariadb.org binary distribution", FlavorMariaDB, "11.4.2", 110402},
		{"10.11.14", "mariadb.org binary distribution", FlavorMariaDB, "10.11.14", 101114},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			version, err := parseServerVersion(tt.raw, tt.comment)

			require.NoError(t, err)
			assert.Equal(t, tt.wantFlavor, version.Flavor)
			assert.Equal(t, tt.wantString, version.String())
			assert.Equal(t, tt.wantNum, version.Num())
		})
	}
}

// TestParseClientVersionMariaDBDistrib is the subtle one: MariaDB's older
// client reports its own tool version first and the server version it belongs
// to after "Distrib". Reading the first number would compare 10.19 against an
// 11.4 server and reach the wrong conclusion.
func TestParseClientVersion(t *testing.T) {
	tests := []struct {
		output     string
		wantFlavor Flavor
		wantString string
	}{
		{"mysqldump  Ver 8.0.46-0ubuntu0.24.04.3 for Linux on x86_64 ((Ubuntu))\n", FlavorMySQL, "8.0.46"},
		{"mysqldump  Ver 10.19 Distrib 10.11.14-MariaDB, for debian-linux-gnu (x86_64)\n", FlavorMariaDB, "10.11.14"},
		{"mariadb-dump  Ver 11.4.2-MariaDB for Linux on x86_64\n", FlavorMariaDB, "11.4.2"},
	}

	for _, tt := range tests {
		t.Run(tt.wantString, func(t *testing.T) {
			version, err := parseClientVersion(tt.output)

			require.NoError(t, err)
			assert.Equal(t, tt.wantFlavor, version.Flavor)
			assert.Equal(t, tt.wantString, version.String())
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := serverVersion{Major: 8, Minor: 0, Patch: 26}

	assert.True(t, v.AtLeast(8, 0, 26))
	assert.True(t, v.AtLeast(8, 0, 25))
	assert.False(t, v.AtLeast(8, 0, 27))
	assert.False(t, v.AtLeast(8, 1, 0))
}

func withBinlog(c capabilities) capabilities {
	c.LogBin, c.CanFlush, c.CanReadPosition = true, true, true
	return c
}
func withGTID(c capabilities) capabilities { c.GTID = true; return c }
func withNonInnoDB(c capabilities, tables ...string) capabilities {
	c.NonInnoDB = tables
	return c
}

func TestArgsCarryNoSecrets(t *testing.T) {
	info, err := parseDSN("mysql://backup:hunter2@db.internal:3306/app")
	require.NoError(t, err)

	joined := strings.Join(info.args(), " ")

	assert.NotContains(t, joined, "hunter2", "the password must never reach the command line")
	assert.Contains(t, joined, "--user=backup")
	assert.Equal(t, "hunter2", info.env()["MYSQL_PWD"])
}

// TestParseGrants is the check that decides whether the dump asks for a
// replication position. Getting it wrong is not cosmetic: mysqldump aborts
// partway through when it cannot run FLUSH TABLES.
func TestParseGrants(t *testing.T) {
	tests := []struct {
		name                 string
		grants               []string
		wantFlush            bool
		wantReadablePosition bool
	}{
		{
			name: "a per-database owner has neither",
			// The common application user. ALL PRIVILEGES looks broad, but
			// RELOAD and REPLICATION CLIENT only exist at server level.
			grants: []string{
				"GRANT USAGE ON *.* TO `backup`@`%`",
				"GRANT ALL PRIVILEGES ON `app`.* TO `backup`@`%`",
			},
		},
		{
			name: "explicit global grants",
			grants: []string{
				"GRANT SELECT, RELOAD, REPLICATION CLIENT ON *.* TO `backup`@`%`",
			},
			wantFlush:            true,
			wantReadablePosition: true,
		},
		{
			name: "reload only",
			grants: []string{
				"GRANT SELECT, LOCK TABLES, RELOAD ON *.* TO `backup`@`%`",
			},
			wantFlush: true,
		},
		{
			name:                 "a global superuser",
			grants:               []string{"GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost` WITH GRANT OPTION"},
			wantFlush:            true,
			wantReadablePosition: true,
		},
		{
			name:                 "MariaDB spelling",
			grants:               []string{"GRANT SELECT, BINLOG MONITOR ON *.* TO `backup`@`%`"},
			wantReadablePosition: true,
		},
		{
			name: "the least-privilege reader",
			grants: []string{
				"GRANT USAGE ON *.* TO `restricted`@`%`",
				"GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES ON `app`.* TO `restricted`@`%`",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canFlush, canReadPosition := parseGrants(tt.grants)

			assert.Equal(t, tt.wantFlush, canFlush, "flush privilege")
			assert.Equal(t, tt.wantReadablePosition, canReadPosition, "replication position privilege")
		})
	}
}
