package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// Sum is the size and SHA-256 of a stream.
type Sum struct {
	Bytes  int64
	SHA256 string
}

// summer counts and hashes everything written through it. It is an io.Writer
// so it can sit in a MultiWriter or a TeeReader without buffering anything.
type summer struct {
	hash  hash.Hash
	bytes int64
}

func newSum() *summer { return &summer{hash: sha256.New()} }

func (s *summer) Write(p []byte) (int, error) {
	n, err := s.hash.Write(p)
	s.bytes += int64(n)
	return n, err
}

func (s *summer) Sum() Sum {
	return Sum{Bytes: s.bytes, SHA256: hex.EncodeToString(s.hash.Sum(nil))}
}

var _ io.Writer = (*summer)(nil)
