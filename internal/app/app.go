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
	"net/http"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/backup"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine/mongodb"
	"github.com/curruwilla/vaultd/internal/engine/mysql"
	"github.com/curruwilla/vaultd/internal/engine/postgres"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/prune"
	"github.com/curruwilla/vaultd/internal/retention"
	"github.com/curruwilla/vaultd/internal/storage/s3"
	"github.com/curruwilla/vaultd/internal/verify"
)

// App holds the configuration and the adapters built from it. Stores are
// memoized: several targets usually share one destination, and each store
// carries an HTTP client worth reusing.
type App struct {
	cfg *config.Config
	log *slog.Logger

	mu        sync.Mutex
	stores    map[string]core.Store
	dumpers   map[string]core.Dumper
	notifiers map[string]core.Notifier
	fanouts   map[string]*notify.Fanout
	client    *http.Client
}

// New returns an App over an already validated config.
func New(cfg *config.Config, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{
		cfg:       cfg,
		log:       log,
		stores:    map[string]core.Store{},
		dumpers:   map[string]core.Dumper{},
		notifiers: map[string]core.Notifier{},
		fanouts:   map[string]*notify.Fanout{},
	}
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

// SetDumper installs a dumper for a target, bypassing the engine registry.
// Tests use it to drive the real orchestration — lock, schedule, manifest,
// index — without a database.
func (a *App) SetDumper(target string, dumper core.Dumper) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dumpers[target] = dumper
}

