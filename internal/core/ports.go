// Package core defines the ports (interfaces) and value types the vaultd
// pipeline is written against. Adapters implementing them live in
// internal/engine (Dumper/Restorer), internal/storage (Store) and
// internal/notify (Notifier).
package core

import (
	"context"
	"errors"
	"io"
	"iter"
	"time"
)

// Engine identifies a database engine vaultd can dump.
type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
	EngineMariaDB  Engine = "mariadb"
	EngineMongoDB  Engine = "mongodb"
)

// Engines lists every supported engine, in config-documentation order.
var Engines = []Engine{EnginePostgres, EngineMySQL, EngineMariaDB, EngineMongoDB}

func (e Engine) Valid() bool {
	for _, known := range Engines {
		if e == known {
			return true
		}
	}
	return false
}

// DSN is a database connection string. It is a distinct type so that call
// sites cannot pass an arbitrary string where a connection string is expected.
type DSN string

// Consistency records how consistent a dump is, as achieved at dump time.
type Consistency string

const (
	// ConsistencySerializableSnapshot is pg_dump --serializable-deferrable.
	ConsistencySerializableSnapshot Consistency = "serializable_snapshot"
	// ConsistencySingleTransaction is mysqldump --single-transaction (InnoDB only).
	ConsistencySingleTransaction Consistency = "single_transaction"
	// ConsistencyPointInTime is mongodump --oplog against a replica set.
	ConsistencyPointInTime Consistency = "point_in_time"
	// ConsistencyLockedTables means every table was read-locked for the
	// duration of the dump: consistent across storage engines, at the cost of
	// blocking writers.
	ConsistencyLockedTables Consistency = "locked_tables"
	// ConsistencyBestEffort means the engine could not offer a consistent snapshot.
	ConsistencyBestEffort Consistency = "best_effort"
)

// TableInfo describes one table (or collection) as seen at probe or dump time.
type TableInfo struct {
	Name      string `json:"name"`
	Rows      int64  `json:"rows"`
	RowsExact bool   `json:"rows_exact"`
	// StorageEngine is the per-table engine (InnoDB, MyISAM…); MySQL/MariaDB only.
	StorageEngine string `json:"storage_engine,omitempty"`
}

// ServerInfo is what a Probe learns about the server before dumping it.
type ServerInfo struct {
	Engine      Engine
	Version     string
	VersionNum  int
	Consistency Consistency
	Tables      []TableInfo
	Warnings    []string
}

// DumpResult is what a Dumper reports once its stream is fully written.
type DumpResult struct {
	Bytes         int64
	SHA256        string
	Consistency   Consistency
	Tables        []TableInfo
	BinlogPos     string
	OplogEnd      string
	DumperVersion string
	StderrTail    string
}

// Dumper produces a restorable logical dump on an io.Writer.
type Dumper interface {
	Probe(ctx context.Context) (ServerInfo, error)
	Dump(ctx context.Context, w io.Writer) (DumpResult, error)
}

// GlobalsDumper is implemented by engines that carry objects living outside
// any single database — PostgreSQL roles and tablespaces. The backup runner
// asks for it with a type assertion, so engines without the concept simply do
// not implement it.
type GlobalsDumper interface {
	HasGlobals() bool
	DumpGlobals(ctx context.Context, w io.Writer) (DumpResult, error)
}

// Restorer writes a dump stream back into a database. A Restorer is built
// around the destination it will write to — the same shape as a Dumper — so
// that it can also answer whether that destination already holds data, which
// is what stands between a restore and an overwritten production database.
//
// v1 only ever points a Restorer at an ephemeral or explicitly confirmed
// database (SPEC §2).
type Restorer interface {
	// IsEmpty reports whether the destination holds no user data yet.
	IsEmpty(ctx context.Context) (bool, error)
	Restore(ctx context.Context, r io.Reader) error
}

// ObjectInfo is the metadata of a stored object.
type ObjectInfo struct {
	Key          string
	Bytes        int64
	ETag         string
	SHA256       string
	LastModified time.Time
}

// PutOptions tunes a single upload.
type PutOptions struct {
	ContentType string
	// PartSize is the initial multipart part size in bytes; the uploader
	// doubles it as the part count grows (see SPEC §4).
	PartSize    int64
	Concurrency int
	Metadata    map[string]string
}

// Store is the object storage port (S3, R2, MinIO, GCS interop).
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, opt PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
	Delete(ctx context.Context, keys []string) error
	// PutIfAbsent writes key only if it does not exist yet (If-None-Match),
	// reporting whether this caller created it. It is the target lock.
	PutIfAbsent(ctx context.Context, key string, b []byte) (bool, error)
	// PutIfMatch overwrites key only if it still carries etag, reporting
	// whether the write went through. It is how the append-only index stays
	// append-only: object stores have no append, so a concurrent writer must
	// lose the race rather than silently truncate the other's entries.
	PutIfMatch(ctx context.Context, key string, b []byte, etag string) (ObjectInfo, bool, error)
}

// ErrNotFound is returned by a Store when an object does not exist. Callers
// distinguish "this backup is gone" from "the bucket is unreachable" with
// errors.Is, never by matching provider-specific error text.
var ErrNotFound = errors.New("object not found")

