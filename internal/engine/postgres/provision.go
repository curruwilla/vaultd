package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/curruwilla/vaultd/internal/core"
)

// ProvisionOptions configures the staging server restore verification uses
// (SPEC §8, decision D3).
type ProvisionOptions struct {
	// DSN is the administrative connection. The database it names is only ever
	// connected to in order to create and drop others — `postgres` is the
	// usual choice.
	DSN string
	// Prefix is the one namespace this provisioner may create and drop in.
	// Without it nothing happens at all: it is what keeps a verification away
	// from a staging database somebody cares about.
	Prefix       string
	BinDir       string
	ProbeTimeout time.Duration
}

// Provisioner creates and drops ephemeral databases on a PostgreSQL verify
// target.
type Provisioner struct {
	opts ProvisionOptions
	conn connInfo
}

// NewProvisioner parses the administrative connection string. It opens no
// connection.
func NewProvisioner(opts ProvisionOptions) (*Provisioner, error) {
	if strings.TrimSpace(opts.Prefix) == "" {
		return nil, errors.New("verify target: no database_prefix; vaultd refuses to create or drop databases without one")
	}
	conn, err := parseDSN(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("verify target: %w", err)
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Provisioner{opts: opts, conn: conn}, nil
}

// Probe reads the staging server's version and confirms the credentials may
// create a database at all. Finding that out here costs one query; finding it
// out after downloading a 400GB backup costs the whole window.
func (p *Provisioner) Probe(ctx context.Context) (core.ServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	conn, err := p.connect(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}
	defer conn.Close(ctx)

	info := core.ServerInfo{Engine: core.EnginePostgres}
	if err := conn.QueryRow(ctx, "select current_setting('server_version')").Scan(&info.Version); err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the verify target's version: %w", err)
	}
	if err := conn.QueryRow(ctx, "select current_setting('server_version_num')::int").Scan(&info.VersionNum); err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the verify target's version: %w", err)
	}

	var canCreate bool
	err = conn.QueryRow(ctx,
		"select rolcreatedb or rolsuper from pg_roles where rolname = current_user").Scan(&canCreate)
	if err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the verify target's privileges: %w", err)
	}
	if !canCreate {
		return core.ServerInfo{}, fmt.Errorf(
			"the verify target's user %q cannot create databases; grant CREATEDB (and nothing more)", p.conn.User)
	}

	return info, nil
}

// Create makes an empty database and returns it as a sandbox.
func (p *Provisioner) Create(ctx context.Context, spec core.SandboxSpec) (core.Sandbox, error) {
	if err := p.guard(spec.Name); err != nil {
		return nil, err
	}

	created, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	conn, err := p.connect(created)
	if err != nil {
		return nil, err
	}
	defer conn.Close(created)

	// CREATE DATABASE cannot run inside a transaction block, which is why this
	// connects to the maintenance database rather than reusing anything.
	if _, err := conn.Exec(created, "create database "+pgx.Identifier{spec.Name}.Sanitize()); err != nil {
		return nil, fmt.Errorf("creating the verify database %s: %s", spec.Name, redactDriverError(err, p.conn.Password))
	}

	dsn := p.conn.withDatabaseDSN(spec.Name)
	restorer, err := NewRestorer(RestoreOptions{
		DSN:          dsn,
		BinDir:       p.opts.BinDir,
		ProbeTimeout: p.opts.ProbeTimeout,
	})
	if err != nil {
		// The database exists but is unusable; leaving it behind would make
		// the next --gc the only way to clean up.
		_ = p.Drop(ctx, spec.Name)
		return nil, err
	}

	return &sandbox{Restorer: restorer, provisioner: p, name: spec.Name, dsn: dsn}, nil
}

// List names the sandbox databases the server holds right now.
func (p *Provisioner) List(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	conn, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	// starts_with rather than LIKE: a prefix such as `vaultd_verify_` is full
	// of underscores, and LIKE would read every one of them as a wildcard.
	rows, err := conn.Query(ctx,
		"select datname from pg_database where starts_with(datname, $1) and not datistemplate order by 1",
		p.opts.Prefix)
	if err != nil {
		return nil, fmt.Errorf("listing the verify databases: %s", redactDriverError(err, p.conn.Password))
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("listing the verify databases: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the verify databases: %w", err)
	}
	return names, nil
}