// Dumper builds the engine adapter of a target.
//
// This switch is the engine registry: adding an engine means adding a case and
// its package, and nothing else in the codebase learns a new name.
func (a *App) Dumper(target *config.Target) (core.Dumper, error) {
	a.mu.Lock()
	installed, ok := a.dumpers[target.Name]
	a.mu.Unlock()
	if ok {
		return installed, nil
	}

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

// Restorer builds the client that writes a backup of the given engine back
// into dsn. The destination comes from the command line, not from the config:
// v1 restores into an explicit target, never implicitly into the database the
// backup came from (SPEC §2).
func (a *App) Restorer(engine core.Engine, dsn string, clean bool) (core.Restorer, error) {
	switch engine {
	case core.EnginePostgres:
		return postgres.NewRestorer(postgres.RestoreOptions{DSN: dsn, Clean: clean})

	case core.EngineMySQL, core.EngineMariaDB:
		return mysql.NewRestorer(mysql.RestoreOptions{DSN: dsn, Flavor: flavorOf(engine)})

	case core.EngineMongoDB:
		return mongodb.NewRestorer(mongodb.RestoreOptions{URI: dsn, Drop: clean})

	default:
		return nil, fmt.Errorf("this backup was taken with engine %q, which this build cannot restore", engine)
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

// Index returns the listing cache of a target.
func (a *App) Index(ctx context.Context, target *config.Target) (*index.Store, error) {
	store, err := a.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	layout, err := a.Layout(target)
	if err != nil {
		return nil, err
	}
	return index.New(store, layout), nil
}

// Retention translates a target's declared policy into the one prune applies.
//
// An absent block means every backup is kept forever, which is what an empty
// policy does; the config warns about it at validate time.
func (a *App) Retention(target *config.Target) retention.Policy {
	declared := target.Retention
	if declared == nil {
		return retention.Policy{MinKeep: 1}
	}

	policy := retention.Policy{MinKeep: declared.MinKeep}
	if declared.Hourly != nil {
		policy.Hourly = retention.Rule{Keep: declared.Hourly.Keep}
	}
	if declared.Daily != nil {
		policy.Daily = retention.Rule{Keep: declared.Daily.Keep}
	}
	if declared.Weekly != nil {
		policy.Weekly = retention.WeekRule{Keep: declared.Weekly.Keep, On: time.Weekday(declared.Weekly.On)}
	}
	if declared.Monthly != nil {
		policy.Monthly = retention.MonthRule{Keep: declared.Monthly.Keep, On: declared.Monthly.On}
	}
	if declared.Yearly != nil {
		policy.Yearly = retention.Rule{Keep: declared.Yearly.Keep}
	}
	return policy
}

// Verifier assembles the verifier of a target. The age identities come from
// the invocation rather than the config: the private key is deliberately not
// something vaultd stores (SPEC §15).
//
// The level is part of the request because restore verification needs a
// staging server to restore into, and building one for a check that will never
// touch it would fail a `--level integrity` run over a verify target that
// happens to be misconfigured.
func (a *App) Verifier(
	ctx context.Context,
	target *config.Target,
	identities []age.Identity,
	level verify.Level,
) (*verify.Verifier, error) {
	store, err := a.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	idx, err := a.Index(ctx, target)
	if err != nil {
		return nil, err
	}

	notifier, err := a.Notifier(target)
	if err != nil {
		return nil, err
	}

	verifier := &verify.Verifier{
		Store:      store,
		Index:      idx,
		Identities: identities,
		Notify:     notifier,
		Now:        func() time.Time { return time.Now().UTC() },
		Log:        a.log,
	}
	if target.Encryption != nil {
		verifier.Passphrase = target.Encryption.Passphrase.Reveal()
	}

	if level == verify.LevelRestore {
		if err := a.attachSandbox(verifier, target); err != nil {
			return nil, err
		}
	}
	return verifier, nil
}

// attachSandbox gives a verifier the staging server it restores into, and the
// assertions it runs once the backup is there (decision D3).
func (a *App) attachSandbox(verifier *verify.Verifier, target *config.Target) error {
	if target.Verify == nil || target.Verify.Into == "" {
		return fmt.Errorf(
			"target %q does not say where to restore for verification; set verify.into to a declared verify target",
			target.Name)
	}

	into, ok := a.cfg.VerifyTarget(target.Verify.Into)
	if !ok {
		return fmt.Errorf("target %q restores into verify target %q, which is not declared", target.Name, target.Verify.Into)
	}
	if into.Engine != target.Engine {
		return fmt.Errorf("target %q (%s) restores into verify target %q (%s); engines must match",
			target.Name, target.Engine, into.Name, into.Engine)
	}

	provisioner, err := a.Provisioner(into)
	if err != nil {
		return err
	}

	verifier.Sandbox = provisioner
	verifier.DatabasePrefix = into.DatabasePrefix
	verifier.Assertions = assertionsOf(target.Verify.Assertions)
	if target.Timeout != nil {
		verifier.RestoreTimeout = target.Timeout.Duration()
	}
	return nil
}

// Provisioner builds the adapter that creates and drops ephemeral databases on
// a verify target. It is the engine registry again, for the other direction.
func (a *App) Provisioner(into *config.VerifyTarget) (core.Provisioner, error) {
	switch into.Engine {
	case core.EnginePostgres:
		return postgres.NewProvisioner(postgres.ProvisionOptions{
			DSN:    into.Conn().Reveal(),
			Prefix: into.DatabasePrefix,
		})

	case core.EngineMySQL, core.EngineMariaDB:
		return mysql.NewProvisioner(mysql.ProvisionOptions{
			DSN:    into.Conn().Reveal(),
			Flavor: flavorOf(into.Engine),
			Prefix: into.DatabasePrefix,
		})

	case core.EngineMongoDB:
		return mongodb.NewProvisioner(mongodb.ProvisionOptions{
			URI:    into.Conn().Reveal(),
			Prefix: into.DatabasePrefix,
		})

	default:
		return nil, fmt.Errorf("verify target %q uses unknown engine %q", into.Name, into.Engine)
	}
}

// assertionsOf translates the configured assertions into what the verifier
// runs. The duration of a max_age assertion is the only field that changes
// shape on the way.
func assertionsOf(declared []config.Assertion) []verify.Assertion {
	if len(declared) == 0 {
		return nil
	}

	out := make([]verify.Assertion, 0, len(declared))
	for _, a := range declared {
		assertion := verify.Assertion{
			Type:      verify.AssertionType(a.Type),
			Tables:    a.Tables,
			Tolerance: a.Tolerance,
			SQL:       a.SQL,
			Expect:    a.Expect,
		}
		if a.Value != nil {
			assertion.MaxAge = a.Value.Duration()
		}
		out = append(out, assertion)
	}
	return out
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
	layout, err := a.Layout(target)
	if err != nil {
		return nil, err
	}
	notifier, err := a.Notifier(target)
	if err != nil {
		return nil, err
	}

	return &backup.Runner{
		Store:  store,
		Dumper: dumper,
		Index:  index.New(store, layout),
		Notify: notifier,
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

// Pruner assembles the retention runner of a target: the policy it applies,
// the bucket it deletes from, the index it puts back in step, and the
// notifiers that hear about it.
func (a *App) Pruner(ctx context.Context, target *config.Target) (*prune.Runner, error) {
	store, err := a.Store(ctx, target.Destination)
	if err != nil {
		return nil, err
	}
	idx, err := a.Index(ctx, target)
	if err != nil {
		return nil, err
	}
	notifier, err := a.Notifier(target)
	if err != nil {
		return nil, err
	}

	return &prune.Runner{
		Target: target.Name,
		Store:  store,
		Index:  idx,
		Policy: a.Retention(target),
		Notify: notifier,
		Now:    func() time.Time { return time.Now().UTC() },
		Log:    a.log,
	}, nil
}
