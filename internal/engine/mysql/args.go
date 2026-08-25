package mysql

import (
	"github.com/curruwilla/vaultd/internal/core"
)

// NonInnoDB selects what happens when a target holds tables on a
// non-transactional storage engine (SPEC §4.1). --single-transaction only
// gives a consistent snapshot for InnoDB; on MyISAM it silently does not, and
// silently inconsistent is the worst outcome for a backup.
type NonInnoDB string

const (
	NonInnoDBWarn NonInnoDB = "warn"
	NonInnoDBFail NonInnoDB = "fail"
	NonInnoDBLock NonInnoDB = "lock"
)

// capabilities is what the probe learned about the server, reduced to what
// actually changes the command line.
type capabilities struct {
	Version serverVersion
	// LogBin reports whether binary logging is on. Without it the client
	// cannot record a replication position, and asking for one is an error.
	LogBin bool
	// GTID reports whether GTIDs are in use (MySQL only).
	GTID bool
	// CanFlush reports whether the backup user holds RELOAD (or
	// FLUSH_TABLES): locking every table goes through FLUSH TABLES.
	CanFlush bool
	// CanReadPosition reports whether the user may run SHOW MASTER STATUS,
	// which needs REPLICATION CLIENT (BINLOG MONITOR on newer MariaDB).
	CanReadPosition bool
	// NonInnoDB lists tables on a non-transactional engine.
	NonInnoDB []string
}

// dumpArgs builds the client's arguments and reports the consistency the
// resulting dump will actually have.
//
// Every flag here is conditional on something the probe measured. Passing
// --set-gtid-purged=ON to a server with GTIDs off, or --source-data to one
// without binary logging, is not a harmless extra: the client refuses to run.
func dumpArgs(caps capabilities, database string, onNonInnoDB NonInnoDB) ([]string, core.Consistency) {
	args := []string{
		"--quick",    // stream rows instead of buffering a table in memory
		"--routines", // stored procedures and functions
		"--triggers", //
		"--events",   // scheduled events
		"--hex-blob", // binary columns survive a text dump intact
	}

	// Tablespace metadata needs the PROCESS privilege, which a least-privilege
	// backup user does not have (SPEC §15).
	if caps.Version.Flavor == FlavorMySQL && caps.Version.AtLeast(8, 0, 0) {
		args = append(args, "--no-tablespaces")
	}

	consistency := core.ConsistencySingleTransaction
	if onNonInnoDB == NonInnoDBLock {
		// A global read lock is the only way to be consistent across storage
		// engines. It blocks writers for the duration, which is why it is
		// opt-in.
		args = append(args, "--lock-all-tables")
		consistency = core.ConsistencyLockedTables
	} else {
		args = append(args, "--single-transaction", "--skip-lock-tables")
		if len(caps.NonInnoDB) > 0 {
			// The snapshot covers the InnoDB tables only; say so in the
			// manifest rather than claiming more than was achieved.
			consistency = core.ConsistencyBestEffort
		}
	}

	// Recording the replication position runs FLUSH TABLES and then SHOW
	// MASTER STATUS. Missing either privilege makes the client abort mid-dump,
	// so the flag is only added when the probe confirmed both.
	if caps.LogBin && caps.CanFlush && caps.CanReadPosition {
		args = append(args, binlogPositionFlag(caps.Version)+"=2")
	}
	if caps.GTID && caps.Version.Flavor == FlavorMySQL {
		args = append(args, "--set-gtid-purged=ON")
	}

	return append(args, database), consistency
}

// binlogPositionFlag picks the spelling this client understands. MySQL renamed
// --master-data to --source-data in 8.0.26 and removed the old name in 8.4;
// MariaDB only ever had the old one.
func binlogPositionFlag(version serverVersion) string {
	if version.Flavor == FlavorMySQL && version.AtLeast(8, 0, 26) {
		return "--source-data"
	}
	return "--master-data"
}

// clientNames lists the binaries that can dump this flavor, best first.
func clientNames(flavor Flavor) []string {
	if flavor == FlavorMariaDB {
		// MariaDB renamed its client in 11.x; older packages still ship the
		// mysqldump name, and that one is MariaDB's, not Oracle's.
		return []string{"mariadb-dump", "mysqldump"}
	}
	return []string{"mysqldump"}
}
