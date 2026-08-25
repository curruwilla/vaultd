package pipeline

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Memory budget. The whole point of streaming is a constant footprint
// (SPEC O2: under 256MB for a database of any size), so the two stages that
// can allocate a lot — the compressor's window and the uploader's part
// buffers — are bounded here and in the uploader rather than left to defaults
// that scale with the machine.
const (
	zstdConcurrency  = 2
	zstdLongWindow   = 1 << 24 // 16MB long-distance matching window
	defaultZstdLevel = 3
	defaultGzipLevel = gzip.DefaultCompression
)

// Algo is a compression algorithm.
type Algo string

const (
	AlgoZstd Algo = "zstd"
	AlgoGzip Algo = "gzip"
	AlgoNone Algo = "none"
)

// Mode is an encryption mode.
type Mode string

const (
	ModeAge        Mode = "age"
	ModePassphrase Mode = "passphrase"
	ModeNone       Mode = "none"
)

// Spec is the transformation a backup goes through. The order is fixed:
// compress, then encrypt. The other way round does not compress — ciphertext
// has no redundancy left to exploit.
type Spec struct {
	Compression Compression
	Encryption  Encryption
}

// Compression configures the first stage.
type Compression struct {
	Algo  Algo
	Level int
	// Long enables long-distance matching, which helps on dumps with repeated
	// schema blocks, at the cost of a larger window in memory.
	Long bool
}

// Encryption configures the second stage.
type Encryption struct {
	Mode Mode
	// Recipients are age X25519 public keys. The matching private keys live
	// off the backup host by design (SPEC §15).
	Recipients []string
	Passphrase string
}

// String renders the compression stage for a manifest: "zstd:3", "none".
func (c Compression) String() string {
	if c.Algo == AlgoNone || c.Algo == "" {
		return string(AlgoNone)
	}
	return string(c.Algo) + ":" + strconv.Itoa(c.Level)
}

// String renders the encryption stage for a manifest: "age:x25519".
func (e Encryption) String() string {
	switch e.Mode {
	case ModeAge:
		return "age:x25519"
	case ModePassphrase:
		return "age:scrypt"
	default:
		return string(ModeNone)
	}
}

// Validate rejects a spec that cannot produce a readable object. The config
// layer checks the same things earlier; the pipeline checks them again because
// it is also driven by tests and, later, by the verify path.
func (s Spec) Validate() error {
	switch s.Compression.Algo {
	case AlgoZstd, AlgoGzip, AlgoNone, "":
	default:
		return fmt.Errorf("unknown compression algorithm %q", s.Compression.Algo)
	}

	switch s.Encryption.Mode {
	case ModeNone, "":
	case ModeAge:
		if len(s.Encryption.Recipients) == 0 {
			return errors.New("age encryption needs at least one recipient")
		}
		if _, err := s.recipients(); err != nil {
			return err
		}
	case ModePassphrase:
		if s.Encryption.Passphrase == "" {
			return errors.New("passphrase encryption needs a passphrase")
		}
	default:
		return fmt.Errorf("unknown encryption mode %q", s.Encryption.Mode)
	}
	return nil
}

// writer builds the stage chain that turns plaintext into the object bytes.
// The returned writer is the plaintext end; closeStages finalizes every stage
// in order, which is what actually writes the zstd frame footer and the age
// payload terminator.
func (s Spec) writer(dst io.Writer) (io.Writer, func() error, error) {
	var closers []io.Closer

	closeAll := func() error {
		// Innermost first: the compressor must flush into the encryptor before
		// the encryptor can finalize.
		for _, c := range closers {
			if err := c.Close(); err != nil {
				return fmt.Errorf("finalizing the backup stream: %w", err)
			}
		}
		return nil
	}

	encrypted, err := s.encryptWriter(dst)
	if err != nil {
		return nil, nil, err
	}
	if c, ok := encrypted.(io.Closer); ok && encrypted != dst {
		closers = append(closers, c)
	}

	compressed, err := s.compressWriter(encrypted)
	if err != nil {
		return nil, nil, err
	}
	if c, ok := compressed.(io.Closer); ok && compressed != encrypted {
		closers = append([]io.Closer{c}, closers...)
	}

	return compressed, closeAll, nil
}

