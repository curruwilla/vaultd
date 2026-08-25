// Package postgres dumps a PostgreSQL database by driving pg_dump, and reads
// the catalog directly to describe what it is about to dump.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RowEstimate selects how the row counts in a manifest are gathered. The
// default is `estimate` (decision D7): catalog statistics cost nothing, while
// count(*) on a large database can take longer than the backup itself.
type RowEstimate string

const (
	RowsExact    RowEstimate = "exact"
	RowsEstimate RowEstimate = "estimate"
	RowsOff      RowEstimate = "off"
)

// Options configures one PostgreSQL target.
type Options struct {
	DSN string
	// ExcludeTableData drops the rows of matching tables while keeping their
	// schema — audit logs and session tables, typically.
	ExcludeTableData []string
	// IncludeGlobals dumps roles and tablespaces as a separate object.
	IncludeGlobals bool
	RowEstimate    RowEstimate
	// BinDir pins the client directory instead of resolving one by version.
	BinDir string
	// ProbeTimeout bounds the catalog queries, not the dump itself.
	ProbeTimeout time.Duration
}

// Dumper implements core.Dumper for PostgreSQL.
type Dumper struct {
	opts Options
	conn connInfo

	mu     sync.Mutex
	probed *core.ServerInfo
	dump   engine.Binary
}

// New parses the connection string and returns a Dumper. It performs no
// network call.
func New(opts Options) (*Dumper, error) {
	conn, err := parseDSN(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres target: %w", err)
	}
	if opts.RowEstimate == "" {
		opts.RowEstimate = RowsEstimate
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Dumper{opts: opts, conn: conn}, nil
}

// Probe reads the server version, resolves a compatible pg_dump and lists the
// tables that will be dumped. Everything a manifest needs to be written, and
// every reason to refuse the backup, is known after this call.
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

	conn, err := d.connect(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}
	defer conn.Close(ctx)

	info := core.ServerInfo{Engine: core.EnginePostgres, Consistency: core.ConsistencySerializableSnapshot}

	if err := conn.QueryRow(ctx, "select current_setting('server_version')").Scan(&info.Version); err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the server version: %w", err)
	}
	if err := conn.QueryRow(ctx, "select current_setting('server_version_num')::int").Scan(&info.VersionNum); err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the server version: %w", err)
	}

	tables, err := d.tables(ctx, conn)
	if err != nil {
		return core.ServerInfo{}, err
	}
	info.Tables = tables

	binary, err := d.resolveBinary(ctx, "pg_dump", info.VersionNum/10000)
	if err != nil {
		return core.ServerInfo{}, err
	}
	d.dump = binary

	d.probed = &info
	return info, nil
}

// Dump streams a custom-format dump to w.
//
// The flags are fixed by design (SPEC §4.1): custom format so a restore can be
// selective and parallel, no internal compression because the pipeline
// compresses better and needs one uniform checksum, and a deferrable
// serializable snapshot so the dump is consistent without blocking writers.
func (d *Dumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	d.mu.Lock()
	info, err := d.probe(ctx)
	binary := d.dump
	d.mu.Unlock()
	if err != nil {
		return core.DumpResult{}, err
	}

	args := []string{
		"--format=custom",
		"--compress=0",
		"--no-owner",
		"--no-acl",
		"--serializable-deferrable",
		// Never prompt: a daemon has no terminal, and a prompt would hang
		// until the phase timeout instead of failing with a clear message.
		"--no-password",
	}
	for _, glob := range d.opts.ExcludeTableData {
		args = append(args, "--exclude-table-data="+glob)
	}

	stderr, err := d.run(ctx, binary, args, w)
	if err != nil {
		return core.DumpResult{}, err
	}

	return core.DumpResult{
		Consistency:   core.ConsistencySerializableSnapshot,
		Tables:        info.Tables,
		DumperVersion: binary.String(),
		StderrTail:    stderr,
	}, nil
}

// HasGlobals reports whether this target also dumps cluster-wide objects.
func (d *Dumper) HasGlobals() bool { return d.opts.IncludeGlobals }

// DumpGlobals streams roles and tablespaces as plain SQL. They live outside
// any single database, so a restore into an empty cluster needs them before
// the dump itself will apply cleanly.
func (d *Dumper) DumpGlobals(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	d.mu.Lock()
	info, err := d.probe(ctx)
	d.mu.Unlock()
	if err != nil {
		return core.DumpResult{}, err
	}

	binary, err := d.resolveBinary(ctx, "pg_dumpall", info.VersionNum/10000)
	if err != nil {
		return core.DumpResult{}, err
	}

	// pg_dumpall connects to a maintenance database, not to the target one.
	previous := d.conn
	d.conn = d.conn.withDatabase("postgres")
	defer func() { d.conn = previous }()

	stderr, err := d.run(ctx, binary, []string{"--globals-only", "--no-password"}, w)
	if err != nil {
		return core.DumpResult{}, err
	}

	return core.DumpResult{
		Consistency:   core.ConsistencyBestEffort,
		DumperVersion: binary.String(),
		StderrTail:    stderr,
	}, nil
}

// run executes a client binary, streaming its stdout to w and keeping the tail
// of its stderr for the failure report.
func (d *Dumper) run(ctx context.Context, binary engine.Binary, args []string, w io.Writer) (string, error) {
	// The path comes from our own resolver and the arguments are built here,
	// never from user input: the connection details travel in the environment.
	cmd := exec.CommandContext(ctx, binary.Path, args...) //nolint:gosec
	cmd.Env = engine.Env(d.conn.env())
	cmd.Stdout = w

	tail := engine.NewTail(engine.StderrTailBytes)
	cmd.Stderr = tail

	// If the context is cancelled and the client ignores the signal, do not
	// wait forever for it to finish writing.
	cmd.WaitDelay = 15 * time.Second

	err := cmd.Run()
	stderr := tail.String()
	if err == nil {
		return stderr, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit is not retried blindly: it is classified and
		// reported with its stderr, because retrying a dump that failed on
		// permissions or a missing table just burns another hour (SPEC §11).
		return stderr, &engine.ExitError{
			Binary:   binary.Name,
			Code:     exitErr.ExitCode(),
			Stderr:   stderr,
			Original: err,
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stderr, fmt.Errorf("%s was cancelled: %w", binary.Name, ctxErr)
	}
	return stderr, fmt.Errorf("running %s: %w", binary.Name, err)
}

var (
	_ core.Dumper        = (*Dumper)(nil)
	_ core.GlobalsDumper = (*Dumper)(nil)
)
