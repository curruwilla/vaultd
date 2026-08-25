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
