package backup

import (
	"bytes"
	"io"
)

// newReader wraps a small in-memory document — a manifest — as a reader for
// the store. Backup streams never go through this.
func newReader(b []byte) io.Reader { return bytes.NewReader(b) }
