package verify

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/curruwilla/vaultd/internal/core"
)

// headBytes and tailBytes are how much of the plaintext a validator keeps to
// judge it. The stream itself is never held: these are two small windows.
const (
	headBytes = 512
	tailBytes = 4 << 10
)

// Validator inspects a plaintext dump as it streams past and says whether it
// looks like what the engine should have produced. It is deliberately cheap:
// the strong check is the plaintext checksum, and this catches the case where
// the bytes are intact but they are not a dump at all — a truncated stream, a
// wrong key that somehow decrypted, an object overwritten by something else.
type Validator interface {
	// Write consumes the stream. A Validator never buffers all of it.
	Write(p []byte) (int, error)
	// Validate is called at the end of the stream.
	Validate() error
	// Describe names what the validator was looking for, for the report.
	Describe() string
}

// window keeps the first headBytes and the last tailBytes of a stream.
type window struct {
	head  []byte
	tail  []byte
	total int64
}

func (w *window) Write(p []byte) (int, error) {
	if len(w.head) < headBytes {
		w.head = append(w.head, p[:min(len(p), headBytes-len(w.head))]...)
	}

	w.tail = append(w.tail, p...)
	if len(w.tail) > tailBytes {
		w.tail = w.tail[len(w.tail)-tailBytes:]
	}

	w.total += int64(len(p))
	return len(p), nil
}

// ValidatorFor returns the validator of an engine's dump format.
func ValidatorFor(engine core.Engine) Validator {
	switch engine {
	case core.EnginePostgres:
		return &magicValidator{
			magic: []byte("PGDMP"),
			what:  "a pg_dump custom-format archive",
		}
	case core.EngineMongoDB:
		return &magicValidator{
			// The mongodump archive magic, little-endian 0x8199e26d.
			magic: []byte{0x6d, 0xe2, 0x99, 0x81},
			what:  "a mongodump archive",
		}
	case core.EngineMySQL, core.EngineMariaDB:
		return &sqlDumpValidator{}
	default:
		return &anyValidator{}
	}
}

// magicValidator checks a format that announces itself in its first bytes.
type magicValidator struct {
	window
	magic []byte
	what  string
}

func (m *magicValidator) Validate() error {
	if m.total == 0 {
		return errors.New("the backup is empty")
	}
	if !bytes.HasPrefix(m.head, m.magic) {
		return fmt.Errorf("the decrypted stream does not start like %s", m.what)
	}
	return nil
}

func (m *magicValidator) Describe() string { return m.what }

// sqlDumpValidator checks a text dump from mysqldump or mariadb-dump. The
// trailing "Dump completed" line is the useful half: it is written last, so a
// stream that lost its end cannot have it.
type sqlDumpValidator struct {
	window
}

func (s *sqlDumpValidator) Validate() error {
	if s.total == 0 {
		return errors.New("the backup is empty")
	}

	head := string(s.head)
	if !strings.Contains(head, "MySQL dump") && !strings.Contains(head, "MariaDB dump") {
		return errors.New("the decrypted stream does not start like a mysqldump or mariadb-dump file")
	}
	if !strings.Contains(string(s.tail), "Dump completed") {
		return errors.New("the dump has no completion marker, so it was cut short")
	}
	return nil
}

func (s *sqlDumpValidator) Describe() string { return "a mysqldump or mariadb-dump file" }

// anyValidator accepts any non-empty stream, for an engine this build has no
// format knowledge of.
type anyValidator struct {
	window
}

func (a *anyValidator) Validate() error {
	if a.total == 0 {
		return errors.New("the backup is empty")
	}
	return nil
}

func (a *anyValidator) Describe() string { return "a non-empty dump" }
