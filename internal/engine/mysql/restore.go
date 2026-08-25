package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RestoreOptions configures a restore into one database.
type RestoreOptions struct {
	DSN          string
	Flavor       Flavor
	BinDir       string
	ProbeTimeout time.Duration
}

// Restorer feeds a SQL dump to the mysql or mariadb client.
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
	if opts.Flavor == "" {
		opts.Flavor = FlavorMySQL
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Restorer{opts: opts, conn: conn}, nil
}

// IsEmpty reports whether the destination database holds no tables or views.
func (r *Restorer) IsEmpty(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()

	db, err := sql.Open("mysql", r.conn.driverDSN)
	if err != nil {
		return false, fmt.Errorf("connecting to %s:%d/%s: %s",
			r.conn.Host, r.conn.Port, r.conn.Database, redactDriverError(err, r.conn.Password))
	}
	defer db.Close()

	var tables int
	err = db.QueryRowContext(ctx,
		"select count(*) from information_schema.tables where table_schema = ?", r.conn.Database).Scan(&tables)
	if err != nil {
		return false, fmt.Errorf("inspecting the destination: %s", redactDriverError(err, r.conn.Password))
	}
	return tables == 0, nil
}

// Restore streams a SQL dump into the destination database.
func (r *Restorer) Restore(ctx context.Context, src io.Reader) error {
	binary, err := r.resolveRestoreClient(ctx)
	if err != nil {
		return err
	}

	args := append(r.conn.args(), "--database="+r.conn.Database)

	// The path comes from our own resolver and every argument is built here;
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

// restoreClientNames lists the interactive clients that can apply a dump,
// best first. They mirror the dump clients: MariaDB's own, then the legacy
// name it still ships.
func restoreClientNames(flavor Flavor) []string {
	if flavor == FlavorMariaDB {
		return []string{"mariadb", "mysql"}
	}
	return []string{"mysql"}
}

func (r *Restorer) resolveRestoreClient(ctx context.Context) (engine.Binary, error) {
	var mismatched []string

	for _, name := range restoreClientNames(r.opts.Flavor) {
		for _, path := range candidatePaths(name, r.opts.BinDir) {
			binary, version, err := probeClient(ctx, name, path)
			if err != nil {
				continue
			}
			if version.Flavor != r.opts.Flavor {
				mismatched = append(mismatched, fmt.Sprintf("%s is %s's", path, version.Flavor))
				continue
			}
			return binary, nil
		}
	}

	packageHint := "mysql-client"
	if r.opts.Flavor == FlavorMariaDB {
		packageHint = "mariadb-client"
	}
	if len(mismatched) > 0 {
		return engine.Binary{}, fmt.Errorf("no %s client to restore with: %v (install %s)", r.opts.Flavor, mismatched, packageHint)
	}
	return engine.Binary{}, fmt.Errorf("no %s client to restore with; install %s, or run the vaultd container image",
		r.opts.Flavor, packageHint)
}

var _ core.Restorer = (*Restorer)(nil)
