package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RestoreOptions configures a restore into one database.
type RestoreOptions struct {
	DSN    string
	BinDir string
	// Clean drops the objects a restore is about to recreate. Without it,
	// restoring over existing objects fails, which is the safer default.
	Clean        bool
	ProbeTimeout time.Duration
}

// Restorer feeds a custom-format archive to pg_restore.
type Restorer struct {
	opts RestoreOptions
	conn connInfo
}

// NewRestorer parses the destination connection string. It opens no
// connection.
func NewRestorer(opts RestoreOptions) (*Restorer, error) {
	conn, err := parseDSN(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("restore destination: %w", err)
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Restorer{opts: opts, conn: conn}, nil
}

// emptyQuery counts the tables a restore would collide with. System and
// extension-owned schemas do not count: a fresh database created from
// template1 is still empty for this purpose.
const emptyQuery = `
select count(*)
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p', 'S', 'v', 'm')
  and n.nspname not in ('pg_catalog', 'information_schema')
  and n.nspname not like 'pg_toast%'
  and n.nspname not like 'pg_temp%'
`

// IsEmpty reports whether the destination holds no user objects.
func (r *Restorer) IsEmpty(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, r.conn.Raw)
	if err != nil {
		return false, fmt.Errorf("connecting to %s:%d/%s: %s",
			r.conn.Host, r.conn.Port, r.conn.Database, redactDriverError(err, r.conn.Password))
	}
	defer conn.Close(ctx)

	var objects int
	if err := conn.QueryRow(ctx, emptyQuery).Scan(&objects); err != nil {
		return false, fmt.Errorf("inspecting the destination: %w", err)
	}
	return objects == 0, nil
}

// Restore streams an archive into the destination database.
//
// The restore runs in one transaction and stops at the first error, so a
// half-applied schema is never left behind: either the database has the backup
// in it or it has nothing new.
func (r *Restorer) Restore(ctx context.Context, src io.Reader) error {
	serverMajor, err := r.serverMajor(ctx)
	if err != nil {
		return err
	}

	binary, err := resolveBinary(ctx, "pg_restore", serverMajor, r.opts.BinDir)
	if err != nil {
		return err
	}

	args := []string{
		"--dbname=" + r.conn.Database,
		"--no-owner",
		"--no-acl",
		"--single-transaction",
		"--exit-on-error",
		"--no-password",
	}
	if r.opts.Clean {
		args = append(args, "--clean", "--if-exists")
	}

	// The path comes from our own resolver and the arguments are built here;
	// the password travels in the environment.
	cmd := exec.CommandContext(ctx, binary.Path, args...) //nolint:gosec
	cmd.Env = engine.Env(r.conn.env())
	cmd.Stdin = src
	cmd.WaitDelay = 15 * time.Second

	tail := engine.NewTail(engine.StderrTailBytes)
	cmd.Stderr = tail

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &engine.ExitError{
				Binary:   binary.Name,
				Code:     exitErr.ExitCode(),
				Stderr:   tail.String(),
				Original: err,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s was cancelled: %w", binary.Name, ctxErr)
		}
		return fmt.Errorf("running %s: %w", binary.Name, err)
	}
	return nil
}

func (r *Restorer) serverMajor(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, r.conn.Raw)
	if err != nil {
		return 0, fmt.Errorf("connecting to %s:%d/%s: %s",
			r.conn.Host, r.conn.Port, r.conn.Database, redactDriverError(err, r.conn.Password))
	}
	defer conn.Close(ctx)

	var versionNum int
	if err := conn.QueryRow(ctx, "select current_setting('server_version_num')::int").Scan(&versionNum); err != nil {
		return 0, fmt.Errorf("reading the server version: %w", err)
	}
	return versionNum / 10000, nil
}

var _ core.Restorer = (*Restorer)(nil)
