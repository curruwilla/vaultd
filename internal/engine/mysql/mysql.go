// Package mysql dumps MySQL and MariaDB databases by driving mysqldump or
// mariadb-dump, after reading the server's catalog to decide which flags that
// particular server will accept.
package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
)

// RowEstimate selects how row counts are gathered for the manifest. The
// default is `estimate` (decision D7).
type RowEstimate string

const (
	RowsExact    RowEstimate = "exact"
	RowsEstimate RowEstimate = "estimate"
	RowsOff      RowEstimate = "off"
)

// Options configures one MySQL or MariaDB target.
type Options struct {
	DSN string
	// Flavor is what the config declared. A server that turns out to be the
	// other fork is an error, not something to paper over: the flags differ.
	Flavor       Flavor
	OnNonInnoDB  NonInnoDB
	RowEstimate  RowEstimate
	BinDir       string
	ProbeTimeout time.Duration
}

// Dumper implements core.Dumper for MySQL and MariaDB.
type Dumper struct {
	opts Options
	conn connInfo

	mu     sync.Mutex
	probed *core.ServerInfo
	caps   capabilities
	dump   engine.Binary
}

// New parses the connection string and returns a Dumper. It performs no
// network call.
func New(opts Options) (*Dumper, error) {
	conn, err := parseDSN(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("%s target: %w", opts.Flavor, err)
	}
	if opts.Flavor == "" {
		opts.Flavor = FlavorMySQL
	}
	if opts.OnNonInnoDB == "" {
		opts.OnNonInnoDB = NonInnoDBWarn
	}
	if opts.RowEstimate == "" {
		opts.RowEstimate = RowsEstimate
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Dumper{opts: opts, conn: conn}, nil
}

// Probe reads the server version, the settings that decide the dump flags, the
// table list, and the storage engines in use.
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

	caps, tables, err := d.inspect(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}

	if caps.Version.Flavor != d.opts.Flavor {
		return core.ServerInfo{}, fmt.Errorf(
			"target is declared as %s but the server reports %s %s; set engine: %s",
			d.opts.Flavor, caps.Version.Flavor, caps.Version, caps.Version.Flavor)
	}

	info := core.ServerInfo{
		Engine:     engineOf(caps.Version.Flavor),
		Version:    caps.Version.String(),
		VersionNum: caps.Version.Num(),
		Tables:     tables,
	}

	if len(caps.NonInnoDB) > 0 {
		if err := d.handleNonInnoDB(&info, caps.NonInnoDB); err != nil {
			return core.ServerInfo{}, err
		}
	}
	switch {
	case !caps.LogBin:
		info.Warnings = append(info.Warnings,
			"binary logging is off, so the dump records no replication position; point-in-time recovery from it will not be possible")
	case !caps.CanFlush || !caps.CanReadPosition:
		info.Warnings = append(info.Warnings, fmt.Sprintf(
			"the backup user has no %s, so the dump records no replication position; grant %s to make point-in-time recovery possible",
			missingPrivileges(caps), missingPrivileges(caps)))
	}

	// Locking every table is FLUSH TABLES WITH READ LOCK: without the
	// privilege it would fail partway through the dump, so refuse now with a
	// message that says what to change.
	if d.opts.OnNonInnoDB == NonInnoDBLock && !caps.CanFlush {
		return core.ServerInfo{}, errors.New(
			"on_non_innodb: lock needs the RELOAD privilege to lock every table, which this user does not have; grant RELOAD or use warn")
	}

	binary, warning, err := d.resolveClient(ctx, caps.Version)
	if err != nil {
		return core.ServerInfo{}, err
	}
	if warning != "" {
		info.Warnings = append(info.Warnings, warning)
	}
	d.dump = binary

	_, info.Consistency = dumpArgs(caps, d.conn.Database, d.opts.OnNonInnoDB)

	d.caps = caps
	d.probed = &info
	return info, nil
}

// handleNonInnoDB applies the configured policy for non-transactional tables.
func (d *Dumper) handleNonInnoDB(info *core.ServerInfo, tables []string) error {
	message := fmt.Sprintf("%d table(s) use a non-transactional storage engine (%s), which --single-transaction cannot dump consistently",
		len(tables), strings.Join(tables, ", "))

	switch d.opts.OnNonInnoDB {
	case NonInnoDBFail:
		return fmt.Errorf("%s; set on_non_innodb: lock to lock the server instead, or warn to accept it", message)
	case NonInnoDBLock:
		info.Warnings = append(info.Warnings, message+"; locking every table for the duration of the dump")
	default:
		info.Warnings = append(info.Warnings, message+"; those tables are dumped without a snapshot")
	}
	return nil
}

// Dump streams a SQL dump to w.
func (d *Dumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	d.mu.Lock()
	info, err := d.probe(ctx)
	caps, binary := d.caps, d.dump
	d.mu.Unlock()
	if err != nil {
		return core.DumpResult{}, err
	}

	dumpFlags, consistency := dumpArgs(caps, d.conn.Database, d.opts.OnNonInnoDB)
	args := append(d.conn.args(), dumpFlags...)

	// The path comes from our own resolver and every argument is built here;
	// the password travels in the environment.
	cmd := exec.CommandContext(ctx, binary.Path, args...) //nolint:gosec
	cmd.Env = engine.Env(d.conn.env())
	cmd.Stdout = w
	cmd.WaitDelay = 15 * time.Second

	tail := engine.NewTail(engine.StderrTailBytes)
	cmd.Stderr = tail

	err = cmd.Run()
	stderr := tail.String()

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

	return core.DumpResult{
		Consistency:   consistency,
		Tables:        info.Tables,
		DumperVersion: binary.String(),
		StderrTail:    stderr,
	}, nil
}

// missingPrivileges names what is absent, so the message says what to grant.
func missingPrivileges(caps capabilities) string {
	var missing []string
	if !caps.CanFlush {
		missing = append(missing, "RELOAD privilege")
	}
	if !caps.CanReadPosition {
		missing = append(missing, "REPLICATION CLIENT privilege")
	}
	return strings.Join(missing, " and no ")
}

func engineOf(flavor Flavor) core.Engine {
	if flavor == FlavorMariaDB {
		return core.EngineMariaDB
	}
	return core.EngineMySQL
}

var _ core.Dumper = (*Dumper)(nil)
