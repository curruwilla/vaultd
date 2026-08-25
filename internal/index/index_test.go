package index_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/index"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

var at = time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)

func layout() manifest.Layout { return manifest.Layout{Prefix: "prod", Target: "prod-pg"} }

func entry(id string, when time.Time) manifest.Entry {
	return manifest.Entry{
		ID:          id,
		Target:      "prod-pg",
		Engine:      core.EnginePostgres,
		Outcome:     manifest.OutcomeSucceeded,
		Kind:        manifest.KindFull,
		Tier:        "daily",
		StartedAt:   when,
		FinishedAt:  when,
		Key:         "prod/prod-pg/" + id + ".pgdump.zst.age",
		ManifestKey: "prod/prod-pg/" + id + ".manifest.json",
		Bytes:       1024,
	}
}

func TestLoadMissingIndexIsEmpty(t *testing.T) {
	store := index.New(memory.New(), layout())

	loaded, err := store.Load(t.Context())

	require.NoError(t, err)
	assert.Empty(t, loaded.Entries)
	assert.Empty(t, loaded.ETag, "an empty ETag is what marks an index that does not exist yet")
}

func TestAppendCreatesAndGrowsTheIndex(t *testing.T) {
	store := index.New(memory.New(), layout())

	require.NoError(t, store.Append(t.Context(), entry("01A", at)))
	require.NoError(t, store.Append(t.Context(), entry("01B", at.Add(time.Hour))))

	loaded, err := store.Load(t.Context())
	require.NoError(t, err)

	require.Len(t, loaded.Entries, 2)
	assert.Equal(t, "01A", loaded.Entries[0].ID, "the index is append-only, oldest first")
	assert.Equal(t, "01B", loaded.Entries[1].ID)
	assert.NotEmpty(t, loaded.ETag)
}

func TestLatestAndBackups(t *testing.T) {
	store := index.New(memory.New(), layout())

	require.NoError(t, store.Append(t.Context(), entry("01A", at)))
	require.NoError(t, store.Append(t.Context(),
		manifest.NewFailureEntry("prod-pg", at.Add(time.Hour), at.Add(time.Hour), "dump", "pg_dump exited with code 1")))

	loaded, err := store.Load(t.Context())
	require.NoError(t, err)

	latest, ok := loaded.Latest()
	require.True(t, ok)
	assert.False(t, latest.Succeeded(), "the most recent attempt failed")
	assert.Equal(t, "dump", latest.Phase)

	backups := loaded.Backups()
	require.Len(t, backups, 1, "a failure is not a backup")
	assert.Equal(t, "01A", backups[0].ID)
}

// TestAppendRetriesOnAConcurrentWrite: two writers race, the loser re-reads
// and appends again rather than overwriting entries it never saw.
func TestAppendRetriesOnAConcurrentWrite(t *testing.T) {
	backing := memory.New()
	racing := &raceOnce{Store: backing, layout: layout()}
	store := index.New(racing, layout())

	require.NoError(t, store.Append(t.Context(), entry("01A", at)))
	require.NoError(t, store.Append(t.Context(), entry("01B", at.Add(time.Hour))))

	loaded, err := index.New(backing, layout()).Load(t.Context())
	require.NoError(t, err)

	// The racing writer got there first, so its entry leads; ours was
	// re-applied on top rather than overwriting it.
	require.Len(t, loaded.Entries, 3, "no entry may be lost to a concurrent write")
	assert.Equal(t, []string{"01RACE", "01A", "01B"}, []string{
		loaded.Entries[0].ID, loaded.Entries[1].ID, loaded.Entries[2].ID,
	})
}

func TestAppendGivesUpAfterPersistentConflicts(t *testing.T) {
	store := index.New(&alwaysConflicts{Store: memory.New()}, layout())

	err := store.Append(t.Context(), entry("01A", at))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "vaultd reindex")
}

