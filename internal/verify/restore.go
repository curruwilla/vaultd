package verify

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/pipeline"
	"github.com/curruwilla/vaultd/internal/restore"
)

// dropTimeout bounds the drop of a verify database. It runs on a context of
// its own — the usual reason to be dropping one is that the run was cancelled
// — so it needs a deadline that is not the cancelled one.
const dropTimeout = 2 * time.Minute

// problemBytes is how much of a client's output a problem carries. The whole
// stderr tail belongs in a failure manifest; a verification result is read as
// a sentence.
const problemBytes = 1000

// restore is L2: the backup is restored into a database of its own on the
// verify target, the assertions are run against what came back, and the
// database is dropped (SPEC §8, decision D3).
//
// It is the only check that proves a backup restores, because it is the only
// one that restores it. L0 and L1 prove the bytes are intact; a dump whose
// bytes are perfect and whose schema the server refuses is still a backup that
// does not come back.
func (v *Verifier) restore(ctx context.Context, m *manifest.Manifest) (Result, error) {
	if v.Sandbox == nil {
		return Result{}, fmt.Errorf(
			"target %q has no verify target to restore into; set verify.into and declare it under verify_targets", m.Target)
	}

	// The key is checked before a staging database is created, so a missing
	// one costs nothing to discover.
	spec, err := pipeline.ParseSpec(m.Pipeline.Compression, m.Pipeline.Encryption)
	if err != nil {
		return Result{}, err
	}
	spec.Encryption.Passphrase = v.Passphrase
	if err := v.canDecrypt(spec); err != nil {
		return Result{}, err
	}

	info, err := v.Sandbox.Probe(ctx)
	if err != nil {
		return Result{}, err
	}
	if reason := versionSkip(m, info); reason != "" {
		return Result{Skipped: true, Reason: reason}, nil
	}

	name := sandboxName(v.DatabasePrefix, m.ID)
	log := v.log().With("target", m.Target, "backup", m.ID, "database", name)

	box, err := v.Sandbox.Create(ctx, core.SandboxSpec{Name: name, Tables: m.Tables})
	if errors.Is(err, core.ErrSandboxUnsupported) {
		return Result{Skipped: true, Reason: err.Error()}, nil
	}
	if err != nil {
		return Result{}, err
	}

	// Dropping it is the one thing that has to happen whatever else does: a
	// database left behind on a staging server costs disk until somebody
	// notices, and `verify --gc` is the second line of defence, not the first.
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
		defer cancel()

		if err := box.Drop(dropCtx); err != nil {
			log.WarnContext(ctx, "the verify database was left behind; collect it with `vaultd verify --gc`",
				"error", err)
		}
	}()

	result := Result{
		OK: true,
		Details: map[string]any{
			"verify_database":       name,
			"verify_target_version": info.Version,
		},
	}
	log.InfoContext(ctx, "restore verification started", "engine", string(m.Engine), "bytes", m.Object.Bytes)

	runner := &restore.Runner{Store: v.Store, Restorer: box, Now: v.Now, Log: v.Log}
	restored, err := runner.Run(ctx, m, restore.Spec{
		Identities: v.Identities,
		Passphrase: v.Passphrase,
		Timeout:    v.RestoreTimeout,
	})
	if err != nil {
		if failure, ok := cannotRun(ctx, err); ok {
			return Result{}, failure
		}

		// Everything else — a client that exited non-zero, a stream that would
		// not decrypt, a checksum that did not match — is the backup failing
		// to come back, which is a finding rather than an error.
		result.OK = false
		result.Problems = append(result.Problems, "the backup did not restore: "+problemText(err))
		if restored.Bytes > 0 {
			result.Details["plaintext_bytes"] = restored.Bytes
		}
		return result, nil
	}

	result.Details["plaintext_bytes"] = restored.Bytes
	result.Details["plaintext_sha256"] = restored.SHA256
	result.Details["restore_ms"] = restored.Duration.Milliseconds()

	checks, err := runAssertions(ctx, box, m, v.Assertions, v.now())
	if err != nil {
		return Result{}, err
	}
	if len(checks) > 0 {
		result.Details["assertions"] = checks
		for _, check := range checks {
			if !check.OK {
				result.OK = false
				result.Problems = append(result.Problems, check.Detail)
			}
		}
	}

	return result, nil
}

// CollectGarbage drops the verify databases a crashed run left behind
// (SPEC §8). The drop of a healthy run is deferred and happens whatever the
// outcome; this is what covers the process that never got to run it.
//
// It drops every database carrying the prefix, so it must not run while
// another verification is in flight — which is why it is a flag an operator
// passes, not something a check does on its own.
func (v *Verifier) CollectGarbage(ctx context.Context) ([]string, error) {
	if v.Sandbox == nil {
		return nil, errors.New("this target has no verify target; --gc collects the databases restore verification creates")
	}

	names, err := v.Sandbox.List(ctx)
	if err != nil {
		return nil, err
	}

	dropped := make([]string, 0, len(names))
	for _, name := range names {
		if err := v.Sandbox.Drop(ctx, name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

// versionSkip reports why this staging server cannot restore this backup, or
// the empty string when it can.
//
// A server older than the one the backup came from cannot read the dump, and
// that is not the backup being broken: it comes back as skipped, with the
// reason, rather than as a failure that would mark a good backup unverified
// (SPEC §8).
func versionSkip(m *manifest.Manifest, info core.ServerInfo) string {
	source, staging := majorVersion(m.ServerVersion), majorVersion(info.Version)
	if source == 0 || staging == 0 {
		// One of them does not say; attempting the restore reports more than
		// refusing to.
		return ""
	}
	if staging < source {
		return fmt.Sprintf(
			"the verify target runs %s and this backup came from %s; restoring needs a server at least as new",
			info.Version, m.ServerVersion)
	}
	return ""
}

// majorVersion reads the leading number of a version string: 17.2, 8.0.36 and
// 7.0.14 all report what a restore actually cares about.
func majorVersion(version string) int {
	digits := version
	for i := range len(version) {
		if version[i] < '0' || version[i] > '9' {
			digits = version[:i]
			break
		}
	}

	major, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return major
}

// sandboxName is the database one verification gets: the configured prefix and
// the backup id. Lowercased, because PostgreSQL folds unquoted identifiers and
// a mixed-case database name is a nuisance for whoever has to type it after a
// crash.
func sandboxName(prefix, id string) string {
	return prefix + strings.ToLower(id)
}

// cannotRun separates the errors that mean the check never ran from the ones
// that mean the backup is broken. Reporting a good backup as broken because
// the operator passed the wrong key would be worse than saying nothing.
func cannotRun(ctx context.Context, err error) (error, bool) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return err, true
	}

	var noMatch *age.NoIdentityMatchError
	if errors.As(err, &noMatch) {
		return errors.New("none of the supplied identities can decrypt this backup; check --identity-file"), true
	}

	// The sandbox was created empty a moment ago. If something else is in it,
	// the staging server is not in the state this check needs.
	if errors.Is(err, restore.ErrDestinationNotEmpty) {
		return fmt.Errorf("the verify database is not empty: %w; run `vaultd verify --gc`", err), true
	}
	return nil, false
}

// problemText reduces a client failure to something a manifest and a webhook
// can carry. The full stderr tail belongs on a failed backup, not on a line an
// operator reads.
func problemText(err error) string {
	text := oneLine(err.Error())
	if len(text) > problemBytes {
		return text[:problemBytes] + "…"
	}
	return text
}
