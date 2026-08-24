package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/config"
)

const inheritanceYAML = `
version: 1
defaults:
  compression: { algo: zstd, level: 9 }
  encryption: { mode: none }
  retention: { daily: { keep: 7 }, min_keep: 3 }
  timeout: 2h
  notify: [ops]
  on_overlap: fail
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret-value
notifiers:
  - name: ops
    type: webhook
    url: https://hooks.example.com/vaultd
    secret: hmac
    events: [backup.failed]
targets:
  - name: inherits
    engine: postgres
    dsn: postgres://backup@pg:5432/app
    destination: r2
    schedule: "0 3 * * *"
  - name: overrides
    engine: postgres
    dsn: postgres://backup@pg:5432/other
    destination: r2
    schedule: "0 4 * * *"
    compression: { algo: gzip }
    timeout: 30m
    retention: { daily: { keep: 2 }, min_keep: 1 }
`

func TestDefaultsInheritance(t *testing.T) {
	cfg, diags := parse(t, inheritanceYAML)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)

	inherits, ok := cfg.Target("inherits")
	require.True(t, ok)
	assert.Equal(t, config.CompressionZstd, inherits.Compression.Algo)
	assert.Equal(t, 9, inherits.Compression.Level)
	assert.Equal(t, 2*time.Hour, inherits.Timeout.Duration())
	assert.Equal(t, config.OverlapFail, inherits.OnOverlap)
	assert.Equal(t, []string{"ops"}, inherits.Notify)
	assert.Equal(t, 7, inherits.Retention.Daily.Keep)
}

func TestDefaultsOverride(t *testing.T) {
	cfg, diags := parse(t, inheritanceYAML)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)

	overrides, ok := cfg.Target("overrides")
	require.True(t, ok)
	assert.Equal(t, config.CompressionGzip, overrides.Compression.Algo)
	// The level falls back to the algorithm's own default, not to the zstd 9
	// set in defaults: 9 means something different to each codec.
	assert.Equal(t, 6, overrides.Compression.Level)
	assert.Equal(t, 30*time.Minute, overrides.Timeout.Duration())
	assert.Equal(t, 2, overrides.Retention.Daily.Keep)

	// Inheritance copies rather than aliases: an override must not reach back
	// into defaults or into a sibling target.
	inherits, _ := cfg.Target("inherits")
	assert.Equal(t, 7, inherits.Retention.Daily.Keep)
	assert.Equal(t, 7, cfg.Defaults.Retention.Daily.Keep)
	assert.Equal(t, config.CompressionZstd, cfg.Defaults.Compression.Algo)
}

func TestBuiltinDefaults(t *testing.T) {
	cfg, diags := parse(t, baseYAML)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)

	target := cfg.Targets[0]
	assert.Equal(t, config.CompressionZstd, target.Compression.Algo)
	assert.Equal(t, 3, target.Compression.Level)
	assert.Equal(t, 4*time.Hour, target.Timeout.Duration())
	assert.Equal(t, config.SpoolNone, target.Spool)
	assert.Equal(t, config.OverlapSkip, target.OnOverlap)
	// Decision D7: estimated row counts by default, exact only on request.
	assert.Equal(t, config.RowEstimateEstimate, target.RowEstimate)
}

func TestDurationRejectsGarbage(t *testing.T) {
	_, _, err := config.Parse([]byte(strings.Replace(baseYAML, "version: 1", "version: 1\ndefaults: { timeout: forever }", 1)), config.LoadOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid duration")
}

func TestWeekdayParsing(t *testing.T) {
	yaml := withTarget("retention: { weekly: { keep: 4, on: Sunday }, min_keep: 1 }")

	cfg, diags := parse(t, yaml)

	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
	assert.Equal(t, "sunday", cfg.Targets[0].Retention.Weekly.On.String())
}

func TestWeekdayRejectsGarbage(t *testing.T) {
	_, _, err := config.Parse([]byte(withTarget("retention: { weekly: { keep: 4, on: caturday } }")), config.LoadOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid weekday")
}