// Drop removes one sandbox database. It is idempotent, and it refuses any name
// outside the configured prefix.
func (p *Provisioner) Drop(ctx context.Context, name string) error {
	if err := p.guard(name); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	conn, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	identifier := pgx.Identifier{name}.Sanitize()

	// FORCE disconnects whatever a crashed pg_restore left attached; without
	// it one stuck connection keeps the database — and its disk — forever.
	if _, err := conn.Exec(ctx, "drop database if exists "+identifier+" with (force)"); err != nil {
		if _, plain := conn.Exec(ctx, "drop database if exists "+identifier); plain != nil {
			return fmt.Errorf("dropping the verify database %s: %s", name, redactDriverError(err, p.conn.Password))
		}
	}
	return nil
}

// guard is the whole safety story of a verify target: vaultd creates and drops
// databases on a live server, and it only ever touches names carrying the
// prefix the config declared (SPEC §8).
func (p *Provisioner) guard(name string) error {
	switch {
	case !strings.HasPrefix(name, p.opts.Prefix), name == p.opts.Prefix:
		return fmt.Errorf(
			"refusing to touch database %q: a verify target only ever creates and drops databases named %s…",
			name, p.opts.Prefix)
	case name == p.conn.Database:
		return fmt.Errorf("refusing to touch database %q: it is the verify target's own connection", name)
	}
	return nil
}

func (p *Provisioner) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, p.conn.Raw)
	if err != nil {
		return nil, fmt.Errorf("connecting to the verify target %s:%d/%s: %s",
			p.conn.Host, p.conn.Port, p.conn.Database, redactDriverError(err, p.conn.Password))
	}
	return conn, nil
}

// sandbox is one ephemeral database: the restorer that writes into it, plus
// the queries the assertions read it back with.
type sandbox struct {
	*Restorer
	provisioner *Provisioner
	name        string
	dsn         string
}

func (s *sandbox) Name() string { return s.name }

func (s *sandbox) Drop(ctx context.Context) error { return s.provisioner.Drop(ctx, s.name) }

// sandboxTablesQuery lists what a restore put there, named the way the
// manifest names it.
const sandboxTablesQuery = `
select n.nspname || '.' || c.relname
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p')
  and n.nspname not in ('pg_catalog', 'information_schema')
  and n.nspname not like 'pg_toast%'
order by 1
`

func (s *sandbox) Tables(ctx context.Context) ([]string, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, sandboxTablesQuery)
	if err != nil {
		return nil, fmt.Errorf("listing the restored tables: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("listing the restored tables: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the restored tables: %w", err)
	}
	return names, nil
}

// CountRows counts one restored table. The count is exact: a database that was
// restored a minute ago has never been analyzed, so its planner estimates are
// worth nothing.
func (s *sandbox) CountRows(ctx context.Context, table string) (int64, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)

	schema, name := splitTable(table)

	// The identifier is quoted rather than interpolated: it comes from the
	// config, and a table may legally be called `users"; drop table x`.
	var count int64
	query := "select count(*) from " + pgx.Identifier{schema, name}.Sanitize()
	if err := conn.QueryRow(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting rows of %s: %s", table, redactDriverError(err, s.provisioner.conn.Password))
	}
	return count, nil
}

// Scalar runs a `query` assertion's SQL and returns its single value.
func (s *sandbox) Scalar(ctx context.Context, query string) (any, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	// The SQL is the operator's own, from the config file; it runs against a
	// database that exists only for this check and is dropped afterwards.
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("running the assertion query: %s", redactDriverError(err, s.provisioner.conn.Password))
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("running the assertion query: %s", redactDriverError(err, s.provisioner.conn.Password))
		}
		return nil, errors.New("the assertion query returned no rows; it has to return one value")
	}

	values, err := rows.Values()
	if err != nil {
		return nil, fmt.Errorf("reading the assertion query result: %w", err)
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("the assertion query returned %d columns; it has to return one value", len(values))
	}
	return values[0], nil
}

func (s *sandbox) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to the verify database %s: %s",
			s.name, redactDriverError(err, s.provisioner.conn.Password))
	}
	return conn, nil
}

// splitTable reads a manifest table name. PostgreSQL manifests always qualify
// with a schema, but an assertion written by hand often says just `users`, and
// that means what it says everywhere else in PostgreSQL: public.
func splitTable(name string) (schema, table string) {
	if schema, table, found := strings.Cut(name, "."); found {
		return schema, table
	}
	return "public", name
}

var (
	_ core.Provisioner = (*Provisioner)(nil)
	_ core.Sandbox     = (*sandbox)(nil)
)
