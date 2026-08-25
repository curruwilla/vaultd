// Package index maintains the per-target listing cache in the bucket
// (SPEC §6.1).
//
// The manifests are the source of truth; this is a cache that makes a listing
// one GET instead of one per backup, and `vaultd reindex` rebuilds it from the
// manifests at any time. Because object stores have no append, "append-only"
// is enforced with a conditional write: a concurrent writer loses the race and
// retries rather than truncating entries it never saw.
package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
)

// appendAttempts bounds the read-modify-write retries. Contention on one
// target's index is rare — the lock in M7 makes it rarer still — so a handful
// of attempts is plenty, and failing loudly beats looping forever.
const appendAttempts = 5

// rebuildFetchers is how many manifests are read at once while rebuilding.
const rebuildFetchers = 8

// Index is the parsed contents of one target's index object.
type Index struct {
	Entries []manifest.Entry
	// ETag is what the entries were read at; it is what a conditional write
	// checks against. Empty means the index does not exist yet.
	ETag string
}

// Latest returns the newest entry, successful or not.
func (i *Index) Latest() (manifest.Entry, bool) {
	if len(i.Entries) == 0 {
		return manifest.Entry{}, false
	}

	latest := i.Entries[0]
	for _, entry := range i.Entries[1:] {
		if entry.FinishedAt.After(latest.FinishedAt) {
			latest = entry
		}
	}
	return latest, true
}

// Backups returns the entries that describe a stored backup, newest last.
func (i *Index) Backups() []manifest.Entry {
	out := make([]manifest.Entry, 0, len(i.Entries))
	for _, entry := range i.Entries {
		if entry.Succeeded() {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].FinishedAt.Before(out[b].FinishedAt) })
	return out
}

// Store reads and writes one target's index.
type Store struct {
	store  core.Store
	layout manifest.Layout
}

// New returns the index store of one target inside one destination.
func New(store core.Store, layout manifest.Layout) *Store {
	return &Store{store: store, layout: layout}
}

// Key is where this index lives.
func (s *Store) Key() string { return s.layout.Index() }

// Load reads the index. A missing index is not an error: it is what a target
// looks like before its first backup, or after the object was lost — `reindex`
// puts it back.
func (s *Store) Load(ctx context.Context) (*Index, error) {
	// The ETag is read first on purpose. If a writer lands between the two
	// calls, the ETag is stale and the next conditional write fails, which
	// costs a retry; reading it afterwards would let a stale write overwrite
	// entries that had already been committed.
	info, err := s.store.Head(ctx, s.Key())
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return &Index{}, nil
		}
		return nil, fmt.Errorf("reading the index: %w", err)
	}

	body, err := s.store.Get(ctx, s.Key())
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return &Index{}, nil
		}
		return nil, fmt.Errorf("reading the index: %w", err)
	}
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading the index: %w", err)
	}

	entries, err := manifest.ParseIndex(raw)
	if err != nil {
		return nil, err
	}
	return &Index{Entries: entries, ETag: info.ETag}, nil
}

// Append adds one entry, retrying if another writer got there first.
func (s *Store) Append(ctx context.Context, entry manifest.Entry) error {
	for attempt := range appendAttempts {
		current, err := s.Load(ctx)
		if err != nil {
			return err
		}

		raw, err := marshal(current.Entries)
		if err != nil {
			return err
		}
		updated, err := manifest.AppendEntry(raw, entry)
		if err != nil {
			return err
		}

		_, written, err := s.store.PutIfMatch(ctx, s.Key(), updated, current.ETag)
		if err != nil {
			return fmt.Errorf("updating the index: %w", err)
		}
		if written {
			return nil
		}
		_ = attempt // another writer won; reload and try again
	}

	return fmt.Errorf("the index of %s changed under %d successive attempts; run `vaultd reindex` if this persists",
		s.layout.Target, appendAttempts)
}

// Replace writes the index wholesale. Prune and reindex use it: both compute
// the complete contents, so there is nothing to merge.
func (s *Store) Replace(ctx context.Context, entries []manifest.Entry) error {
	raw, err := marshal(entries)
	if err != nil {
		return err
	}

	if _, err := s.store.Put(ctx, s.Key(), bytesReader(raw), core.PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		return fmt.Errorf("writing the index: %w", err)
	}
	return nil
}

