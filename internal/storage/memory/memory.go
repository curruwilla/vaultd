// Package memory is an in-memory core.Store. It exists so the backup
// orchestration, the manifest layer and the CLI can be tested end to end
// without a bucket; the S3 adapter is covered separately against MinIO.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// Store keeps objects in a map. It is safe for concurrent use, because the
// pipeline uploads while the dumper is still writing.
type Store struct {
	mu      sync.RWMutex
	objects map[string]object
	// Now is the clock stamped onto stored objects; tests override it.
	Now func() time.Time
	// FailPut, when set, makes every Put fail with this error — the upload
	// half of a failed backup.
	FailPut error
}

type object struct {
	data     []byte
	etag     string
	modified time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{objects: map[string]object{}, Now: time.Now}
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, _ core.PutOptions) (core.ObjectInfo, error) {
	if s.FailPut != nil {
		return core.ObjectInfo{}, s.FailPut
	}
	if err := ctx.Err(); err != nil {
		return core.ObjectInfo{}, err
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return core.ObjectInfo{}, err
	}
	return s.write(key, buf.Bytes()), nil
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.objects[key]
	if !ok {
		return nil, notFound(key)
	}
	return io.NopCloser(bytes.NewReader(obj.data)), nil
}

func (s *Store) Head(_ context.Context, key string) (core.ObjectInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.objects[key]
	if !ok {
		return core.ObjectInfo{}, notFound(key)
	}
	return core.ObjectInfo{Key: key, Bytes: int64(len(obj.data)), ETag: obj.etag, LastModified: obj.modified}, nil
}

func (s *Store) List(_ context.Context, prefix string) iter.Seq2[core.ObjectInfo, error] {
	s.mu.RLock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	infos := make([]core.ObjectInfo, 0, len(keys))
	sort.Strings(keys)
	for _, key := range keys {
		obj := s.objects[key]
		infos = append(infos, core.ObjectInfo{Key: key, Bytes: int64(len(obj.data)), ETag: obj.etag, LastModified: obj.modified})
	}
	s.mu.RUnlock()

	return func(yield func(core.ObjectInfo, error) bool) {
		for _, info := range infos {
			if !yield(info, nil) {
				return
			}
		}
	}
}

func (s *Store) Delete(_ context.Context, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, key := range keys {
		delete(s.objects, key)
	}
	return nil
}

func (s *Store) PutIfAbsent(_ context.Context, key string, b []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = object{data: bytes.Clone(b), etag: etag(b), modified: s.Now()}
	return true, nil
}

// Objects returns a snapshot of every stored key. Tests assert on it.
func (s *Store) Objects() map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string][]byte, len(s.objects))
	for key, obj := range s.objects {
		out[key] = bytes.Clone(obj.data)
	}
	return out
}

func (s *Store) write(key string, data []byte) core.ObjectInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	obj := object{data: data, etag: etag(data), modified: s.Now()}
	s.objects[key] = obj
	return core.ObjectInfo{Key: key, Bytes: int64(len(data)), ETag: obj.etag, LastModified: obj.modified}
}

func etag(b []byte) string { return fmt.Sprintf("%x", len(b)) }

func notFound(key string) error { return fmt.Errorf("object %q: %w", key, core.ErrNotFound) }

var _ core.Store = (*Store)(nil)
