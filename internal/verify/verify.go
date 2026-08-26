// Package verify checks that a stored backup is still what its manifest says
// it is (SPEC §8).
//
// Three levels live here. L0 asks the bucket what it holds — cheap enough to
// run after every backup. L1 reads the object back in full, decrypts it,
// decompresses it and compares the plaintext checksum with the manifest, which
// is the check that actually catches a corrupted or truncated backup. L2
// restores the backup into a database of its own on a staging server and runs
// the configured assertions against it, which is the only check that proves a
// backup comes back rather than that its bytes are intact (restore.go).
package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"filippo.io/age"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/notify"
	"github.com/curruwilla/vaultd/internal/pipeline"
)

// Level is how hard a verification looks.
type Level string

const (
	// LevelIntegrity asks the store what it holds: the object exists and is
	// the size the manifest recorded.
	LevelIntegrity Level = "integrity"
	// LevelStructural reads the whole object back and checks it against the
	// manifest, byte for byte.
	LevelStructural Level = "structural"
	// LevelRestore restores into an ephemeral database on the verify target
	// and asserts against what came back.
	LevelRestore Level = "restore"
)

// Levels lists the levels in increasing cost.
var Levels = []Level{LevelIntegrity, LevelStructural, LevelRestore}

// Result is what one verification found.
type Result struct {
	Level Level     `json:"level"`
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	// Problems are what makes a backup fail. An empty list with OK false means
	// the verification could not run at all.
	Problems []string       `json:"problems,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	// Skipped says the check could not be attempted for a reason that is not
	// the backup's fault — a staging server older than the source, a topology
	// that cannot be restored into an ephemeral database. It is neither a pass
	// nor a failure: nothing is recorded, so a backup verified last week keeps
	// the verification it earned (SPEC §8).
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Duration is how long the check took, which is what makes L1 worth
	// scheduling rather than running everywhere.
	Duration time.Duration `json:"-"`
}

// Summary renders the result as one line.
func (r Result) Summary(target string) string {
	if r.Skipped {
		return fmt.Sprintf("skipped: %s was not verified: %s", target, r.Reason)
	}
	if r.OK {
		return fmt.Sprintf("ok: %s passed %s verification", target, r.Level)
	}
	if len(r.Problems) == 0 {
		return fmt.Sprintf("%s failed %s verification", target, r.Level)
	}
	return fmt.Sprintf("%s failed %s verification: %s", target, r.Level, r.Problems[0])
}

// Verifier checks backups of one destination.
type Verifier struct {
	Store core.Store
	Index *index.Store
	// Identities decrypt age-encrypted backups. The private key lives off the
	// backup host by design (SPEC §15), so it is supplied for the run rather
	// than read from the config.
	Identities []age.Identity
	// Passphrase decrypts a backup taken in passphrase mode; that one does
	// live in the config, so it comes from there.
	Passphrase string
	// Sandbox provisions the ephemeral databases restore verification writes
	// into. It is nil unless the target declares a verify target, which is why
	// `--level restore` then refuses rather than restoring somewhere nobody
	// asked for.
	Sandbox core.Provisioner
	// DatabasePrefix names those databases. It is the verify target's own
	// prefix, and the only namespace vaultd will create or drop in.
	DatabasePrefix string
	// Assertions are the checks run against a restored backup.
	Assertions []Assertion
	// RestoreTimeout bounds one L2 restore; zero means the context decides.
	RestoreTimeout time.Duration
	// Notify receives verify.succeeded and verify.failed (SPEC §12). A skipped
	// check sends nothing: it found no problem, and paging someone because a
	// staging server is a major behind would train them to ignore the channel.
	Notify core.Notifier
	Now    func() time.Time
	Log    *slog.Logger
}

// Backup verifies one indexed backup at the given level and records the
// outcome on its manifest and in the index.
func (v *Verifier) Backup(ctx context.Context, entry manifest.Entry, level Level) (Result, error) {
	if !entry.Succeeded() {
		return Result{}, fmt.Errorf("%s is a failed run, not a backup", entry.FinishedAt.Format(time.RFC3339))
	}

	m, err := v.Index.Manifest(ctx, entry.ManifestKey)
	if err != nil {
		return Result{}, err
	}

	result, err := v.Manifest(ctx, m, level)
	if err != nil {
		return Result{}, err
	}

	if result.Skipped {
		// Nothing ran, so nothing is recorded: writing this down would replace
		// the verification the backup already has with the absence of one.
		return result, nil
	}

	notify.Emit(ctx, v.Notify, v.Log, verifiedEvent(m, result))

	if err := v.record(ctx, m, entry.ManifestKey, result); err != nil {
		// The check itself succeeded; failing to write the outcome down is
		// worth reporting but does not change what was found.
		return result, fmt.Errorf("the verification ran but its result was not recorded: %w", err)
	}
	return result, nil
}

// Manifest verifies the backup a manifest describes, without recording
// anything. It is the seam the tests and, later, the daemon use.
func (v *Verifier) Manifest(ctx context.Context, m *manifest.Manifest, level Level) (Result, error) {
	started := v.now()

	var (
		result Result
		err    error
	)
	switch level {
	case LevelIntegrity:
		result, err = v.integrity(ctx, m)
	case LevelStructural:
		result, err = v.structural(ctx, m)
	case LevelRestore:
		result, err = v.restore(ctx, m)
	default:
		return Result{}, fmt.Errorf("unknown verify level %q; use one of integrity, structural, restore", level)
	}
	if err != nil {
		return Result{}, err
	}

	result.Level = level
	result.At = started.UTC()
	result.Duration = v.now().Sub(started)
	return result, nil
}

// integrity is L0: what the store says about the objects, checked against what
// the manifest claims. It costs one HEAD per object and no egress, which is
// why it can run after every backup.
func (v *Verifier) integrity(ctx context.Context, m *manifest.Manifest) (Result, error) {
	result := Result{OK: true, Details: map[string]any{}}

	objects := []struct {
		what   string
		object manifest.Object
	}{{"object", m.Object}}
	if m.Globals != nil {
		objects = append(objects, struct {
			what   string
			object manifest.Object
		}{"globals", *m.Globals})
	}

	for _, target := range objects {
		info, err := v.Store.Head(ctx, target.object.Key)
		if errors.Is(err, core.ErrNotFound) {
			result.OK = false
			result.Problems = append(result.Problems, fmt.Sprintf("the %s %s is missing from the bucket", target.what, target.object.Key))
			continue
		}
		if err != nil {
			return Result{}, err
		}

		result.Details[target.what+"_bytes"] = info.Bytes
		if info.Bytes != target.object.Bytes {
			result.OK = false
			result.Problems = append(result.Problems, fmt.Sprintf(
				"the %s is %d bytes in the bucket but the manifest records %d",
				target.what, info.Bytes, target.object.Bytes))
		}
		// Some stores carry our own checksum in object metadata; when they do,
		// it is worth comparing without paying for the download.
		if info.SHA256 != "" && info.SHA256 != target.object.SHA256 {
			result.OK = false
			result.Problems = append(result.Problems, fmt.Sprintf(
				"the %s checksum in the bucket does not match the manifest", target.what))
		}
	}

	return result, nil
}

// structural is L1: read the object back, undo the pipeline, and compare what
// comes out with what the manifest recorded. Everything streams, so a 4TB
// backup costs bandwidth and CPU but not memory or disk.
func (v *Verifier) structural(ctx context.Context, m *manifest.Manifest) (Result, error) {
	result, err := v.integrity(ctx, m)
	if err != nil {
		return Result{}, err
	}
	if !result.OK {
		// No point reading an object the store says is the wrong size.
		return result, nil
	}

	spec, err := pipeline.ParseSpec(m.Pipeline.Compression, m.Pipeline.Encryption)
	if err != nil {
		return Result{}, err
	}
	spec.Encryption.Passphrase = v.Passphrase

	if err := v.canDecrypt(spec); err != nil {
		return Result{}, err
	}

	body, err := v.Store.Get(ctx, m.Object.Key)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	// The ciphertext is checksummed on the way in, so a corrupted object is
	// caught even when age's own authentication would have caught it later.
	ciphertext := newSum()
	plaintext := newSum()
	validator := ValidatorFor(m.Engine)

	reader, err := spec.Reader(io.TeeReader(body, ciphertext), v.Identities...)
	if err != nil {
		// A key that matches nothing is the operator's problem, and reporting
		// the backup as broken would be a lie. Anything else at this point —
		// a header that will not parse — is the object itself.
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return Result{}, errors.New("none of the supplied identities can decrypt this backup; check --identity-file")
		}

		result.OK = false
		result.Problems = append(result.Problems, fmt.Sprintf("the backup could not be read back: %s", err))
		v.addCiphertext(&result, m, ciphertext.Sum())
		return result, nil
	}
	defer reader.Close()

	if _, err := io.Copy(io.MultiWriter(plaintext, validator), reader); err != nil {
		// A decryption or decompression failure is the backup failing, not the
		// tool: an authentication error is exactly what a flipped byte
		// produces.
		result.OK = false
		result.Problems = append(result.Problems, fmt.Sprintf("the backup could not be read back: %s", err))
		v.addCiphertext(&result, m, ciphertext.Sum())
		return result, nil
	}

	v.addCiphertext(&result, m, ciphertext.Sum())

	plain := plaintext.Sum()
	result.Details["plaintext_bytes"] = plain.Bytes
	result.Details["plaintext_sha256"] = plain.SHA256

	if plain.Bytes != m.Plaintext.Bytes {
		result.OK = false
		result.Problems = append(result.Problems, fmt.Sprintf(
			"the restored stream is %d bytes but the manifest records %d", plain.Bytes, m.Plaintext.Bytes))
	}
	if plain.SHA256 != m.Plaintext.SHA256 {
		result.OK = false
		result.Problems = append(result.Problems,
			"the checksum of the restored stream does not match the manifest")
	}
	if err := validator.Validate(); err != nil {
		result.OK = false
		result.Problems = append(result.Problems, err.Error())
	} else {
		result.Details["format"] = validator.Describe()
	}

	return result, nil
}

func (v *Verifier) addCiphertext(result *Result, m *manifest.Manifest, sum sum) {
	result.Details["object_sha256"] = sum.SHA256
	if sum.Bytes == m.Object.Bytes && sum.SHA256 != m.Object.SHA256 {
		result.OK = false
		result.Problems = append(result.Problems,
			"the checksum of the stored object does not match the manifest")
	}
}

// canDecrypt refuses early when the key for this backup is not present, rather
// than reporting a good backup as broken.
func (v *Verifier) canDecrypt(spec pipeline.Spec) error {
	switch spec.Encryption.Mode {
	case pipeline.ModeAge:
		if len(v.Identities) == 0 {
			return errors.New("this backup is age-encrypted; pass the private key with --identity-file to verify it")
		}
	case pipeline.ModePassphrase:
		if v.Passphrase == "" {
			return errors.New("this backup is encrypted with a passphrase; the config must supply it to verify")
		}
	}
	return nil
}

// record writes the outcome onto the manifest and into the index. The manifest
// is the source of truth for a backup's verification state, and retention
// reads it through the index (SPEC §7, invariant 2).
func (v *Verifier) record(ctx context.Context, m *manifest.Manifest, manifestKey string, result Result) error {
	m.Verify = &manifest.Verify{
		Level:   string(result.Level),
		At:      result.At,
		OK:      result.OK,
		Details: result.Details,
	}
	if len(result.Problems) > 0 {
		if m.Verify.Details == nil {
			m.Verify.Details = map[string]any{}
		}
		m.Verify.Details["problems"] = result.Problems
	}

	raw, err := m.Marshal()
	if err != nil {
		return err
	}
	if _, err := v.Store.Put(ctx, manifestKey, bytesReader(raw), core.PutOptions{ContentType: "application/json"}); err != nil {
		return fmt.Errorf("rewriting the manifest: %w", err)
	}

	if v.Index == nil {
		return nil
	}
	return v.Index.SetVerification(ctx, m.ID, string(result.Level), result.OK, result.At)
}

func (v *Verifier) log() *slog.Logger {
	if v.Log == nil {
		return slog.Default()
	}
	return v.Log
}

func (v *Verifier) now() time.Time {
	if v.Now == nil {
		return time.Now().UTC()
	}
	return v.Now().UTC()
}