// Inspector reads a database back once something has been restored into it.
//
// The names it takes are the ones the manifest records, and those are
// engine-specific — `public.users` for PostgreSQL, `users` for MySQL,
// `app.users` for MongoDB. Each adapter maps them onto whatever it actually
// created, so the caller compares manifests with restores without knowing
// which engine it is holding.
type Inspector interface {
	// Tables lists what the restored database holds, in its own naming.
	Tables(ctx context.Context) ([]string, error)
	// CountRows counts one table exactly. A freshly restored database has no
	// statistics worth reading, so an estimate here would measure nothing.
	CountRows(ctx context.Context, table string) (int64, error)
	// Scalar runs a query and returns its single value.
	Scalar(ctx context.Context, query string) (any, error)
}

// Sandbox is a database created for one restore verification and dropped
// afterwards (SPEC §8, decision D3). It is what L2 restores into, and what the
// assertions then read.
type Sandbox interface {
	Restorer
	Inspector
	// Name is the database that was created. It always carries the verify
	// target's configured prefix.
	Name() string
	// Drop removes it, whatever it ended up holding. It is idempotent: the
	// verification defers it, and `verify --gc` may have collected it first.
	Drop(ctx context.Context) error
}

// SandboxSpec is the sandbox a verification asks for.
type SandboxSpec struct {
	// Name is the database to create.
	Name string
	// Tables are the manifest's table names. An engine that has to remap
	// namespaces while restoring — MongoDB, whose archive carries the database
	// it came from — reads the source out of them; the SQL engines ignore
	// them.
	Tables []TableInfo
}

// Provisioner hands out sandboxes on a verify target and takes them back. It
// is the seam the roadmap's Docker and Kubernetes runners implement without
// anything above them changing (SPEC §8).
type Provisioner interface {
	// Probe reports what the staging server is, which is what decides whether
	// it can restore this backup at all.
	Probe(ctx context.Context) (ServerInfo, error)
	// Create makes an empty database and returns it. The name must carry the
	// configured prefix; a Provisioner refuses anything else.
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)
	// List names the sandbox databases that exist on the server right now,
	// which is what `verify --gc` collects after a crashed run.
	List(ctx context.Context) ([]string, error)
	// Drop removes one by name, refusing a name outside the prefix.
	Drop(ctx context.Context, name string) error
}

// ErrSandboxUnsupported is returned by a Provisioner that cannot give a
// particular backup an ephemeral database of its own. Verification reports it
// as skipped rather than failed: the backup is not what is wrong.
var ErrSandboxUnsupported = errors.New("this backup cannot be restored into an ephemeral database")

// ErrQueryUnsupported is returned by an Inspector for an engine with no query
// language to run a `query` assertion against.
var ErrQueryUnsupported = errors.New("this engine has no queries to assert on")

// Event names something worth telling an operator about, in the vocabulary a
// notifier subscribes to (SPEC §12). It lives here rather than in the config
// package because the config declares which events a notifier wants, the
// runner emits them and the notify adapters render them: three packages that
// must agree on one spelling.
type Event string

const (
	EventBackupStarted    Event = "backup.started"
	EventBackupSucceeded  Event = "backup.succeeded"
	EventBackupFailed     Event = "backup.failed"
	EventVerifySucceeded  Event = "verify.succeeded"
	EventVerifyFailed     Event = "verify.failed"
	EventRetentionPruned  Event = "retention.pruned"
	EventRetentionBlocked Event = "retention.blocked"
	EventScheduleMissed   Event = "schedule.missed"
	EventStorageError     Event = "storage.error"
)

// Events lists every event a notifier may subscribe to.
var Events = []Event{
	EventBackupStarted, EventBackupSucceeded, EventBackupFailed,
	EventVerifySucceeded, EventVerifyFailed,
	EventRetentionPruned, EventRetentionBlocked,
	EventScheduleMissed, EventStorageError,
}

func (e Event) Valid() bool {
	for _, known := range Events {
		if e == known {
			return true
		}
	}
	return false
}

// Severity ranks a notification for whoever is on call. It is derived from the
// event, never configured: an operator who can downgrade backup.failed to
// "info" has built themselves a silent outage.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// SeverityOf returns the severity an event always carries.
func SeverityOf(event Event) Severity {
	switch event {
	case EventBackupFailed, EventVerifyFailed, EventStorageError:
		return SeverityCritical
	case EventRetentionBlocked, EventScheduleMissed:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Failure is the error carried by a notification. StderrTail is the last of
// what the database client printed (SPEC §11): it is usually the only place
// the real reason for a failed dump is written down.
type Failure struct {
	Phase      string `json:"phase,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// Notification is one delivered event (SPEC §12). Everything in it is already
// redacted: it travels to a third-party endpoint, so a DSN or a token that
// reaches this struct has left the building.
type Notification struct {
	Event      Event          `json:"event"`
	At         time.Time      `json:"at"`
	Target     string         `json:"target,omitempty"`
	BackupID   string         `json:"backup_id,omitempty"`
	Severity   Severity       `json:"severity"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Error      *Failure       `json:"error,omitempty"`
	Summary    string         `json:"summary"`
	Details    map[string]any `json:"details,omitempty"`
}

// Notifier delivers a notification to one endpoint.
//
// A Notifier reports its delivery failures so they can be logged, but no
// caller may fail a backup over one: a webhook that is down does not make a
// stored backup any less stored (SPEC §12).
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}
