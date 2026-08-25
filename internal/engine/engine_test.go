package engine_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/engine"
)

func TestTailKeepsTheEnd(t *testing.T) {
	tail := engine.NewTail(32)

	_, err := tail.Write([]byte(strings.Repeat("noise\n", 100)))
	require.NoError(t, err)
	_, err = tail.Write([]byte("pg_dump: error: permission denied\n"))
	require.NoError(t, err)

	got := tail.String()
	assert.Contains(t, got, "permission denied")
	assert.LessOrEqual(t, len(got), 40, "the tail must stay bounded")
	assert.True(t, strings.HasPrefix(got, "…"), "a truncated tail should say so")
}

func TestTailBelowTheLimitIsVerbatim(t *testing.T) {
	tail := engine.NewTail(1024)

	_, err := tail.Write([]byte("one\ntwo\n"))
	require.NoError(t, err)

	assert.Equal(t, "one\ntwo", tail.String())
}

func TestLastLines(t *testing.T) {
	out := engine.LastLines("a\n\nb\nc\nd\n", 2)

	assert.Equal(t, "c\nd", out)
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		output      string
		wantVersion string
		wantMajor   int
	}{
		{"pg_dump (PostgreSQL) 17.2 (Debian 17.2-1.pgdg120+1)\n", "17.2", 17},
		{"pg_dump (PostgreSQL) 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1)\n", "16.15", 16},
		{"mysqldump  Ver 8.0.39 for Linux on x86_64 (MySQL Community Server)\n", "8.0.39", 8},
		{"mongodump version: 100.9.4\n", "100.9.4", 100},
	}

	for _, tt := range tests {
		t.Run(tt.wantVersion, func(t *testing.T) {
			version, major, err := engine.ParseVersion(tt.output)

			require.NoError(t, err)
			assert.Equal(t, tt.wantVersion, version)
			assert.Equal(t, tt.wantMajor, major)
		})
	}
}

func TestParseVersionRejectsNonsense(t *testing.T) {
	_, _, err := engine.ParseVersion("command not found")

	require.Error(t, err)
}

// TestEnvIsMinimal is a safety property: a stray PGHOST on the host must not
// be able to redirect a backup to a different server.
func TestEnvIsMinimal(t *testing.T) {
	t.Setenv("PGHOST", "wrong-host.internal")
	t.Setenv("PGPASSWORD", "leaked")

	env := engine.Env(map[string]string{"PGHOST": "right-host.internal", "PGUSER": "backup"})

	assert.Contains(t, env, "PGHOST=right-host.internal")
	assert.Contains(t, env, "PGUSER=backup")
	assert.NotContains(t, env, "PGHOST=wrong-host.internal")
	assert.NotContains(t, env, "PGPASSWORD=leaked")
	assert.Contains(t, env, "LC_ALL=C")
}

func TestEnvSkipsEmptyValues(t *testing.T) {
	env := engine.Env(map[string]string{"PGPASSWORD": "", "PGUSER": "backup"})

	for _, entry := range env {
		assert.NotEqual(t, "PGPASSWORD=", entry)
	}
}

func TestExitErrorCarriesStderr(t *testing.T) {
	err := &engine.ExitError{
		Binary: "pg_dump",
		Code:   1,
		Stderr: "pg_dump: warning: something\npg_dump: error: permission denied for table users",
	}

	assert.Contains(t, err.Error(), "pg_dump exited with code 1")
	assert.Contains(t, err.Error(), "permission denied for table users")
}
