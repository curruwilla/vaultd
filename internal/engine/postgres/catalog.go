package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/curruwilla/vaultd/internal/core"
)

// tableListQuery lists the ordinary and partitioned tables a dump will contain,
// with the planner's row estimate. reltuples is maintained by ANALYZE and
// costs nothing to read; it can be stale, which is why the manifest records
// whether a count is exact.
const tableListQuery = `
select n.nspname || '.' || c.relname as name,
       greatest(c.reltuples, 0)::bigint as rows
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p')
  and n.nspname not in ('pg_catalog', 'information_schema')
  and n.nspname not like 'pg_toast%'
order by 1
`

func (d *Dumper) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, d.conn.Raw)
	if err != nil {
		// pgx puts the connection string in its error; keep it out of ours.
		return nil, fmt.Errorf("connecting to %s:%d/%s: %s",
			d.conn.Host, d.conn.Port, d.conn.Database, d.connectFailure(err))
	}
	return conn, nil
}

// tables lists what will be dumped, with row counts gathered according to the
// configured strategy.
func (d *Dumper) tables(ctx context.Context, conn *pgx.Conn) ([]core.TableInfo, error) {
	rows, err := conn.Query(ctx, tableListQuery)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}

	var tables []core.TableInfo
	for rows.Next() {
		var t core.TableInfo
		if err := rows.Scan(&t.Name, &t.Rows); err != nil {
			return nil, fmt.Errorf("listing tables: %w", err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}

	switch d.opts.RowEstimate {
	case RowsOff:
		for i := range tables {
			tables[i].Rows = 0
		}
	case RowsExact:
		if err := d.countExactly(ctx, conn, tables); err != nil {
			return nil, err
		}
	default: // RowsEstimate
		for i := range tables {
			tables[i].RowsExact = false
		}
	}
	return tables, nil
}

// countExactly runs count(*) per table. It is opt-in for a reason: on a large
// database this reads every heap page.
func (d *Dumper) countExactly(ctx context.Context, conn *pgx.Conn, tables []core.TableInfo) error {
	for i := range tables {
		schema, table, err := splitQualified(tables[i].Name)
		if err != nil {
			return err
		}

		// The identifier comes from the catalog, and is quoted anyway: a table
		// named `users"; drop table x` is legal in PostgreSQL.
		query := "select count(*) from " + pgx.Identifier{schema, table}.Sanitize()

		var count int64
		if err := conn.QueryRow(ctx, query).Scan(&count); err != nil {
			return fmt.Errorf("counting rows of %s: %w", tables[i].Name, err)
		}
		tables[i].Rows = count
		tables[i].RowsExact = true
	}
	return nil
}

func splitQualified(name string) (schema, table string, err error) {
	for i := range len(name) {
		if name[i] == '.' {
			return name[:i], name[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("table name %q is not schema-qualified", name)
}

// connectFailure reduces a driver error to its message. pgx echoes the
// connection string it was given, so the password is stripped before the text
// reaches a log line or a webhook (SPEC §15).
func (d *Dumper) connectFailure(err error) string {
	return redactDriverError(err, d.conn.Password)
}

// redactDriverError strips a password out of a driver error and trims it to
// something a log line can carry.
func redactDriverError(err error, password string) string {
	msg := err.Error()
	if password != "" {
		msg = strings.ReplaceAll(msg, password, "***")
	}
	if len(msg) > 300 {
		return msg[:300] + "…"
	}
	return msg
}
