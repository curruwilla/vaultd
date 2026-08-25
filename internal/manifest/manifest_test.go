package manifest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
)

var at = time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)

func TestLayoutKeys(t *testing.T) {
	layout := manifest.Layout{Prefix: "prod", Target: "prod-pg"}

	assert.Equal(t,
		"prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.pgdump.zst.age",
		layout.Data(at, manifest.KindFull, ".pgdump.zst.age"))
	assert.Equal(t,
		"prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.manifest.json",
		layout.Manifest(at, manifest.KindFull))
	assert.Equal(t,
		"prod/prod-pg/2026/08/24/prod-pg-20260824T031500Z-globals.sql.zst.age",
		layout.Globals(at, ".sql.zst.age"))
	assert.Equal(t, "prod/_index/prod-pg.jsonl", layout.Index())
	assert.Equal(t, "prod/_locks/prod-pg.lock", layout.Lock())
	assert.Equal(t, "prod/prod-pg/", layout.TargetPrefix())
}

func TestLayoutWithoutPrefix(t *testing.T) {
	layout := manifest.Layout{Target: "prod-pg"}

	assert.Equal(t, "prod-pg/2026/08/24/prod-pg-20260824T031500Z-full.pgdump", layout.Data(at, manifest.KindFull, ".pgdump"))
	assert.Equal(t, "_locks/prod-pg.lock", layout.Lock())
}

// TestLayoutUsesUTC guards against a key that changes with the host timezone:
// two daemons in different zones must agree on where a backup lives.
func TestLayoutUsesUTC(t *testing.T) {
	zone := time.FixedZone("UTC+13", 13*3600)
	layout := manifest.Layout{Target: "t"}

	assert.Equal(t, layout.Data(at, manifest.KindFull, ""), layout.Data(at.In(zone), manifest.KindFull, ""))
}

func TestDataExtension(t *testing.T) {
	tests := []struct {
		engine      core.Engine
		compression string
		encryption  string
		want        string
	}{
		{core.EnginePostgres, "zstd:3", "age:x25519", ".pgdump.zst.age"},
		{core.EnginePostgres, "none", "none", ".pgdump"},
		{core.EngineMySQL, "gzip:6", "age:scrypt", ".sql.gz.age"},
		{core.EngineMariaDB, "zstd:1", "none", ".sql.zst"},
		{core.EngineMongoDB, "zstd:3", "age:x25519", ".archive.zst.age"},
	}

	for _, tt := range tests {
		t.Run(string(tt.engine)+" "+tt.compression+" "+tt.encryption, func(t *testing.T) {
			assert.Equal(t, tt.want, manifest.DataExtension(tt.engine, tt.compression, tt.encryption))
		})
	}
}

func TestIsManifestKey(t *testing.T) {
	assert.True(t, manifest.IsManifestKey("prod/t/2026/08/24/t-x-full.manifest.json"))
	assert.False(t, manifest.IsManifestKey("prod/t/2026/08/24/t-x-full.pgdump.zst.age"))
}

func TestMarshalRoundTrip(t *testing.T) {
	m := &manifest.Manifest{
		Schema:        manifest.Schema,
		ID:            manifest.NewID(at),
		Target:        "prod-pg",
		Engine:        core.EnginePostgres,
		ServerVersion: "17.2",
		StartedAt:     at,
		FinishedAt:    at.Add(84 * time.Second),
		DurationMS:    84000,
		Kind:          manifest.KindFull,
		Tier:          "daily",
		Object:        manifest.Object{Key: "k", Bytes: 4193282104, SHA256: "abc"},
		Plaintext:     manifest.Plaintext{Bytes: 19388211004, SHA256: "def"},
		Pipeline:      manifest.Pipeline{Compression: "zstd:3", Encryption: "age:x25519", Dumper: "pg_dump 17.2"},
		Consistency:   core.ConsistencySerializableSnapshot,
		Tables:        []core.TableInfo{{Name: "public.users", Rows: 1928372}},
		VaultdVersion: "0.1.0",
	}

	encoded, err := m.Marshal()
	require.NoError(t, err)

	decoded, err := manifest.Unmarshal(encoded)
	require.NoError(t, err)

	assert.Equal(t, m.ID, decoded.ID)
	assert.Equal(t, m.Object, decoded.Object)
	assert.Equal(t, m.Plaintext, decoded.Plaintext)
	assert.Equal(t, m.Tables, decoded.Tables)
	assert.True(t, m.FinishedAt.Equal(decoded.FinishedAt))
}

func TestUnmarshalRejectsUnknownSchema(t *testing.T) {
	_, err := manifest.Unmarshal([]byte(`{"schema": 99, "id": "x"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema 99 is not supported")
}

func TestNewIDIsSortableByTime(t *testing.T) {
	earlier := manifest.NewID(at)
	later := manifest.NewID(at.Add(time.Hour))

	assert.Less(t, earlier, later, "ULIDs must sort chronologically as strings")
}

func TestIndexAppendAndParse(t *testing.T) {
	m := &manifest.Manifest{
		Schema: manifest.Schema, ID: "01ABC", Target: "prod-pg", Kind: manifest.KindFull, Tier: "daily",
		FinishedAt: at, Object: manifest.Object{Key: "k", Bytes: 10, SHA256: "s"},
		Plaintext: manifest.Plaintext{Bytes: 100},
	}

	index, err := manifest.AppendEntry(nil, manifest.NewEntry(m, "mk"))
	require.NoError(t, err)

	m.ID = "01DEF"
	index, err = manifest.AppendEntry(index, manifest.NewEntry(m, "mk2"))
	require.NoError(t, err)

	entries, err := manifest.ParseIndex(index)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "01ABC", entries[0].ID)
	assert.Equal(t, "mk2", entries[1].ManifestKey)
	assert.Equal(t, int64(100), entries[1].PlaintextBytes)
}

func TestParseIndexReportsCorruption(t *testing.T) {
	_, err := manifest.ParseIndex([]byte("{\"id\":\"ok\"}\nnot json\n"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "index line 2 is corrupt")
}