func TestReplaceRewritesEverything(t *testing.T) {
	store := index.New(memory.New(), layout())
	require.NoError(t, store.Append(t.Context(), entry("01A", at)))
	require.NoError(t, store.Append(t.Context(), entry("01B", at.Add(time.Hour))))

	require.NoError(t, store.Replace(t.Context(), []manifest.Entry{entry("01B", at.Add(time.Hour))}))

	loaded, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Len(t, loaded.Entries, 1)
	assert.Equal(t, "01B", loaded.Entries[0].ID)
}

func TestRebuildReadsTheManifests(t *testing.T) {
	backing := memory.New()
	seedManifests(t, backing, at, at.Add(24*time.Hour))

	entries, err := index.New(backing, layout()).Rebuild(t.Context())

	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.True(t, entries[0].FinishedAt.Before(entries[1].FinishedAt), "oldest first")
	assert.Equal(t, core.EnginePostgres, entries[0].Engine)
	assert.True(t, entries[0].Succeeded())
}

// TestEntriesFallsBackToTheManifests is what makes the index a cache rather
// than a second source of truth.
func TestEntriesFallsBackToTheManifests(t *testing.T) {
	backing := memory.New()
	seedManifests(t, backing, at)
	store := index.New(backing, layout())

	entries, cached, err := store.Entries(t.Context())
	require.NoError(t, err)
	assert.False(t, cached, "there is no index yet")
	require.Len(t, entries, 1)

	require.NoError(t, store.Replace(t.Context(), entries))

	entries, cached, err = store.Entries(t.Context())
	require.NoError(t, err)
	assert.True(t, cached)
	require.Len(t, entries, 1)
}

func TestRebuildRejectsACorruptManifest(t *testing.T) {
	backing := memory.New()
	_, err := backing.Put(t.Context(), "prod/prod-pg/2026/08/24/x-full.manifest.json",
		bytes.NewReader([]byte(`{"schema": 99}`)), core.PutOptions{})
	require.NoError(t, err)

	_, err = index.New(backing, layout()).Rebuild(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema 99")
}

func seedManifests(t *testing.T, store core.Store, times ...time.Time) {
	t.Helper()

	for i, when := range times {
		m := &manifest.Manifest{
			Schema:     manifest.Schema,
			ID:         manifest.NewID(when),
			Target:     "prod-pg",
			Engine:     core.EnginePostgres,
			Kind:       manifest.KindFull,
			Tier:       "daily",
			StartedAt:  when,
			FinishedAt: when,
			Object:     manifest.Object{Key: layout().Data(when, manifest.KindFull, ".pgdump"), Bytes: int64(1000 * (i + 1))},
		}

		raw, err := m.Marshal()
		require.NoError(t, err)

		_, err = store.Put(t.Context(), layout().Manifest(when, manifest.KindFull), bytes.NewReader(raw), core.PutOptions{})
		require.NoError(t, err)
	}
}

// raceOnce lets the first conditional write fail after slipping another
// writer's entry in, the way a second daemon replica would.
type raceOnce struct {
	*memory.Store
	layout manifest.Layout
	raced  bool
}

func (r *raceOnce) PutIfMatch(ctx context.Context, key string, b []byte, etag string) (core.ObjectInfo, bool, error) {
	if !r.raced {
		r.raced = true

		current, err := r.readIndex(ctx)
		if err != nil {
			return core.ObjectInfo{}, false, err
		}
		withRacer, err := manifest.AppendEntry(current, entry("01RACE", at.Add(30*time.Minute)))
		if err != nil {
			return core.ObjectInfo{}, false, err
		}
		if _, err := r.Put(ctx, key, bytes.NewReader(withRacer), core.PutOptions{}); err != nil {
			return core.ObjectInfo{}, false, err
		}
		return core.ObjectInfo{}, false, nil
	}
	return r.Store.PutIfMatch(ctx, key, b, etag) //nolint:staticcheck // the embedded method is the one being wrapped
}

func (r *raceOnce) readIndex(ctx context.Context) ([]byte, error) {
	body, err := r.Get(ctx, r.layout.Index())
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// alwaysConflicts never accepts a conditional write.
type alwaysConflicts struct{ *memory.Store }

func (a *alwaysConflicts) PutIfMatch(context.Context, string, []byte, string) (core.ObjectInfo, bool, error) {
	return core.ObjectInfo{}, false, nil
}
