package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql" // database/sql driver

	"github.com/curruwilla/vaultd/internal/core"
)

// transactionalEngines are the storage engines whose tables --single-transaction
// really does snapshot. Everything else needs a lock to be consistent.
var transactionalEngines = map[string]bool{
	"innodb":  true,
	"rocksdb": true,
}

const tableListQuery = `
select table_name, coalesce(engine, ''), coalesce(table_rows, 0)
from information_schema.tables
where table_schema = ? and table_type = 'BASE TABLE'
order by table_name
`

// inspect gathers everything the probe needs in one connection.
func (d *Dumper) inspect(ctx context.Context) (capabilities, []core.TableInfo, error) {
	db, err := d.open(ctx)
	if err != nil {
		return capabilities{}, nil, err
	}
	defer db.Close()

	var caps capabilities

	var versionRaw, comment string
	if err := db.QueryRowContext(ctx, "select version()").Scan(&versionRaw); err != nil {
		return capabilities{}, nil, fmt.Errorf("reading the server version: %w", err)
	}
	// version_comment is where some MariaDB builds identify themselves.
	_ = db.QueryRowContext(ctx, "select @@version_comment").Scan(&comment)

	caps.Version, err = parseServerVersion(versionRaw, comment)
	if err != nil {
		return capabilities{}, nil, err
	}

	caps.LogBin = boolSetting(ctx, db, "@@log_bin")
	caps.CanFlush, caps.CanReadPosition = grants(ctx, db)
	// MariaDB has no gtid_mode at all, so the query failing is an answer.
	caps.GTID = gtidEnabled(ctx, db)

	tables, nonInnoDB, err := d.tables(ctx, db)
	if err != nil {
		return capabilities{}, nil, err
	}
	caps.NonInnoDB = nonInnoDB

	return caps, tables, nil
}

func (d *Dumper) open(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", d.conn.driverDSN)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s:%d/%s: %s", d.conn.Host, d.conn.Port, d.conn.Database, d.failure(err))
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to %s:%d/%s: %s", d.conn.Host, d.conn.Port, d.conn.Database, d.failure(err))
	}
	return db, nil
}

func (d *Dumper) tables(ctx context.Context, db *sql.DB) ([]core.TableInfo, []string, error) {
	rows, err := db.QueryContext(ctx, tableListQuery, d.conn.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("listing tables: %w", err)
	}
	defer rows.Close()

	var (
		tables    []core.TableInfo
		nonInnoDB []string
	)
	for rows.Next() {
		var (
			name        string
			storeEngine string
			estimate    int64
		)
		if err := rows.Scan(&name, &storeEngine, &estimate); err != nil {
			return nil, nil, fmt.Errorf("listing tables: %w", err)
		}

		table := core.TableInfo{Name: name, StorageEngine: storeEngine}
		if d.opts.RowEstimate == RowsEstimate {
			// information_schema keeps a sampled estimate for InnoDB; it can
			// be off by a wide margin, which is why the manifest records
			// whether a count is exact.
			table.Rows = estimate
		}
		tables = append(tables, table)

		if storeEngine != "" && !transactionalEngines[strings.ToLower(storeEngine)] {
			nonInnoDB = append(nonInnoDB, name+" ("+storeEngine+")")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing tables: %w", err)
	}

	if d.opts.RowEstimate == RowsExact {
		if err := d.countExactly(ctx, db, tables); err != nil {
			return nil, nil, err
		}
	}
	return tables, nonInnoDB, nil
}

// countExactly runs count(*) per table. Opt-in: on a large database this reads
// every row.
func (d *Dumper) countExactly(ctx context.Context, db *sql.DB, tables []core.TableInfo) error {
	for i := range tables {
		query := "select count(*) from " + quoteIdentifier(d.conn.Database) + "." + quoteIdentifier(tables[i].Name)

		var count int64
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return fmt.Errorf("counting rows of %s: %w", tables[i].Name, err)
		}
		tables[i].Rows = count
		tables[i].RowsExact = true
	}
	return nil
}

// quoteIdentifier renders a name MySQL will read back as one identifier. The
// names come from the catalog, and a table may legally be called `weird“ one`.
func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func boolSetting(ctx context.Context, db *sql.DB, name string) bool {
	var value string
	if err := db.QueryRowContext(ctx, "select "+name).Scan(&value); err != nil {
		return false
	}
	switch strings.ToUpper(value) {
	case "1", "ON", "TRUE":
		return true
	default:
		return false
	}
}

// grants reads what the backup user may actually do.
//
// A least-privilege user often holds neither privilege, and the dump has to
// adapt rather than fail halfway through: mysqldump only discovers the problem
// after it has already written part of the stream.
func grants(ctx context.Context, db *sql.DB) (canFlush, canReadPosition bool) {
	rows, err := db.QueryContext(ctx, "show grants for current_user()")
	if err != nil {
		return false, false
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return false, false
		}
		lines = append(lines, grant)
	}
	if rows.Err() != nil {
		// A partial answer would overstate what the user can do, and the safe
		// reading of "unknown" is "cannot".
		return false, false
	}
	return parseGrants(lines)
}

// parseGrants interprets SHOW GRANTS output.
//
// Only global grants count. RELOAD and REPLICATION CLIENT exist at the server
// level only, so `GRANT ALL PRIVILEGES ON `app`.*` — what a per-database
// application user typically has — confers neither, however broad it looks.
func parseGrants(lines []string) (canFlush, canReadPosition bool) {
	for _, line := range lines {
		upper := strings.ToUpper(line)

		scope, _, found := strings.Cut(upper, " TO ")
		if !found {
			scope = upper
		}
		if !strings.Contains(scope, " ON *.*") {
			continue
		}

		if strings.Contains(scope, "ALL PRIVILEGES") || strings.Contains(scope, "SUPER") {
			return true, true
		}
		if strings.Contains(scope, "RELOAD") || strings.Contains(scope, "FLUSH_TABLES") {
			canFlush = true
		}
		// MariaDB 10.5 renamed REPLICATION CLIENT to BINLOG MONITOR and kept
		// the old name as an alias.
		if strings.Contains(scope, "REPLICATION CLIENT") ||
			strings.Contains(scope, "REPLICATION_CLIENT") ||
			strings.Contains(scope, "BINLOG MONITOR") ||
			strings.Contains(scope, "BINLOG_MONITOR") {
			canReadPosition = true
		}
	}
	return canFlush, canReadPosition
}

func gtidEnabled(ctx context.Context, db *sql.DB) bool {
	var mode string
	if err := db.QueryRowContext(ctx, "select @@gtid_mode").Scan(&mode); err != nil {
		return false
	}
	mode = strings.ToUpper(mode)
	return mode == "ON" || mode == "ON_PERMISSIVE"
}

// failure reduces a driver error to its message with the password removed: the
// driver echoes the DSN it was given.
func (d *Dumper) failure(err error) string {
	msg := err.Error()
	if d.conn.Password != "" {
		msg = strings.ReplaceAll(msg, d.conn.Password, "***")
	}
	if len(msg) > 300 {
		return msg[:300] + "…"
	}
	return msg
}
