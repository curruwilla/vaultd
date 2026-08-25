// Package app wires the configuration to the adapters: which store a
// destination means, which dumper an engine means, and what a target's
// settings translate into for the pipeline.
//
// The wiring is explicit constructor injection. What the config picks at
// runtime — an engine name, a provider name — is resolved through the small
// registries below rather than a DI container, so the whole graph is readable
// top to bottom.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/backup"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mongodb"
	"github.com/curruwilla/vaultd/internal/engine/mysql"
	"github.com/curruwilla/vaultd/internal/engine/postgres"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

// App holds the configuration and the adapters built from it. Stores are
// memoized: several targets usually share one destination, and each store
// carries an HTTP client worth reusing.
type App struct {
	cfg *config.Config
	log *slog.Logger

	mu     sync.Mutex
	stores map[string]core.Store
}

// New returns an App over an already validated config.
func New(cfg *config.Config, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{cfg: cfg, log: log, stores: map[string]core.Store{}}
}

// Config returns the configuration this App was built from.
func (a *App) Config() *config.Config { return a.cfg }

// Store returns the object store of a destination, building it once.
func (a *App) Store(ctx context.Context, name string) (core.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if store, ok := a.stores[name]; ok {
		return store, nil
	}

	dest, ok := a.cfg.Destination(name)
	if !ok {
		return nil, fmt.Errorf("destination %q is not declared", name)
	}

	store, err := s3.New(ctx, s3.Config{
		Provider:        s3.Provider(dest.Provider),
		Bucket:          dest.Bucket,
		Endpoint:        dest.Endpoint,
		Region:          dest.Region,
		AccessKeyID:     dest.AccessKeyID.Reveal(),
		SecretAccessKey: dest.SecretAccessKey.Reveal(),
		ForcePathStyle:  dest.ForcePathStyle,
		StorageClass:    storageClassFor(dest),
	})
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", name, err)
	}

	a.stores[name] = store
	return store, nil
}

// SetStore installs a store for a destination, bypassing construction. Tests
// use it to run the real orchestration against an in-memory bucket.
func (a *App) SetStore(name string, store core.Store) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stores[name] = store
}

// Dumper builds the engine adapter of a target.
//
// This switch is the engine registry: adding an engine means adding a case and
// its package, and nothing else in the codebase learns a new name.
func (a *App) Dumper(target *config.Target) (core.Dumper, error) {
	switch target.Engine {
	case core.EnginePostgres:
		return postgres.New(postgres.Options{
			DSN:              target.Conn().Reveal(),
			ExcludeTableData: target.Options.ExcludeTableData,
			IncludeGlobals:   target.Options.IncludeGlobals != nil && *target.Options.IncludeGlobals,
			RowEstimate:      postgres.RowEstimate(target.RowEstimate),
		})

	case core.EngineMySQL, core.EngineMariaDB:
		return mysql.New(mysql.Options{
			DSN:         target.Conn().Reveal(),
			Flavor:      flavorOf(target.Engine),
			OnNonInnoDB: mysql.NonInnoDB(target.Options.OnNonInnoDB),
			RowEstimate: mysql.RowEstimate(target.RowEstimate),
		})

	case core.EngineMongoDB:
		return mongodb.New(mongodb.Options{
			URI:         target.Conn().Reveal(),
			Oplog:       target.Options.Oplog != nil && *target.Options.Oplog,
			RowEstimate: mongodb.RowEstimate(target.RowEstimate),
		})

	default:
		return nil, fmt.Errorf("target %q uses unknown engine %q", target.Name, target.Engine)
	}
}

// Layout returns the object key layout of a target inside its destination.
func (a *App) Layout(target *config.Target) (manifest.Layout, error) {
	dest, ok := a.cfg.Destination(target.Destination)
	if !ok {
		return manifest.Layout{}, fmt.Errorf("target %q writes to destination %q, which is not declared", target.Name, target.Destination)
	}
	return manifest.Layout{Prefix: dest.Prefix, Target: target.Name}, nil
}

// BackupSpec translates a target into everything the runner needs.
func (a *App) BackupSpec(target *config.Target, tier string) (backup.Spec, error) {
	layout, err := a.Layout(target)
	if err != nil {
		return backup.Spec{}, err
	}

	// Streaming is the only mode M1 implements. Silently ignoring a
	// spool setting would make the config lie about where the backup lives
	// while it runs.
	if target.Spool == config.SpoolDisk {
		return backup.Spec{}, fmt.Errorf("target %q sets spool: disk, which is not implemented yet; backups stream straight to the destination", target.Name)
	}

	spec := backup.Spec{
		Target:   target.Name,
		Engine:   target.Engine,
		Kind:     manifest.KindFull,
		Tier:     tier,
		Layout:   layout,
		Pipeline: pipelineSpec(target),
	}
	if target.Timeout != nil {
		spec.Timeout = target.Timeout.Duration()
	}
	return spec, nil
}

// Runner assembles the backup runner of a target.
func (a *App) Runner(ctx context.Context, target *config.Target) (*backup.Runner, error) {
	store, err := a.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	dumper, err := a.Dumper(target)
	if err != nil {
		return nil, err
	}

	return &backup.Runner{
		Store:  store,
		Dumper: dumper,
		Now:    func() time.Time { return time.Now().UTC() },
		Log:    a.log,
	}, nil
}

// pipelineSpec maps a target's compression and encryption onto the pipeline.
func pipelineSpec(target *config.Target) pipeline.Spec {
	var spec pipeline.Spec

	if c := target.Compression; c != nil {
		spec.Compression = pipeline.Compression{
			Algo:  pipeline.Algo(c.Algo),
			Level: c.Level,
			Long:  c.Long,
		}
	}
	if e := target.Encryption; e != nil {
		spec.Encryption = pipeline.Encryption{
			Mode:       pipeline.Mode(e.Mode),
			Recipients: e.Recipients,
			Passphrase: e.Passphrase.Reveal(),
		}
	}
	return spec
}

// flavorOf maps the configured engine onto the fork the client must match.
func flavorOf(engine core.Engine) mysql.Flavor {
	if engine == core.EngineMariaDB {
		return mysql.FlavorMariaDB
	}
	return mysql.FlavorMySQL
}

// storageClassFor drops a storage class the provider does not implement, so a
// config shared between AWS and R2 does not fail on R2 (SPEC §6).
func storageClassFor(dest *config.Destination) string {
	if dest.Provider == config.ProviderR2 {
		return ""
	}
	return dest.StorageClass
}
