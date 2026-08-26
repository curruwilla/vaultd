package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql driver

	"github.com/curruwilla/vaultd/internal/core"
)

// ProvisionOptions configures the staging server restore verification uses
// (SPEC §8, decision D3).
type ProvisionOptions struct {
	// DSN is the administrative connection. The database it names is only ever
	// used to create and drop others.
	DSN    string
	Flavor Flavor
	// Prefix is the one namespace this provisioner may create and drop in.
	Prefix       string
	BinDir       string
	ProbeTimeout time.Duration
}

// Provisioner creates and drops ephemeral databases on a MySQL or MariaDB
// verify target.
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
	if opts.Flavor == "" {
		opts.Flavor = FlavorMySQL
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Provisioner{opts: opts, conn: conn}, nil
}

// Probe reads the staging server's version and fork. A server of the other
// fork is refused here rather than halfway through a restore: the dump was
// written by the other client and does not always apply.
func (p *Provisioner) Probe(ctx context.Context) (core.ServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	db, err := p.open(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}
	defer db.Close()

	var versionRaw, comment string
	if err := db.QueryRowContext(ctx, "select version()").Scan(&versionRaw); err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the verify target's version: %s", p.failure(err))
	}
	_ = db.QueryRowContext(ctx, "select @@version_comment").Scan(&comment)

	version, err := parseServerVersion(versionRaw, comment)
	if err != nil {
		return core.ServerInfo{}, err
	}
	if version.Flavor != p.opts.Flavor {
		return core.ServerInfo{}, fmt.Errorf(
			"the verify target is declared as %s but the server reports %s %s", p.opts.Flavor, version.Flavor, version)
	}

	return core.ServerInfo{
		Engine:     engineOf(version.Flavor),
		Version:    version.String(),
		VersionNum: version.Num(),
	}, nil
}

// Create makes an empty database and returns it as a sandbox.
func (p *Provisioner) Create(ctx context.Context, spec core.SandboxSpec) (core.Sandbox, error) {
	if err := p.guard(spec.Name); err != nil {
		return nil, err
	}

	created, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	db, err := p.open(created)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// The name is ours — a prefix from the config plus a backup id — and it is
	// quoted anyway, because a create statement takes no placeholders.
	if _, err := db.ExecContext(created, "create database "+quoteIdentifier(spec.Name)); err != nil {
		return nil, fmt.Errorf("creating the verify database %s: %s", spec.Name, p.failure(err))
	}

	conn, err := p.conn.withDatabase(spec.Name)
	if err != nil {
		_ = p.Drop(ctx, spec.Name)
		return nil, err
	}
	restorer, err := NewRestorer(RestoreOptions{
		DSN:          conn.driverDSN,
		Flavor:       p.opts.Flavor,
		BinDir:       p.opts.BinDir,
		ProbeTimeout: p.opts.ProbeTimeout,
	})
	if err != nil {
		_ = p.Drop(ctx, spec.Name)
		return nil, err
	}

	return &sandbox{Restorer: restorer, provisioner: p, name: spec.Name, conn: conn}, nil
}

// List names the sandbox databases the server holds right now.
func (p *Provisioner) List(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	db, err := p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// left() rather than LIKE: a prefix such as `vaultd_verify_` is full of
	// underscores, and LIKE reads every one of them as a wildcard.
	rows, err := db.QueryContext(ctx,
		"select schema_name from information_schema.schemata where left(schema_name, char_length(?)) = ? order by 1",
		p.opts.Prefix, p.opts.Prefix)
	if err != nil {
		return nil, fmt.Errorf("listing the verify databases: %s", p.failure(err))
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

	db, err := p.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "drop database if exists "+quoteIdentifier(name)); err != nil {
		return fmt.Errorf("dropping the verify database %s: %s", name, p.failure(err))
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
	case strings.EqualFold(name, p.conn.Database):
		return fmt.Errorf("refusing to touch database %q: it is the verify target's own connection", name)
	}
	return nil
}

func (p *Provisioner) open(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", p.conn.driverDSN)
	if err != nil {
		return nil, p.connectFailure(err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, p.connectFailure(err)
	}
	return db, nil
}

func (p *Provisioner) connectFailure(err error) error {
	return fmt.Errorf("connecting to the verify target %s:%d/%s: %s",
		p.conn.Host, p.conn.Port, p.conn.Database, p.failure(err))
}

func (p *Provisioner) failure(err error) string {
	return redactDriverError(err, p.conn.Password)
}

// sandbox is one ephemeral database: the restorer that writes into it, plus
// the queries the assertions read it back with.
type sandbox struct {
	*Restorer
	provisioner *Provisioner
	name        string
	conn        connInfo
}

func (s *sandbox) Name() string { return s.name }

func (s *sandbox) Drop(ctx context.Context) error { return s.provisioner.Drop(ctx, s.name) }

func (s *sandbox) Tables(ctx context.Context) ([]string, error) {
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		"select table_name from information_schema.tables where table_schema = ? and table_type = 'BASE TABLE' order by 1",
		s.name)
	if err != nil {
		return nil, fmt.Errorf("listing the restored tables: %s", s.provisioner.failure(err))
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

// CountRows counts one restored table. The count is exact: information_schema
// keeps a sampled estimate that can be off by half, which would measure
// nothing about a restore.
func (s *sandbox) CountRows(ctx context.Context, table string) (int64, error) {
	db, err := s.open(ctx)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	// A MySQL manifest names tables without a schema; one written by hand may
	// still qualify it, and the qualifier can only be the database itself.
	if _, unqualified, found := strings.Cut(table, "."); found {
		table = unqualified
	}

	var count int64
	query := "select count(*) from " + quoteIdentifier(s.name) + "." + quoteIdentifier(table)
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting rows of %s: %s", table, s.provisioner.failure(err))
	}
	return count, nil
}

// Scalar runs a `query` assertion's SQL and returns its single value.
func (s *sandbox) Scalar(ctx context.Context, query string) (any, error) {
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// The SQL is the operator's own, from the config file; it runs against a
	// database that exists only for this check and is dropped afterwards.
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("running the assertion query: %s", s.provisioner.failure(err))
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading the assertion query result: %w", err)
	}
	if len(columns) != 1 {
		return nil, fmt.Errorf("the assertion query returned %d columns; it has to return one value", len(columns))
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("running the assertion query: %s", s.provisioner.failure(err))
		}
		return nil, errors.New("the assertion query returned no rows; it has to return one value")
	}

	var value any
	if err := rows.Scan(&value); err != nil {
		return nil, fmt.Errorf("reading the assertion query result: %w", err)
	}
	return value, nil
}

func (s *sandbox) open(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", s.conn.driverDSN)
	if err != nil {
		return nil, fmt.Errorf("connecting to the verify database %s: %s", s.name, s.provisioner.failure(err))
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to the verify database %s: %s", s.name, s.provisioner.failure(err))
	}
	return db, nil
}

var (
	_ core.Provisioner = (*Provisioner)(nil)
	_ core.Sandbox     = (*sandbox)(nil)
)
