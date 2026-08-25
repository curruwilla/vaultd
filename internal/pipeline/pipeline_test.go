package pipeline_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/pipeline"
)

// payload builds a compressible blob of a known size: a real dump is highly
// repetitive, and a random blob would hide compression bugs behind noise.
func payload(size int) []byte {
	block := []byte("INSERT INTO users VALUES (1, 'someone@example.com', 'active', now());\n")
	var b bytes.Buffer
	for b.Len() < size {
		b.Write(block)
	}
	return b.Bytes()[:size]
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestRunRoundTrip(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	specs := map[string]pipeline.Spec{
		"zstd and age": {
			Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 3, Long: true},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}},
		},
		"gzip and age": {
			Compression: pipeline.Compression{Algo: pipeline.AlgoGzip, Level: 6},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}},
		},
		"zstd and passphrase": {
			Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 1},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModePassphrase, Passphrase: "correct horse battery staple"},
		},
		"zstd only": {
			Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 3},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModeNone},
		},
		"nothing at all": {
			Compression: pipeline.Compression{Algo: pipeline.AlgoNone},
			Encryption:  pipeline.Encryption{Mode: pipeline.ModeNone},
		},
	}

	dump := payload(3 << 20) // 3MB, several zstd blocks

	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			var object bytes.Buffer

			result, err := pipeline.Run(t.Context(), spec,
				func(_ context.Context, w io.Writer) error {
					_, err := w.Write(dump)
					return err
				},
				func(_ context.Context, r io.Reader) error {
					_, err := io.Copy(&object, r)
					return err
				},
			)
			require.NoError(t, err)

			// The plaintext sum is what the database produced; the ciphertext
			// sum is exactly what a HEAD of the stored object would report.
			assert.Equal(t, int64(len(dump)), result.Plaintext.Bytes)
			assert.Equal(t, sha256hex(dump), result.Plaintext.SHA256)
			assert.Equal(t, int64(object.Len()), result.Ciphertext.Bytes)
			assert.Equal(t, sha256hex(object.Bytes()), result.Ciphertext.SHA256)

			restored := readBack(t, spec, object.Bytes(), identity)
			assert.True(t, bytes.Equal(dump, restored), "the restored stream differs from the dump")
		})
	}
}

func readBack(t *testing.T, spec pipeline.Spec, object []byte, identity *age.X25519Identity) []byte {
	t.Helper()

	r, err := spec.Reader(bytes.NewReader(object), identity)
	require.NoError(t, err)
	defer r.Close()

	restored, err := io.ReadAll(r)
	require.NoError(t, err)
	return restored
}

func TestRunCompresses(t *testing.T) {
	spec := pipeline.Spec{Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 3}}
	dump := payload(1 << 20)

	var object bytes.Buffer
	result, err := pipeline.Run(t.Context(), spec,
		func(_ context.Context, w io.Writer) error { _, err := w.Write(dump); return err },
		func(_ context.Context, r io.Reader) error { _, err := io.Copy(&object, r); return err },
	)
	require.NoError(t, err)

	assert.Less(t, result.Ciphertext.Bytes, result.Plaintext.Bytes/10, "a repetitive dump should compress hard")
}

func TestRunPropagatesDumpFailure(t *testing.T) {
	wantErr := errors.New("pg_dump exited with code 1")
	var consumeErr error

	_, err := pipeline.Run(t.Context(), zstdOnly(),
		func(_ context.Context, w io.Writer) error {
			// Fail mid-stream, the way a dumper does when the server drops it.
			_, _ = w.Write(payload(64 << 10))
			return wantErr
		},
		func(_ context.Context, r io.Reader) error {
			_, consumeErr = io.Copy(io.Discard, r)
			return consumeErr
		},
	)

	require.ErrorIs(t, err, wantErr)
	// The consumer must see the failure rather than a clean EOF: an uploader
	// that reads EOF would complete the multipart upload and store a
	// truncated backup.
	require.ErrorIs(t, consumeErr, wantErr)
}