// Remove drops the entries of the named backups, and the failure records that
// predate everything still kept — those describe a period nothing is retained
// from any more.
//
// Like Append it is a conditional read-modify-write: a backup that finished
// while prune was deleting must not lose its index entry and become invisible
// until the next reindex.
func (s *Store) Remove(ctx context.Context, deleted []string, oldestKept time.Time) error {
	gone := make(map[string]bool, len(deleted))
	for _, id := range deleted {
		gone[id] = true
	}

	for range appendAttempts {
		current, err := s.Load(ctx)
		if err != nil {
			return err
		}

		raw, err := marshal(prune(current.Entries, gone, oldestKept))
		if err != nil {
			return err
		}

		_, written, err := s.store.PutIfMatch(ctx, s.Key(), raw, current.ETag)
		if err != nil {
			return fmt.Errorf("updating the index: %w", err)
		}
		if written {
			return nil
		}
	}

	return fmt.Errorf("the index of %s changed under %d successive attempts; run `vaultd reindex` if this persists",
		s.layout.Target, appendAttempts)
}

// prune returns the entries that survive a retention pass.
func prune(entries []manifest.Entry, deleted map[string]bool, oldestKept time.Time) []manifest.Entry {
	out := make([]manifest.Entry, 0, len(entries))
	for _, entry := range entries {
		switch {
		case entry.Succeeded():
			if !deleted[entry.ID] {
				out = append(out, entry)
			}
		case oldestKept.IsZero() || !entry.FinishedAt.Before(oldestKept):
			// A failure record is history worth keeping for as long as the
			// backups around it are kept.
			out = append(out, entry)
		}
	}
	return out
}

// SetVerification records the outcome of a verification against one backup.
// Retention reads it from here (SPEC §7, invariant 2), so it has to survive a
// concurrent append the same way every other index write does.
func (s *Store) SetVerification(ctx context.Context, id, level string, ok bool, at time.Time) error {
	for range appendAttempts {
		current, err := s.Load(ctx)
		if err != nil {
			return err
		}

		found := false
		for i := range current.Entries {
			if current.Entries[i].ID != id {
				continue
			}
			verified := at.UTC()
			passed := ok
			current.Entries[i].VerifiedAt = &verified
			current.Entries[i].VerifyLevel = level
			current.Entries[i].VerifyOK = &passed
			found = true
		}
		if !found {
			// The backup exists — its manifest was just read — but the index
			// does not know it. Rebuilding is the caller's business.
			return fmt.Errorf("backup %s is not in the index; run `vaultd reindex %s`", id, s.layout.Target)
		}

		raw, err := marshal(current.Entries)
		if err != nil {
			return err
		}

		_, written, err := s.store.PutIfMatch(ctx, s.Key(), raw, current.ETag)
		if err != nil {
			return fmt.Errorf("updating the index: %w", err)
		}
		if written {
			return nil
		}
	}

	return fmt.Errorf("the index of %s changed under %d successive attempts; run `vaultd reindex` if this persists",
		s.layout.Target, appendAttempts)
}

// Rebuild reads every manifest under the target's prefix and returns what the
// index should contain. It is the answer to a lost, truncated or stale index —
// the bucket always knows.
func (s *Store) Rebuild(ctx context.Context) ([]manifest.Entry, error) {
	var keys []string
	for object, err := range s.store.List(ctx, s.layout.TargetPrefix()) {
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", s.layout.Target, err)
		}
		if manifest.IsManifestKey(object.Key) {
			keys = append(keys, object.Key)
		}
	}

	entries := make([]manifest.Entry, len(keys))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(rebuildFetchers)
	for i, key := range keys {
		g.Go(func() error {
			m, err := s.fetch(ctx, key)
			if err != nil {
				return err
			}
			entries[i] = manifest.NewEntry(m, key)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(a, b int) bool { return entries[a].FinishedAt.Before(entries[b].FinishedAt) })
	return entries, nil
}

// Entries returns the index contents, rebuilding from manifests when the index
// object is missing. The second result says which of the two happened, so a
// caller can suggest `vaultd reindex`.
func (s *Store) Entries(ctx context.Context) (entries []manifest.Entry, cached bool, err error) {
	current, err := s.Load(ctx)
	if err != nil {
		return nil, false, err
	}
	if current.ETag != "" {
		return current.Entries, true, nil
	}

	rebuilt, err := s.Rebuild(ctx)
	return rebuilt, false, err
}

// Manifest reads one backup's manifest from the bucket.
func (s *Store) Manifest(ctx context.Context, key string) (*manifest.Manifest, error) {
	return s.fetch(ctx, key)
}

func (s *Store) fetch(ctx context.Context, key string) (*manifest.Manifest, error) {
	body, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	m, err := manifest.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	return m, nil
}

func marshal(entries []manifest.Entry) ([]byte, error) {
	var raw []byte
	for _, entry := range entries {
		appended, err := manifest.AppendEntry(raw, entry)
		if err != nil {
			return nil, err
		}
		raw = appended
	}
	return raw, nil
}
