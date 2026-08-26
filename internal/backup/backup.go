// Package backup runs one backup end to end: probe the server, stream the dump
// through the pipeline into object storage, and write the manifest that makes
// the result findable and verifiable.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/curruwilla/vaultd/internal/buildinfo"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
	"github.com/curruwilla/vaultd/internal/pipeline"
)

// Phase names the stage a backup failed in. It travels into the manifest of a
// failed run and into the webhook payload (SPEC §12), where "which phase" is
// the first question an operator asks.
type Phase string

const (
	PhaseProbe    Phase = "probe"
	PhaseDump     Phase = "dump"
	PhaseUpload   Phase = "upload"
	PhaseManifest Phase = "manifest"
)

// Error is a failure attributed to a phase.
type Error struct {
	Phase  Phase
	Target string
	Err    error
	// Stderr is the tail of the dumper's output, when there was one.
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("backup of %s failed during %s: %s", e.Target, e.Phase, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Spec is one backup to run.
type Spec struct {
	Target   string
	Engine   core.Engine
	Kind     manifest.Kind
	Tier     string
	Layout   manifest.Layout
	Pipeline pipeline.Spec
	// Timeout bounds the whole run. Zero means no limit beyond the context.
	Timeout     time.Duration
	PartSize    int64
	Concurrency int
}

// Runner executes a Spec against one dumper and one store.
type Runner struct {
	Store  core.Store
	Dumper core.Dumper
	// Index is the listing cache. It is optional: a backup is complete once
	// its manifest is stored, and the index can always be rebuilt from those.
	Index *index.Store
	// Notify receives this run's lifecycle events (SPEC §12). It is optional,
	// and a delivery failure is logged rather than returned: a webhook that is
	// down does not make a stored backup any less stored.
	Notify core.Notifier
	// Now is the clock; tests replace it so keys are deterministic.
	Now func() time.Time
	Log *slog.Logger
}

// Plan is what a dry run reports: everything that would happen, and nothing
// that would be written.
type Plan struct {
	Target        string
	Engine        core.Engine
	ServerVersion string
	Consistency   core.Consistency
	Tables        int
	Rows          int64
	DataKey       string
	ManifestKey   string
	GlobalsKey    string
	Compression   string
	Encryption    string
	Warnings      []string
}

// Plan probes the server and works out the keys and settings a real run would
// use. It writes nothing.
func (r *Runner) Plan(ctx context.Context, spec Spec) (*Plan, error) {
	at := r.now()

	info, err := r.Dumper.Probe(ctx)
	if err != nil {
		return nil, &Error{Phase: PhaseProbe, Target: spec.Target, Err: err}
	}

	kind := spec.kind()
	plan := &Plan{
		Target:        spec.Target,
		Engine:        info.Engine,
		ServerVersion: info.Version,
		Consistency:   info.Consistency,
		Tables:        len(info.Tables),
		DataKey:       spec.Layout.Data(at, kind, spec.extension()),
		ManifestKey:   spec.Layout.Manifest(at, kind),
		Compression:   spec.Pipeline.Compression.String(),
		Encryption:    spec.Pipeline.Encryption.String(),
		Warnings:      info.Warnings,
	}
	for _, t := range info.Tables {
		plan.Rows += t.Rows
	}
	if _, ok := r.globals(); ok {
		plan.GlobalsKey = spec.Layout.Globals(at, globalsExtension(spec))
	}
	return plan, nil
}

// Run performs the backup and returns the manifest it stored.
func (r *Runner) Run(ctx context.Context, spec Spec) (result *manifest.Manifest, err error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	started := r.now()
	log := r.log().With("target", spec.Target, "engine", string(spec.Engine))

	notify.Emit(ctx, r.Notify, log, startedEvent(spec, started))

	// A failed run is recorded too. Retention refuses to delete anything while
	// the most recent attempt failed (SPEC §7, invariant 3), and an index that
	// only holds successes cannot tell a quiet week from a broken one.
	defer func() {
		if err != nil {
			r.recordFailure(ctx, spec, started, err)
			notify.Emit(ctx, r.Notify, log, failedEvent(spec, started, r.now(), err))
		}
	}()

	info, err := r.Dumper.Probe(ctx)
	if err != nil {
		return nil, &Error{Phase: PhaseProbe, Target: spec.Target, Err: err}
	}

	kind := spec.kind()

	// Object keys are stamped to the second (SPEC §6.1), so two runs that
	// start within the same second would land on the same key and the second
	// would silently overwrite the first. Move forward until the slot is free.
	started, err = r.reserve(ctx, spec, kind, started)
	if err != nil {
		return nil, &Error{Phase: PhaseUpload, Target: spec.Target, Err: err}
	}

	dataKey := spec.Layout.Data(started, kind, spec.extension())

	log.InfoContext(ctx, "backup started",
		"key", dataKey, "server_version", info.Version, "tables", len(info.Tables))

	dump, object, err := r.stream(ctx, spec, dataKey, r.Dumper.Dump)
	if err != nil {
		return nil, err
	}

	// A dump of zero bytes is not a backup, whatever the client's exit code
	// said. Storing it would put a manifest in the bucket claiming a
	// restorable backup exists, and retention would happily count it.
	if object.Plaintext.Bytes == 0 {
		if delErr := r.Store.Delete(context.WithoutCancel(ctx), []string{dataKey}); delErr != nil {
			log.WarnContext(ctx, "could not remove the empty object", "key", dataKey, "error", delErr)
		}
		return nil, &Error{
			Phase:  PhaseDump,
			Target: spec.Target,
			Err:    errors.New("the dump produced no data"),
			Stderr: dump.StderrTail,
		}
	}

	m := &manifest.Manifest{
		Schema:        manifest.Schema,
		ID:            manifest.NewID(started),
		Target:        spec.Target,
		Engine:        info.Engine,
		ServerVersion: info.Version,
		StartedAt:     started.UTC(),
		Kind:          kind,
		Tier:          spec.Tier,
		Object: manifest.Object{
			Key:    dataKey,
			Bytes:  object.Ciphertext.Bytes,
			SHA256: object.Ciphertext.SHA256,
		},
		Plaintext: manifest.Plaintext{
			Bytes:  object.Plaintext.Bytes,
			SHA256: object.Plaintext.SHA256,
		},
		Pipeline: manifest.Pipeline{
			Compression: spec.Pipeline.Compression.String(),
			Encryption:  spec.Pipeline.Encryption.String(),
			Dumper:      dump.DumperVersion,
		},
		Consistency:   dump.Consistency,
		Tables:        tablesOf(info, dump),
		Warnings:      info.Warnings,
		VaultdVersion: buildinfo.Short(),
	}
	if dump.BinlogPos != "" {
		m.Binlog = &dump.BinlogPos
	}
	if dump.OplogEnd != "" {
		m.OplogEnd = &dump.OplogEnd
	}

	if globals, ok := r.globals(); ok {
		globalsKey := spec.Layout.Globals(started, globalsExtension(spec))

		_, globalsObject, err := r.stream(ctx, spec, globalsKey, globals.DumpGlobals)
		if err != nil {
			return nil, err
		}
		m.Globals = &manifest.Object{
			Key:    globalsKey,
			Bytes:  globalsObject.Ciphertext.Bytes,
			SHA256: globalsObject.Ciphertext.SHA256,
		}
		log.InfoContext(ctx, "globals stored", "key", globalsKey, "bytes", globalsObject.Ciphertext.Bytes)
	}

	finished := r.now()
	m.FinishedAt = finished.UTC()
	m.DurationMS = finished.Sub(started).Milliseconds()

	manifestKey := spec.Layout.Manifest(started, kind)
	if err := r.putManifest(ctx, manifestKey, m); err != nil {
		return nil, &Error{Phase: PhaseManifest, Target: spec.Target, Err: err}
	}

	// The index is a cache: a failure to update it does not undo a stored
	// backup, and `vaultd reindex` puts it right.
	if r.Index != nil {
		if err := r.Index.Append(ctx, manifest.NewEntry(m, manifestKey)); err != nil {
			log.WarnContext(ctx, "the backup is stored but the index was not updated; run `vaultd reindex`",
				"error", err)
		}
	}

	log.InfoContext(ctx, "backup finished",
		"key", dataKey,
		"manifest", manifestKey,
		"bytes", m.Object.Bytes,
		"plaintext_bytes", m.Plaintext.Bytes,
		"duration_ms", m.DurationMS)

	notify.Emit(ctx, r.Notify, log, succeededEvent(m))

	return m, nil
}

// recordFailure appends a failure entry to the index. It runs on a context
// detached from the run's own, because a timeout or a cancellation is exactly
// the case worth recording.
func (r *Runner) recordFailure(ctx context.Context, spec Spec, started time.Time, runErr error) {
	if r.Index == nil {
		return
	}

	phase := PhaseDump
	var failure *Error
	if errors.As(runErr, &failure) {
		phase = failure.Phase
	}

	entry := manifest.NewFailureEntry(spec.Target, started.UTC(), r.now().UTC(), string(phase), runErr.Error())

	ctx = context.WithoutCancel(ctx)
	if err := r.Index.Append(ctx, entry); err != nil {
		r.log().WarnContext(ctx, "could not record the failure in the index",
			"target", spec.Target, "error", err)
	}
}

// reserveWindow is how far the timestamp may be nudged before giving up. A
// minute of one-second collisions means something is invoking backups in a
// loop, which is a problem to report rather than to work around.
const reserveWindow = 60

// reserve finds a timestamp whose keys are not taken yet.
func (r *Runner) reserve(ctx context.Context, spec Spec, kind manifest.Kind, at time.Time) (time.Time, error) {
	for range reserveWindow {
		manifestTaken, err := r.exists(ctx, spec.Layout.Manifest(at, kind))
		if err != nil {
			return time.Time{}, err
		}
		dataTaken, err := r.exists(ctx, spec.Layout.Data(at, kind, spec.extension()))
		if err != nil {
			return time.Time{}, err
		}
		if !manifestTaken && !dataTaken {
			return at, nil
		}
		at = at.Add(time.Second)
	}

	return time.Time{}, fmt.Errorf("every key in the %d seconds from %s is already taken",
		reserveWindow, at.Add(-reserveWindow*time.Second).Format(manifest.TimeFormat))
}

func (r *Runner) exists(ctx context.Context, key string) (bool, error) {
	_, err := r.Store.Head(ctx, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, core.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("checking whether %s is free: %w", key, err)
	}
}

// stream runs one dump through the pipeline into one object, attributing a
// failure to the side that caused it: a dump that died is a database problem,
// a failed upload is a storage problem, and they are handled differently.
func (r *Runner) stream(
	ctx context.Context,
	spec Spec,
	key string,
	dump func(context.Context, io.Writer) (core.DumpResult, error),
) (core.DumpResult, pipeline.Result, error) {
	var (
		result  core.DumpResult
		dumpErr error
		putErr  error
	)

	sums, err := pipeline.Run(ctx, spec.Pipeline,
		func(ctx context.Context, w io.Writer) error {
			result, dumpErr = dump(ctx, w)
			return dumpErr
		},
		func(ctx context.Context, body io.Reader) error {
			_, putErr = r.Store.Put(ctx, key, body, core.PutOptions{
				ContentType: "application/octet-stream",
				PartSize:    spec.PartSize,
				Concurrency: spec.Concurrency,
			})
			return putErr
		},
	)
	if err != nil {
		return core.DumpResult{}, pipeline.Result{}, r.classify(spec, result, dumpErr, putErr, err)
	}
	return result, sums, nil
}

func (r *Runner) classify(spec Spec, dump core.DumpResult, dumpErr, putErr, err error) error {
	switch {
	case dumpErr != nil:
		return &Error{Phase: PhaseDump, Target: spec.Target, Err: dumpErr, Stderr: dump.StderrTail}
	case putErr != nil:
		return &Error{Phase: PhaseUpload, Target: spec.Target, Err: putErr}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Phase: PhaseDump, Target: spec.Target, Err: fmt.Errorf("timed out after %s: %w", spec.Timeout, err)}
	default:
		return &Error{Phase: PhaseDump, Target: spec.Target, Err: err, Stderr: dump.StderrTail}
	}
}

func (r *Runner) putManifest(ctx context.Context, key string, m *manifest.Manifest) error {
	body, err := m.Marshal()
	if err != nil {
		return err
	}

	_, err = r.Store.Put(ctx, key, newReader(body), core.PutOptions{ContentType: "application/json"})
	return err
}

func (r *Runner) globals() (core.GlobalsDumper, bool) {
	globals, ok := r.Dumper.(core.GlobalsDumper)
	if !ok || !globals.HasGlobals() {
		return nil, false
	}
	return globals, true
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

func (s Spec) kind() manifest.Kind {
	if s.Kind == "" {
		return manifest.KindFull
	}
	return s.Kind
}

func (s Spec) extension() string {
	return manifest.DataExtension(s.Engine, s.Pipeline.Compression.String(), s.Pipeline.Encryption.String())
}

// globalsExtension mirrors the data extension, but the globals dump is plain
// SQL rather than a custom-format archive.
func globalsExtension(spec Spec) string {
	return manifest.GlobalsExtension(spec.Pipeline.Compression.String(), spec.Pipeline.Encryption.String())
}

// tablesOf prefers what the dumper reported over what the probe saw: by the
// time a dump finishes, it knows exactly which tables it wrote.
func tablesOf(info core.ServerInfo, dump core.DumpResult) []core.TableInfo {
	if len(dump.Tables) > 0 {
		return dump.Tables
	}
	return info.Tables
}