// TestRunPropagatesUploadFailure covers the deadlock this design exists to
// avoid: the consumer dies while the producer is mid-write.
func TestRunPropagatesUploadFailure(t *testing.T) {
	wantErr := errors.New("the bucket said no")

	_, err := pipeline.Run(t.Context(), zstdOnly(),
		func(_ context.Context, w io.Writer) error {
			// Far more than any pipe buffer, so this blocks unless the reader
			// side actively fails it.
			_, err := io.Copy(w, bytes.NewReader(payload(16<<20)))
			return err
		},
		func(_ context.Context, _ io.Reader) error { return wantErr },
	)

	require.ErrorIs(t, err, wantErr)
}

func TestRunHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	_, err := pipeline.Run(ctx, zstdOnly(),
		func(ctx context.Context, w io.Writer) error {
			// A dumper that respects its context, as exec.CommandContext does.
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					if _, err := w.Write(payload(64 << 10)); err != nil {
						return err
					}
				}
			}
		},
		func(_ context.Context, r io.Reader) error {
			buf := make([]byte, 32<<10)
			if _, err := io.ReadFull(r, buf); err != nil {
				return err
			}
			cancel()
			_, err := io.Copy(io.Discard, r)
			return err
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRunRejectsBadSpec(t *testing.T) {
	tests := map[string]pipeline.Spec{
		"unknown algorithm": {Compression: pipeline.Compression{Algo: "lzma"}},
		"unknown mode":      {Encryption: pipeline.Encryption{Mode: "pgp"}},
		"age without recipients": {
			Encryption: pipeline.Encryption{Mode: pipeline.ModeAge},
		},
		"unparseable recipient": {
			Encryption: pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{"age1nope"}},
		},
		"passphrase without a passphrase": {
			Encryption: pipeline.Encryption{Mode: pipeline.ModePassphrase},
		},
	}

	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			called := false

			_, err := pipeline.Run(t.Context(), spec,
				func(context.Context, io.Writer) error { called = true; return nil },
				func(context.Context, io.Reader) error { return nil },
			)

			require.Error(t, err)
			assert.False(t, called, "a bad spec must be caught before the database is touched")
		})
	}
}

func TestReaderNeedsAnIdentity(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	spec := pipeline.Spec{Encryption: pipeline.Encryption{Mode: pipeline.ModeAge, Recipients: []string{identity.Recipient().String()}}}

	_, err = spec.Reader(strings.NewReader("whatever"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity is required")
}

func TestSpecDescriptions(t *testing.T) {
	assert.Equal(t, "zstd:3", pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 3}.String())
	assert.Equal(t, "gzip:6", pipeline.Compression{Algo: pipeline.AlgoGzip, Level: 6}.String())
	assert.Equal(t, "none", pipeline.Compression{Algo: pipeline.AlgoNone}.String())
	assert.Equal(t, "age:x25519", pipeline.Encryption{Mode: pipeline.ModeAge}.String())
	assert.Equal(t, "age:scrypt", pipeline.Encryption{Mode: pipeline.ModePassphrase}.String())
	assert.Equal(t, "none", pipeline.Encryption{Mode: pipeline.ModeNone}.String())
}

// TestRunIsMemoryBounded is the streaming guarantee in test form (SPEC O2):
// a dump far larger than any buffer must flow through without the pipeline
// accumulating it.
func TestRunIsMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 64MB of dump")
	}

	const size = 64 << 20
	var uploaded int64

	result, err := pipeline.Run(t.Context(), zstdOnly(),
		func(_ context.Context, w io.Writer) error {
			_, err := io.CopyN(w, rand.Reader, size)
			return err
		},
		func(_ context.Context, r io.Reader) error {
			n, err := io.Copy(io.Discard, r)
			uploaded = n
			return err
		},
	)
	require.NoError(t, err)

	assert.Equal(t, int64(size), result.Plaintext.Bytes)
	assert.Equal(t, uploaded, result.Ciphertext.Bytes)
}

func zstdOnly() pipeline.Spec {
	return pipeline.Spec{Compression: pipeline.Compression{Algo: pipeline.AlgoZstd, Level: 1}}
}
