package postgres

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient writes a stand-in binary that reports the given --version output.
func fakeClient(t *testing.T, dir, name, version string) {
	t.Helper()

	script := "#!/bin/sh\necho '" + version + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
}

// TestResolveBinaryRejectsAnOlderClient is the rule that keeps a restore from
// failing months later: pg_dump must be at least the server's major version
// (SPEC §3).
func TestResolveBinaryRejectsAnOlderClient(t *testing.T) {
	dir := t.TempDir()
	fakeClient(t, dir, "pg_dump", "pg_dump (PostgreSQL) 16.4")

	d := &Dumper{opts: Options{BinDir: dir}}

	_, err := d.resolveBinary(t.Context(), "pg_dump", 17)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server is PG 17, need pg_dump >= 17, found 16.4")
}

func TestResolveBinaryAcceptsTheSameMajor(t *testing.T) {
	dir := t.TempDir()
	fakeClient(t, dir, "pg_dump", "pg_dump (PostgreSQL) 17.2 (Debian 17.2-1.pgdg120+1)")

	d := &Dumper{opts: Options{BinDir: dir}}

	binary, err := d.resolveBinary(t.Context(), "pg_dump", 17)

	require.NoError(t, err)
	assert.Equal(t, "17.2", binary.Version)
	assert.Equal(t, 17, binary.Major)
	assert.Equal(t, "pg_dump 17.2", binary.String())
}

// TestResolveBinaryAcceptsANewerClient covers the other half of the rule: a
// newer client can always dump an older server.
func TestResolveBinaryAcceptsANewerClient(t *testing.T) {
	dir := t.TempDir()
	fakeClient(t, dir, "pg_dump", "pg_dump (PostgreSQL) 18.1")

	d := &Dumper{opts: Options{BinDir: dir}}

	binary, err := d.resolveBinary(t.Context(), "pg_dump", 16)

	require.NoError(t, err)
	assert.Equal(t, 18, binary.Major)
}

func TestResolveBinaryReportsAMissingClient(t *testing.T) {
	d := &Dumper{opts: Options{BinDir: t.TempDir()}}

	_, err := d.resolveBinary(t.Context(), "pg_dump", 17)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bin_dir")
}

func TestResolveBinaryRejectsANonExecutable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pg_dump"), []byte("not executable"), 0o644))

	d := &Dumper{opts: Options{BinDir: dir}}

	_, err := d.resolveBinary(t.Context(), "pg_dump", 17)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not executable")
}
