package verify_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/storage/memory"
	"github.com/curruwilla/vaultd/internal/verify"
)

// fakeProvisioner is a verify target that lives in memory. It hands out one
// sandbox, remembers every database it was asked to create and drop, and lets
// a test decide what the restore and the assertions find.
type fakeProvisioner struct {
	version   string
	existing  []string
	createErr error

	// What the sandbox it hands out will do.
	tables     []string
	rows       map[string]int64
	restoreErr error
	// halfRead makes the sandbox consume half the stream and report success —
	// a client that exits 0 having applied part of a backup.
	halfRead bool

	created []string
	dropped []string
	box     *fakeSandbox
}

func (p *fakeProvisioner) Probe(context.Context) (core.ServerInfo, error) {
	return core.ServerInfo{Engine: core.EnginePostgres, Version: p.version}, nil
}

func (p *fakeProvisioner) Create(_ context.Context, spec core.SandboxSpec) (core.Sandbox, error) {
	if p.createErr != nil {
		return nil, p.createErr
	}
	p.created = append(p.created, spec.Name)
	p.box = &fakeSandbox{provisioner: p, name: spec.Name}
	return p.box, nil
}

func (p *fakeProvisioner) List(context.Context) ([]string, error) { return p.existing, nil }

func (p *fakeProvisioner) Drop(_ context.Context, name string) error {
	p.dropped = append(p.dropped, name)
	return nil
}

type fakeSandbox struct {
	provisioner *fakeProvisioner
	name        string
	written     []byte
}

func (s *fakeSandbox) Name() string { return s.name }

func (s *fakeSandbox) IsEmpty(context.Context) (bool, error) { return true, nil }

func (s *fakeSandbox) Drop(ctx context.Context) error { return s.provisioner.Drop(ctx, s.name) }

func (s *fakeSandbox) Restore(_ context.Context, r io.Reader) error {
	if s.provisioner.restoreErr != nil {
		return s.provisioner.restoreErr
	}

	if s.provisioner.halfRead {
		// Read part of the stream and report success, which is what a client
		// that exits 0 halfway through the archive does.
		partial, err := io.ReadAll(io.LimitReader(r, 100))
		s.written = partial
		return err
	}

	written, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.written = written
	return nil
}

func (s *fakeSandbox) Tables(context.Context) ([]string, error) {
	if s.provisioner.tables != nil {
		return s.provisioner.tables, nil
	}

	names := make([]string, 0, len(s.provisioner.rows))
	for name := range s.provisioner.rows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *fakeSandbox) CountRows(_ context.Context, table string) (int64, error) {
	return s.provisioner.rows[table], nil
}

func (s *fakeSandbox) Scalar(context.Context, string) (any, error) {
	return nil, core.ErrQueryUnsupported
}

func TestRestoreVerificationPasses(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())
	m.Tables = []core.TableInfo{
		{Name: "public.users", Rows: 2000, RowsExact: true},
		{Name: "public.orders", Rows: 5000, RowsExact: true},
	}

	provisioner := &fakeProvisioner{
		version: "17.2",
		rows:    map[string]int64{"public.users": 2000, "public.orders": 5000},
	}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"
	v.Assertions = []verify.Assertion{
		{Type: verify.AssertTableCount},
		{Type: verify.AssertRowCount},
		{Type: verify.AssertMaxAge, MaxAge: 26 * time.Hour},
	}

	result, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.NoError(t, err)
	assert.True(t, result.OK, "problems: %v", result.Problems)
	assert.Equal(t, verify.LevelRestore, result.Level)

	// The backup really went through the pipeline and came out the other side.
	require.NotNil(t, provisioner.box)
	assert.Equal(t, pgDump(), provisioner.box.written)

	// The database is named after the backup, and it is gone afterwards.
	require.Len(t, provisioner.created, 1)
	assert.Equal(t, "vaultd_verify_"+strings.ToLower(m.ID), provisioner.created[0])
	assert.Equal(t, provisioner.created, provisioner.dropped)

	checks, ok := result.Details["assertions"].([]verify.Check)
	require.True(t, ok, "the assertions belong on the result: %v", result.Details)
	assert.Len(t, checks, 4) // table_count, one row_count per table, max_age
}

// TestRestoreVerificationReportsABackupThatWillNotRestore: the client refusing
// the dump is a finding about the backup, not an error from the tool. Anything
// else and a nightly verification turns a broken backup into a broken run.
func TestRestoreVerificationReportsABackupThatWillNotRestore(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	provisioner := &fakeProvisioner{
		version:    "17.2",
		restoreErr: errors.New("pg_restore exited 1: unsupported version (1.16) in file header"),
	}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	result, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.NoError(t, err, "a backup that will not restore is a finding, not an error")
	assert.False(t, result.OK)
	require.NotEmpty(t, result.Problems)
	assert.Contains(t, result.Problems[0], "the backup did not restore")
	assert.Contains(t, result.Problems[0], "unsupported version")

	// And the database is dropped anyway: a failed check must not leave one
	// behind on the staging server.
	assert.Equal(t, provisioner.created, provisioner.dropped)
}

