//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/storage/s3"
)

// One MinIO serves the whole package: starting a container per test would
// dominate the runtime, and each test gets its own bucket anyway.
var minioServer struct {
	endpoint string
	user     string
	password string
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcminio.Run(ctx, "minio/minio:RELEASE.2025-04-22T22-12-26Z")
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting MinIO:", err)
		os.Exit(1)
	}

	endpoint, err := container.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading the MinIO endpoint:", err)
		os.Exit(1)
	}
	minioServer.endpoint = "http://" + endpoint
	minioServer.user = container.Username
	minioServer.password = container.Password

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintln(os.Stderr, "terminating MinIO:", err)
	}
	os.Exit(code)
}

// newStore returns a Store pointed at a fresh bucket. MinIO speaks the same
// protocol as S3 and R2, so the upload path — multipart and conditional writes
// included — is exercised for real.
func newStore(t *testing.T) *s3.Store {
	t.Helper()

	store, err := s3.New(t.Context(), s3.Config{
		Provider:        s3.ProviderMinIO,
		Bucket:          bucketName(t),
		Endpoint:        minioServer.endpoint,
		Region:          "us-east-1",
		AccessKeyID:     minioServer.user,
		SecretAccessKey: minioServer.password,
	})
	require.NoError(t, err)

	require.NoError(t, store.CreateBucket(t.Context()))
	return store
}

// bucketName derives a legal, unique bucket name from the test name.
func bucketName(t *testing.T) string {
	name := strings.ToLower(t.Name())
	name = strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(name)
	return "vaultd-" + name
}

func TestPutGetHead(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	payload := bytes.Repeat([]byte("vaultd"), 1024)

	info, err := store.Put(ctx, "small/object.bin", bytes.NewReader(payload), core.PutOptions{ContentType: "application/octet-stream"})
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), info.Bytes)

	head, err := store.Head(ctx, "small/object.bin")
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), head.Bytes)
	assert.NotEmpty(t, head.ETag)

	body, err := store.Get(ctx, "small/object.bin")
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(payload, got))
}

// TestPutMultipart drives the adaptive multipart path: more data than one
// part, from a reader whose length is unknown in advance — exactly what a
// streaming dump looks like.
func TestPutMultipart(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	const size = 21 << 20 // three 8MB parts, the last one short
	payload := make([]byte, size)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	info, err := store.Put(ctx, "big/object.bin", unknownLength(bytes.NewReader(payload)), core.PutOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(size), info.Bytes)

	body, err := store.Get(ctx, "big/object.bin")
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Len(t, got, size)
	assert.True(t, bytes.Equal(payload, got), "the reassembled object differs from what was uploaded")
}

// TestPutAbortsOnStreamFailure covers the guarantee that a failed backup does
// not leave a half-uploaded object behind.
func TestPutAbortsOnStreamFailure(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	failing := io.MultiReader(
		bytes.NewReader(make([]byte, 12<<20)),
		errorReader{errors.New("the dump died mid-stream")},
	)

	_, err := store.Put(ctx, "aborted/object.bin", failing, core.PutOptions{})
	require.Error(t, err)

	_, err = store.Head(ctx, "aborted/object.bin")
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestListAndDelete(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	keys := []string{"prod/t/a", "prod/t/b", "prod/other/c"}
	for _, key := range keys {
		_, err := store.Put(ctx, key, bytes.NewReader([]byte(key)), core.PutOptions{})
		require.NoError(t, err)
	}

	var listed []string
	for object, err := range store.List(ctx, "prod/t/") {
		require.NoError(t, err)
		listed = append(listed, object.Key)
	}
	assert.Equal(t, []string{"prod/t/a", "prod/t/b"}, listed)

	require.NoError(t, store.Delete(ctx, []string{"prod/t/a", "prod/t/b"}))

	_, err := store.Head(ctx, "prod/t/a")
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestGetMissingObject(t *testing.T) {
	store := newStore(t)

	_, err := store.Get(t.Context(), "nothing/here")

	assert.ErrorIs(t, err, core.ErrNotFound)
}

// TestPutIfAbsent is the lock primitive: exactly one caller may create a key.
func TestPutIfAbsent(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	created, err := store.PutIfAbsent(ctx, "_locks/prod-pg.lock", []byte(`{"holder":"one"}`))
	require.NoError(t, err)
	assert.True(t, created)

	created, err = store.PutIfAbsent(ctx, "_locks/prod-pg.lock", []byte(`{"holder":"two"}`))
	require.NoError(t, err)
	assert.False(t, created, "a second holder must not take a lock that exists")

	body, err := store.Get(ctx, "_locks/prod-pg.lock")
	require.NoError(t, err)
	defer body.Close()

	held, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"holder":"one"}`, string(held))
}

// unknownLength hides a reader's Len method, so the uploader cannot shortcut
// its sizing decisions the way it could with a *bytes.Reader.
func unknownLength(r io.Reader) io.Reader { return struct{ io.Reader }{r} }

type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }
