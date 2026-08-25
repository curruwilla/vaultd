// Package mongodb dumps a MongoDB deployment by driving mongodump, and reads
// the server's own metadata to describe what it is dumping.
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
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RowEstimate selects how document counts are gathered (decision D7).
type RowEstimate string

const (
	RowsExact    RowEstimate = "exact"
	RowsEstimate RowEstimate = "estimate"
	RowsOff      RowEstimate = "off"
)

// defaultParallelCollections matches SPEC §4.1. It bounds how many
// collections mongodump reads at once, and therefore how much it buffers.
const defaultParallelCollections = 4

// Options configures one MongoDB target.
type Options struct {
	URI string
	// Oplog captures the operations that happen during the dump, making the
	// result consistent to a single point in time. It needs a replica set.
	Oplog                  bool
	RowEstimate            RowEstimate
	BinDir                 string
	NumParallelCollections int
	ProbeTimeout           time.Duration
}

// Dumper implements core.Dumper for MongoDB.
type Dumper struct {
	opts Options
	conn connInfo

	mu         sync.Mutex
	probed     *core.ServerInfo
	dump       engine.Binary
	useOplog   bool
	replicaSet string
}

// New parses the URI and returns a Dumper. It performs no network call.
func New(opts Options) (*Dumper, error) {
	conn, err := parseURI(opts.URI)
	if err != nil {
		return nil, fmt.Errorf("mongodb target: %w", err)
	}
	if opts.RowEstimate == "" {
		opts.RowEstimate = RowsEstimate
	}
	if opts.NumParallelCollections <= 0 {
		opts.NumParallelCollections = defaultParallelCollections
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Dumper{opts: opts, conn: conn}, nil
}

// Probe reads the server version, whether it is a replica set, and the
// collections that will be dumped.
func (d *Dumper) Probe(ctx context.Context) (core.ServerInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probe(ctx)
}

func (d *Dumper) probe(ctx context.Context) (core.ServerInfo, error) {
	if d.probed != nil {
		return *d.probed, nil
	}

	ctx, cancel := context.WithTimeout(ctx, d.opts.ProbeTimeout)
	defer cancel()

	deployment, collections, err := d.inspect(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}

	info := core.ServerInfo{
		Engine:     core.EngineMongoDB,
		Version:    deployment.Version,
		VersionNum: deployment.VersionNum,
		Tables:     collections,
	}

	d.replicaSet = deployment.ReplicaSet
	d.useOplog = d.opts.Oplog

	switch {
	case !d.opts.Oplog:
		info.Consistency = core.ConsistencyBestEffort
		if deployment.ReplicaSet != "" {
			info.Warnings = append(info.Warnings,
				"this is a replica set, so a point-in-time consistent dump is available; set options.oplog: true to use it")
		}
	case deployment.ReplicaSet == "":
		// Degrade rather than fail: a standalone server simply has no oplog to
		// read, and the operator asked for a backup, not for an argument.
		d.useOplog = false
		info.Consistency = core.ConsistencyBestEffort
		info.Warnings = append(info.Warnings,
			"oplog was requested but this server is standalone, not a replica set; the dump is consistent per collection only")
	case d.conn.Database != "":
		// mongodump refuses --oplog on anything but a full dump.
		d.useOplog = false
		info.Consistency = core.ConsistencyBestEffort
		info.Warnings = append(info.Warnings,
			"oplog was requested but the URI names a single database; mongodump only captures the oplog on a full dump")
	default:
		info.Consistency = core.ConsistencyPointInTime
	}

	binary, err := d.resolveClient(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}
	d.dump = binary

	d.probed = &info
	return info, nil
}

// Dump streams a mongodump archive to w.
func (d *Dumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	d.mu.Lock()
	info, err := d.probe(ctx)
	binary, useOplog := d.dump, d.useOplog
	d.mu.Unlock()
	if err != nil {
		return core.DumpResult{}, err
	}

	args := []string{
		"--archive", // one stream on stdout instead of a directory tree
		"--numParallelCollections=" + strconv.Itoa(d.opts.NumParallelCollections),
	}
	if useOplog {
		args = append(args, "--oplog")
	}
	if d.conn.Database != "" {
		args = append(args, "--db="+d.conn.Database)
	}
	// Compression is the pipeline's job: mongodump's own --gzip would produce
	// a stream we cannot checksum or encrypt uniformly with the others.

	credentials, cleanup, err := d.credentialArgs()
	if err != nil {
		return core.DumpResult{}, err
	}
	defer cleanup()

	// The path comes from our own resolver; the URI reaches the client through
	// an inherited file descriptor, never through argv.
	cmd := exec.CommandContext(ctx, binary.Path, append(args, credentials.args...)...) //nolint:gosec
	cmd.Env = engine.Env(nil)
	cmd.ExtraFiles = credentials.extraFiles
	cmd.Stdout = w
	cmd.WaitDelay = 15 * time.Second

	tail := engine.NewTail(engine.StderrTailBytes)
	cmd.Stderr = tail

	err = cmd.Run()
	stderr := d.conn.redact(tail.String())

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return core.DumpResult{}, &engine.ExitError{
				Binary:   binary.Name,
				Code:     exitErr.ExitCode(),
				Stderr:   stderr,
				Original: err,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return core.DumpResult{}, fmt.Errorf("%s was cancelled: %w", binary.Name, ctxErr)
		}
		return core.DumpResult{}, fmt.Errorf("running %s: %w", binary.Name, err)
	}

	result := core.DumpResult{
		Consistency:   info.Consistency,
		Tables:        info.Tables,
		DumperVersion: binary.String(),
		StderrTail:    stderr,
	}
	if useOplog {
		// The archive carries the oplog slice itself; what goes in the
		// manifest is where the server's oplog stood once the dump finished,
		// which is the point a future replay would start from. Failing to read
		// it is not a failed backup.
		head, err := d.oplogHead(ctx)
		if err != nil {
			return result, nil //nolint:nilerr // the backup itself succeeded
		}
		result.OplogEnd = head
	}
	return result, nil
}

