package mongodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RestoreOptions configures a restore into one deployment.
type RestoreOptions struct {
	URI    string
	BinDir string
	// Drop removes each collection before it is restored. Without it,
	// mongorestore merges into whatever is already there, which is rarely what
	// a restore means.
	Drop                   bool
	NumParallelCollections int
	ProbeTimeout           time.Duration
	// NSFrom and NSTo rename namespaces on the way in. mongorestore writes
	// every collection back into the namespace the archive records, so
	// restoring somewhere else — the ephemeral database a verification uses —
	// is a rename rather than a destination.
	NSFrom string
	NSTo   string
}

// Restorer feeds a mongodump archive to mongorestore.
type Restorer struct {
	opts RestoreOptions
	conn connInfo
}

// NewRestorer parses the destination URI. It opens no connection.
func NewRestorer(opts RestoreOptions) (*Restorer, error) {
	conn, err := parseURI(opts.URI)
	if err != nil {
		return nil, fmt.Errorf("restore destination: %w", err)
	}
	if opts.NumParallelCollections <= 0 {
		opts.NumParallelCollections = defaultParallelCollections
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Restorer{opts: opts, conn: conn}, nil
}

// IsEmpty reports whether the destination holds no user collections. When the
// URI names a database, only that one is inspected; otherwise the whole
// deployment is, because that is what the archive would be restored into.
func (r *Restorer) IsEmpty(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(r.conn.Raw))
	if err != nil {
		return false, fmt.Errorf("connecting to %s: %s", r.conn.Hosts, r.conn.redact(err.Error()))
	}
	defer func() { _ = client.Disconnect(ctx) }()

	databases := []string{r.conn.Database}
	if r.conn.Database == "" {
		names, err := client.ListDatabaseNames(ctx, bson.D{})
		if err != nil {
			return false, fmt.Errorf("listing databases: %s", r.conn.redact(err.Error()))
		}
		databases = nil
		for _, name := range names {
			if !systemDatabases[name] {
				databases = append(databases, name)
			}
		}
	}

	for _, name := range databases {
		collections, err := client.Database(name).ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return false, fmt.Errorf("listing collections of %s: %s", name, r.conn.redact(err.Error()))
		}
		if len(collections) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Restore streams an archive into the destination deployment.
//
// mongorestore writes each collection back into the namespace the archive
// records, so the destination URI selects the server, not a new name for the
// data.
func (r *Restorer) Restore(ctx context.Context, src io.Reader) error {
	binary, err := r.resolveClient(ctx)
	if err != nil {
		return err
	}

	args := []string{
		"--archive",
		"--numParallelCollections=" + strconv.Itoa(r.opts.NumParallelCollections),
	}
	if r.opts.Drop {
		args = append(args, "--drop")
	}
	if r.opts.NSFrom != "" && r.opts.NSTo != "" {
		args = append(args, "--nsFrom="+r.opts.NSFrom, "--nsTo="+r.opts.NSTo)
	}

	credentials, cleanup, err := credentialArgs(r.conn)
	if err != nil {
		return err
	}
	defer cleanup()

	// The path comes from our own resolver; the URI reaches the client through
	// an inherited file descriptor, never through argv.
	cmd := exec.CommandContext(ctx, binary.Path, append(args, credentials.args...)...) //nolint:gosec
	cmd.Env = engine.Env(nil)
	cmd.ExtraFiles = credentials.extraFiles
	cmd.Stdin = src
	cmd.WaitDelay = 15 * time.Second

	tail := engine.NewTail(engine.StderrTailBytes)
	cmd.Stderr = tail

	if err := cmd.Run(); err != nil {
		stderr := r.conn.redact(tail.String())

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &engine.ExitError{
				Binary:   binary.Name,
				Code:     exitErr.ExitCode(),
				Stderr:   stderr,
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

func (r *Restorer) resolveClient(ctx context.Context) (engine.Binary, error) {
	const name = "mongorestore"

	if r.opts.BinDir != "" {
		path := filepath.Join(r.opts.BinDir, name)
		if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return engine.Binary{}, fmt.Errorf("bin_dir %s holds no usable %s", r.opts.BinDir, name)
		}
		return engine.ProbeBinary(ctx, name, path)
	}

	path, err := exec.LookPath(name)
	if err != nil {
		return engine.Binary{}, fmt.Errorf(
			"%s is not installed (install mongodb-database-tools, or run the vaultd container image)", name)
	}
	return engine.ProbeBinary(ctx, name, path)
}

var _ core.Restorer = (*Restorer)(nil)
