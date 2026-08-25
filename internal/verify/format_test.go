package verify_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/verify"
)

// feed streams a payload through a validator the way the verifier does, in
// small chunks, so a validator that only looked at one Write would be caught.
func feed(t *testing.T, engine core.Engine, payload []byte) error {
	t.Helper()

	validator := verify.ValidatorFor(engine)
	for _, chunk := range slice(payload, 64) {
		_, err := validator.Write(chunk)
		require.NoError(t, err)
	}
	return validator.Validate()
}

func slice(b []byte, size int) [][]byte {
	var out [][]byte
	for start := 0; start < len(b); start += size {
		out = append(out, b[start:min(start+size, len(b))])
	}
	return out
}

func mysqlDump() []byte {
	return []byte("-- MySQL dump 10.13  Distrib 8.0.46, for Linux (x86_64)\n" +
		strings.Repeat("INSERT INTO `users` VALUES (1,'a@example.com');\n", 200) +
		"-- Dump completed on 2026-08-24 22:32:50\n")
}

func TestValidatorsAcceptRealDumps(t *testing.T) {
	tests := map[core.Engine][]byte{
		core.EnginePostgres: append([]byte("PGDMP"), bytes.Repeat([]byte{0x01}, 500)...),
		core.EngineMySQL:    mysqlDump(),
		core.EngineMariaDB: []byte("-- MariaDB dump 10.19  Distrib 10.11.14-MariaDB\n" +
			"INSERT INTO `users` VALUES (1);\n-- Dump completed on 2026-08-24\n"),
		core.EngineMongoDB: append([]byte{0x6d, 0xe2, 0x99, 0x81}, bytes.Repeat([]byte{0x02}, 500)...),
	}

	for engine, payload := range tests {
		t.Run(string(engine), func(t *testing.T) {
			assert.NoError(t, feed(t, engine, payload))
		})
	}
}

func TestValidatorsRejectTheWrongFormat(t *testing.T) {
	tests := []struct {
		name    string
		engine  core.Engine
		payload []byte
		want    string
	}{
		{
			name:    "postgres, not an archive",
			engine:  core.EnginePostgres,
			payload: []byte("-- MySQL dump 10.13\n-- Dump completed\n"),
			want:    "does not start like a pg_dump custom-format archive",
		},
		{
			name:    "mongodb, not an archive",
			engine:  core.EngineMongoDB,
			payload: []byte("PGDMP and then some"),
			want:    "does not start like a mongodump archive",
		},
		{
			name:    "mysql, not a dump",
			engine:  core.EngineMySQL,
			payload: []byte("PGDMP and then some"),
			want:    "does not start like a mysqldump",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := feed(t, tt.engine, tt.payload)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// TestSQLDumpNeedsItsCompletionMarker: mysqldump writes that line last, so a
// stream that lost its tail cannot carry it. It is the cheapest truncation
// check there is for a text dump.
func TestSQLDumpNeedsItsCompletionMarker(t *testing.T) {
	full := mysqlDump()
	truncated := full[:len(full)-200]

	err := feed(t, core.EngineMySQL, truncated)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cut short")
}

func TestValidatorsRejectAnEmptyStream(t *testing.T) {
	for _, engine := range []core.Engine{core.EnginePostgres, core.EngineMySQL, core.EngineMongoDB, "sqlite"} {
		t.Run(string(engine), func(t *testing.T) {
			err := feed(t, engine, nil)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "empty")
		})
	}
}

// An engine this build has no format knowledge of still gets the checks that
// do not depend on the format.
func TestUnknownEngineAcceptsAnyNonEmptyStream(t *testing.T) {
	assert.NoError(t, feed(t, "sqlite", []byte("anything at all")))
}