// credentials describes how the connection URI is handed to the client.
type credentials struct {
	args       []string
	extraFiles []*os.File
}

// credentialArgs passes the URI through an inherited pipe rather than the
// command line.
//
// mongodump has no environment variable for credentials, and its --config file
// would put the password on disk. It does read /dev/fd/N, so the URI is written
// into a pipe the child inherits: never on disk, never in argv, gone when the
// process exits.
func (d *Dumper) credentialArgs() (credentials, func(), error) {
	return credentialArgs(d.conn)
}

func credentialArgs(conn connInfo) (credentials, func(), error) {
	if _, err := os.Stat("/dev/fd"); err != nil {
		// No /dev/fd (Windows, an unusual container): fall back to the command
		// line, which is the only remaining option this client offers. The
		// missing directory is the answer, not a failure to report.
		return credentials{args: []string{"--uri=" + conn.Raw}}, func() {}, nil //nolint:nilerr
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return credentials{}, nil, fmt.Errorf("preparing the connection handoff: %w", err)
	}

	go func() {
		// A YAML document with one key; mongodump reads it before connecting.
		_, _ = writer.WriteString("uri: " + conn.Raw + "\n")
		_ = writer.Close()
	}()

	cleanup := func() { _ = reader.Close() }
	// The child sees the first ExtraFiles entry as descriptor 3.
	return credentials{args: []string{"--config=/dev/fd/3"}, extraFiles: []*os.File{reader}}, cleanup, nil
}

// resolveClient finds mongodump. The database tools are versioned
// independently of the server (100.x), so there is no version rule to enforce:
// a current mongodump handles every supported server.
func (d *Dumper) resolveClient(ctx context.Context) (engine.Binary, error) {
	const name = "mongodump"

	var path string
	if d.opts.BinDir != "" {
		path = filepath.Join(d.opts.BinDir, name)
		if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return engine.Binary{}, fmt.Errorf("bin_dir %s holds no usable %s", d.opts.BinDir, name)
		}
	} else {
		found, err := exec.LookPath(name)
		if err != nil {
			return engine.Binary{}, fmt.Errorf(
				"%s is not installed (install mongodb-database-tools, or run the vaultd container image)", name)
		}
		path = found
	}

	return engine.ProbeBinary(ctx, name, path)
}

var _ core.Dumper = (*Dumper)(nil)
