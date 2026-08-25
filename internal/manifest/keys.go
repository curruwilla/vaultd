package manifest

import (
	"path"
	"strings"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// TimeFormat is the timestamp used inside object keys: sortable, unambiguous
// and safe in a key. Always UTC.
const TimeFormat = "20060102T150405Z"

const (
	manifestSuffix = ".manifest.json"
	globalsSuffix  = "-globals"
	indexDir       = "_index"
	lockDir        = "_locks"
)

// Layout builds the object keys of one target inside one destination
// (SPEC §6.1):
//
//	<prefix>/<target>/2026/08/24/<target>-20260824T031500Z-full.pgdump.zst.age
//	<prefix>/<target>/2026/08/24/<target>-20260824T031500Z-full.manifest.json
//	<prefix>/_index/<target>.jsonl
//	<prefix>/_locks/<target>.lock
type Layout struct {
	// Prefix is the destination prefix; it may be empty.
	Prefix string
	Target string
}

// TargetPrefix is the key prefix holding every backup of this target. It is
// what a listing walks.
func (l Layout) TargetPrefix() string { return l.join(l.Target) + "/" }

// Base returns the shared stem of every object of one backup run.
func (l Layout) Base(at time.Time) string {
	at = at.UTC()
	return l.join(l.Target, at.Format("2006"), at.Format("01"), at.Format("02"), l.Target+"-"+at.Format(TimeFormat))
}

// Data is the key of the backup stream itself.
func (l Layout) Data(at time.Time, kind Kind, ext string) string {
	return l.Base(at) + "-" + string(kind) + ext
}

// Manifest is the key of the metadata document of a backup.
func (l Layout) Manifest(at time.Time, kind Kind) string {
	return l.Base(at) + "-" + string(kind) + manifestSuffix
}

// Globals is the key of the PostgreSQL cluster-wide objects (roles,
// tablespaces) dumped alongside a database.
func (l Layout) Globals(at time.Time, ext string) string {
	return l.Base(at) + globalsSuffix + ext
}

// Index is the append-only listing cache of this target.
func (l Layout) Index() string { return l.join(indexDir, l.Target+".jsonl") }

// Lock is the key whose creation grants exclusive execution of this target.
func (l Layout) Lock() string { return l.join(lockDir, l.Target+".lock") }

func (l Layout) join(parts ...string) string {
	if l.Prefix == "" {
		return path.Join(parts...)
	}
	return path.Join(append([]string{strings.Trim(l.Prefix, "/")}, parts...)...)
}

// IsManifestKey reports whether a key holds a manifest document.
func IsManifestKey(key string) bool { return strings.HasSuffix(key, manifestSuffix) }

// DataExtension composes the extension of a data object from the pipeline that
// produced it, so the name says how to read it back: .pgdump.zst.age is a
// custom-format dump, zstd-compressed, then age-encrypted.
func DataExtension(engine core.Engine, compression, encryption string) string {
	return dumpExtension(engine) + pipelineSuffixes(compression, encryption)
}

// GlobalsExtension is the same, for the plain SQL of cluster-wide objects.
func GlobalsExtension(compression, encryption string) string {
	return ".sql" + pipelineSuffixes(compression, encryption)
}

func pipelineSuffixes(compression, encryption string) string {
	var ext string

	switch {
	case strings.HasPrefix(compression, "zstd"):
		ext += ".zst"
	case strings.HasPrefix(compression, "gzip"):
		ext += ".gz"
	}

	if encryption != "" && encryption != "none" {
		ext += ".age"
	}
	return ext
}

func dumpExtension(engine core.Engine) string {
	switch engine {
	case core.EnginePostgres:
		return ".pgdump" // pg_dump -Fc, the custom format
	case core.EngineMySQL, core.EngineMariaDB:
		return ".sql"
	case core.EngineMongoDB:
		return ".archive" // mongodump --archive
	default:
		return ".dump"
	}
}