// TestRestoreVerificationCatchesAPartialRestore: a client that exits 0 having
// consumed half the archive leaves a database holding half a backup. The
// checksum taken on the way past is what notices.
func TestRestoreVerificationCatchesAPartialRestore(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	provisioner := &fakeProvisioner{version: "17.2", halfRead: true}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	result, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.NoError(t, err)
	assert.False(t, result.OK)
	require.NotEmpty(t, result.Problems)
	assert.Contains(t, result.Problems[0], "not what was backed up")
}

// TestRestoreVerificationSkipsAnOlderVerifyTarget covers SPEC §8: a staging
// server older than the source cannot read the dump, and that is not the
// backup being broken.
func TestRestoreVerificationSkipsAnOlderVerifyTarget(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, manifestKey := stored(t, store, identity, pgDump())

	idx := index.New(store, layout())
	require.NoError(t, idx.Append(t.Context(), manifest.NewEntry(m, manifestKey)))

	provisioner := &fakeProvisioner{version: "15.6"}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	result, err := v.Backup(t.Context(), manifest.NewEntry(m, manifestKey), verify.LevelRestore)

	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.False(t, result.OK)
	assert.Contains(t, result.Reason, "at least as new")
	assert.Empty(t, provisioner.created, "nothing may be created for a check that cannot run")

	// Nothing is recorded: a skipped check must not replace the verification a
	// backup already earned, nor mark it as failed.
	loaded, err := idx.Load(t.Context())
	require.NoError(t, err)
	require.Len(t, loaded.Entries, 1)
	assert.Nil(t, loaded.Entries[0].VerifyOK)

	reloaded := storedManifest(t, store, manifestKey)
	assert.Nil(t, reloaded.Verify)
}

// TestRestoreVerificationSkipsWhatCannotBeSandboxed: an adapter that cannot
// give this backup a database of its own says so, and it is a skip for the
// same reason.
func TestRestoreVerificationSkipsWhatCannotBeSandboxed(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	provisioner := &fakeProvisioner{
		version:   "17.2",
		createErr: fmt.Errorf("%w: this backup holds 3 databases", core.ErrSandboxUnsupported),
	}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	result, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Contains(t, result.Reason, "holds 3 databases")
}

// TestRestoreVerificationNeedsAVerifyTarget: with nowhere declared to restore
// into, the answer is an error rather than a restore into somewhere else.
func TestRestoreVerificationNeedsAVerifyTarget(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	_, err := verifier(t, store, identity).Manifest(t.Context(), m, verify.LevelRestore)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify.into")
}

// TestRestoreVerificationNeedsTheKeyBeforeItTouchesTheServer: discovering the
// missing key after creating a database on the staging server would leave one
// behind for nothing.
func TestRestoreVerificationNeedsTheKey(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())

	provisioner := &fakeProvisioner{version: "17.2"}

	v := verifier(t, store, identity)
	v.Identities = nil
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	_, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--identity-file")
	assert.Empty(t, provisioner.created)
}

func TestRestoreVerificationFailsOnARowCountThatDoesNotMatch(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)
	m, _ := stored(t, store, identity, pgDump())
	m.Tables = []core.TableInfo{{Name: "public.users", Rows: 2000, RowsExact: true}}

	provisioner := &fakeProvisioner{version: "17.2", rows: map[string]int64{"public.users": 0}}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"
	v.Assertions = []verify.Assertion{{Type: verify.AssertRowCount}}

	result, err := v.Manifest(t.Context(), m, verify.LevelRestore)

	require.NoError(t, err)
	assert.False(t, result.OK)
	require.NotEmpty(t, result.Problems)
	assert.Contains(t, result.Problems[0], "came back with 0 rows")
}

// TestCollectGarbage covers the second line of defence: the databases a run
// that crashed never got to drop (SPEC §8).
func TestCollectGarbage(t *testing.T) {
	store := memory.New()
	identity := newIdentity(t)

	provisioner := &fakeProvisioner{
		version:  "17.2",
		existing: []string{"vaultd_verify_01jaaa", "vaultd_verify_01jbbb"},
	}

	v := verifier(t, store, identity)
	v.Sandbox = provisioner
	v.DatabasePrefix = "vaultd_verify_"

	dropped, err := v.CollectGarbage(t.Context())

	require.NoError(t, err)
	assert.Equal(t, provisioner.existing, dropped)
	assert.Equal(t, provisioner.existing, provisioner.dropped)
}

func TestCollectGarbageNeedsAVerifyTarget(t *testing.T) {
	store := memory.New()

	_, err := verifier(t, store, newIdentity(t)).CollectGarbage(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no verify target")
}
