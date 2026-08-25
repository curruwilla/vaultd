package app_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/retention"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

const configYAML = `
version: 1
defaults:
  compression: { algo: zstd, level: 7, long: true }
  encryption:  { mode: age, recipients: ["age1t73geuxnfxeam4a8cafwz2nqpma5wjd0uhz4pm4rmvzrv4dhy43q6vc078"] }
  timeout: 90m
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret
    prefix: prod
    storage_class: GLACIER
targets:
  - name: prod-pg
    engine: postgres
    dsn: postgres://backup@pg:5432/app
    destination: r2
    schedule: "0 3 * * *"
    options:
      include_globals: true
      exclude_table_data: ["public.sessions"]
  - name: prod-mysql
    engine: mysql
    dsn: mysql://backup@mysql:3306/app
    destination: r2
    schedule: "0 4 * * *"
    options: { on_non_innodb: fail }
  - name: prod-mariadb
    engine: mariadb
    dsn: mariadb://backup@mariadb:3306/app
    destination: r2
    schedule: "0 5 * * *"
  - name: prod-mongo
    engine: mongodb
    uri: mongodb://backup@mongo:27017/app?replicaSet=rs0
    destination: r2
    schedule: "0 6 * * *"
    options: { oplog: true }
`

func load(t *testing.T) *config.Config {
	t.Helper()

	cfg, diags, err := config.Parse([]byte(configYAML), config.LoadOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "%v", diags)
	return cfg
}

func TestBackupSpecMapsTheTarget(t *testing.T) {
	cfg := load(t)
	application := app.New(cfg, nil)

	target, ok := cfg.Target("prod-pg")
	require.True(t, ok)

	spec, err := application.BackupSpec(target, "weekly")
	require.NoError(t, err)

	assert.Equal(t, "prod-pg", spec.Target)
	assert.Equal(t, "weekly", spec.Tier)
	assert.Equal(t, 90*time.Minute, spec.Timeout)
	assert.Equal(t, "zstd:7", spec.Pipeline.Compression.String())
	assert.True(t, spec.Pipeline.Compression.Long)
	assert.Equal(t, "age:x25519", spec.Pipeline.Encryption.String())
	// The destination prefix, not the target name, decides where keys start.
	assert.Equal(t, "prod/prod-pg/", spec.Layout.TargetPrefix())
}

func TestBackupSpecRejectsDiskSpooling(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")
	target.Spool = config.SpoolDisk

	_, err := app.New(cfg, nil).BackupSpec(target, "daily")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented yet")
}

func TestDumperIsBuiltForPostgres(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")

	dumper, err := app.New(cfg, nil).Dumper(target)

	require.NoError(t, err)
	assert.NotNil(t, dumper)
}

func TestDumperIsBuiltForEveryEngine(t *testing.T) {
	cfg := load(t)
	application := app.New(cfg, nil)

	for _, name := range []string{"prod-pg", "prod-mysql", "prod-mariadb", "prod-mongo"} {
		t.Run(name, func(t *testing.T) {
			target, ok := cfg.Target(name)
			require.True(t, ok)

			dumper, err := application.Dumper(target)

			require.NoError(t, err)
			assert.NotNil(t, dumper)
		})
	}
}

func TestDumperRejectsAnUnknownEngine(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")
	target.Engine = "cassandra"

	_, err := app.New(cfg, nil).Dumper(target)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown engine")
}

func TestRunnerUsesTheInjectedStore(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")

	application := app.New(cfg, nil)
	store := memory.New()
	application.SetStore("r2", store)

	runner, err := application.Runner(t.Context(), target)
	require.NoError(t, err)
	assert.Same(t, store, runner.Store)
}

func TestStoreRejectsAnUndeclaredDestination(t *testing.T) {
	cfg := load(t)
	cfg.Targets[0].Destination = "elsewhere"

	_, err := app.New(cfg, nil).Store(t.Context(), "elsewhere")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestLayoutFollowsTheDestinationPrefix(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")

	layout, err := app.New(cfg, nil).Layout(target)

	require.NoError(t, err)
	assert.Equal(t, "prod", layout.Prefix)
	assert.Equal(t, "prod-pg", layout.Target)
}

func TestRetentionMapsTheDeclaredPolicy(t *testing.T) {
	cfg, diags, err := config.Parse([]byte(`
version: 1
destinations:
  - name: r2
    provider: r2
    bucket: b
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: k
    secret_access_key: s
targets:
  - name: prod-pg
    engine: postgres
    dsn: postgres://backup@pg:5432/app
    destination: r2
    encryption: { mode: none }
    retention:
      hourly:  { keep: 24 }
      daily:   { keep: 7 }
      weekly:  { keep: 4, on: sunday }
      monthly: { keep: 12, on: 1 }
      yearly:  { keep: 3 }
      min_keep: 3
`), config.LoadOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "%v", diags)

	target, _ := cfg.Target("prod-pg")
	policy := app.New(cfg, nil).Retention(target)

	assert.Equal(t, 24, policy.Hourly.Keep)
	assert.Equal(t, 7, policy.Daily.Keep)
	assert.Equal(t, 4, policy.Weekly.Keep)
	assert.Equal(t, time.Sunday, policy.Weekly.On)
	assert.Equal(t, 12, policy.Monthly.Keep)
	assert.Equal(t, 1, policy.Monthly.On)
	assert.Equal(t, 3, policy.Yearly.Keep)
	assert.Equal(t, 3, policy.MinKeep)
}

// TestRetentionWithoutAPolicyKeepsEverything: an absent retention block must
// not become "delete everything but the floor".
func TestRetentionWithoutAPolicyKeepsEverything(t *testing.T) {
	cfg := load(t)
	target, _ := cfg.Target("prod-pg")
	target.Retention = nil

	policy := app.New(cfg, nil).Retention(target)
	plan := policy.Plan(retention.Input{Backups: []retention.Backup{
		{ID: "a", At: time.Now().Add(-72 * time.Hour)},
		{ID: "b", At: time.Now().Add(-48 * time.Hour)},
		{ID: "c", At: time.Now()},
	}})

	assert.Empty(t, plan.Delete)
	assert.Len(t, plan.Keep, 3)
}