func (s Spec) encryptWriter(dst io.Writer) (io.Writer, error) {
	switch s.Encryption.Mode {
	case ModeNone, "":
		return dst, nil

	case ModeAge:
		recipients, err := s.recipients()
		if err != nil {
			return nil, err
		}
		w, err := age.Encrypt(dst, recipients...)
		if err != nil {
			return nil, fmt.Errorf("starting age encryption: %w", err)
		}
		return w, nil

	case ModePassphrase:
		recipient, err := age.NewScryptRecipient(s.Encryption.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("preparing passphrase encryption: %w", err)
		}
		w, err := age.Encrypt(dst, recipient)
		if err != nil {
			return nil, fmt.Errorf("starting age encryption: %w", err)
		}
		return w, nil

	default:
		return nil, fmt.Errorf("unknown encryption mode %q", s.Encryption.Mode)
	}
}

func (s Spec) compressWriter(dst io.Writer) (io.Writer, error) {
	switch s.Compression.Algo {
	case AlgoNone, "":
		return dst, nil

	case AlgoZstd:
		level := s.Compression.Level
		if level == 0 {
			level = defaultZstdLevel
		}
		opts := []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
			zstd.WithEncoderConcurrency(zstdConcurrency),
		}
		if s.Compression.Long {
			opts = append(opts, zstd.WithWindowSize(zstdLongWindow))
		}
		w, err := zstd.NewWriter(dst, opts...)
		if err != nil {
			return nil, fmt.Errorf("starting zstd compression: %w", err)
		}
		return w, nil

	case AlgoGzip:
		level := s.Compression.Level
		if level == 0 {
			level = defaultGzipLevel
		}
		w, err := gzip.NewWriterLevel(dst, level)
		if err != nil {
			return nil, fmt.Errorf("starting gzip compression: %w", err)
		}
		return w, nil

	default:
		return nil, fmt.Errorf("unknown compression algorithm %q", s.Compression.Algo)
	}
}

// Reader reverses the pipeline: it decrypts and decompresses an object read
// back from the bucket. Verification and restore both read through it, which
// is what makes a round trip testable end to end.
func (s Spec) Reader(r io.Reader, identities ...age.Identity) (io.ReadCloser, error) {
	decrypted, err := s.decryptReader(r, identities)
	if err != nil {
		return nil, err
	}
	return s.decompressReader(decrypted)
}

func (s Spec) decryptReader(r io.Reader, identities []age.Identity) (io.Reader, error) {
	switch s.Encryption.Mode {
	case ModeNone, "":
		return r, nil

	case ModeAge:
		if len(identities) == 0 {
			return nil, errors.New("this backup is age-encrypted; the matching identity is required to read it")
		}
		decrypted, err := age.Decrypt(r, identities...)
		if err != nil {
			return nil, fmt.Errorf("decrypting: %w", err)
		}
		return decrypted, nil

	case ModePassphrase:
		identity, err := age.NewScryptIdentity(s.Encryption.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("preparing passphrase decryption: %w", err)
		}
		decrypted, err := age.Decrypt(r, identity)
		if err != nil {
			return nil, fmt.Errorf("decrypting: %w", err)
		}
		return decrypted, nil

	default:
		return nil, fmt.Errorf("unknown encryption mode %q", s.Encryption.Mode)
	}
}

func (s Spec) decompressReader(r io.Reader) (io.ReadCloser, error) {
	switch s.Compression.Algo {
	case AlgoNone, "":
		return io.NopCloser(r), nil

	case AlgoZstd:
		decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(zstdConcurrency))
		if err != nil {
			return nil, fmt.Errorf("reading zstd stream: %w", err)
		}
		return decoder.IOReadCloser(), nil

	case AlgoGzip:
		decoder, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("reading gzip stream: %w", err)
		}
		return decoder, nil

	default:
		return nil, fmt.Errorf("unknown compression algorithm %q", s.Compression.Algo)
	}
}

func (s Spec) recipients() ([]age.Recipient, error) {
	out := make([]age.Recipient, 0, len(s.Encryption.Recipients))
	for _, raw := range s.Encryption.Recipients {
		recipient, err := age.ParseX25519Recipient(raw)
		if err != nil {
			return nil, fmt.Errorf("age recipient %q: %w", raw, err)
		}
		out = append(out, recipient)
	}
	return out, nil
}
