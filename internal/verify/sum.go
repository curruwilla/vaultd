package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// sum is the size and checksum of a stream that went past.
type sum struct {
	Bytes  int64
	SHA256 string
}

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

func (s *summer) Sum() sum {
	return sum{Bytes: s.bytes, SHA256: hex.EncodeToString(s.hash.Sum(nil))}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
