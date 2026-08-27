package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/metrics"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	return recorder.Body.String()
}

func TestBackupSuccessPublishesTheSeriesAnAlertReads(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	at := time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC)
	m.BackupSucceeded("prod-pg", "postgres", 84*time.Second, 4193282104, 19388211004, at)

	body := scrape(t, m)

	assert.Contains(t, body, `vaultd_backup_last_success_timestamp{target="prod-pg"} 1.7875413e+09`)
	assert.Contains(t, body, `vaultd_backup_bytes{kind="compressed",target="prod-pg"} 4.193282104e+09`)
	assert.Contains(t, body, `vaultd_backup_bytes{kind="plain",target="prod-pg"} 1.9388211004e+10`)
	assert.Contains(t, body, `vaultd_backup_duration_seconds_count{engine="postgres",target="prod-pg"} 1`)
}

// A backup is minutes-to-hours work. With the default client buckets, which
// stop at ten seconds, every real run would land in +Inf and the histogram
// would say nothing at all.
func TestDurationBucketsCoverRealBackups(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.BackupSucceeded("prod-pg", "postgres", 40*time.Minute, 1, 2, time.Now())

	body := scrape(t, m)
	assert.Contains(t, body, `vaultd_backup_duration_seconds_bucket{engine="postgres",target="prod-pg",le="3600"} 1`)
	assert.Contains(t, body, `vaultd_backup_duration_seconds_bucket{engine="postgres",target="prod-pg",le="1800"} 0`)
}

func TestFailuresCountByPhase(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.BackupFailed("prod-pg", "dump")
	m.BackupFailed("prod-pg", "dump")
	m.BackupFailed("prod-pg", "upload")

	body := scrape(t, m)
	assert.Contains(t, body, `vaultd_backup_failures_total{phase="dump",target="prod-pg"} 2`)
	assert.Contains(t, body, `vaultd_backup_failures_total{phase="upload",target="prod-pg"} 1`)
}

// The engine is only known once the probe has run, so a failed run has no
// value for that label. Reaching for the duration histogram to record one
// would publish a permanent engine="" series that describes nothing.
func TestAFailedRunStampsNoDurationSeries(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.BackupFailed("prod-pg", "probe")

	assert.NotContains(t, scrape(t, m), "vaultd_backup_duration_seconds",
		"a failed run has no engine and no duration to publish")
}

// A restarted daemon whose gauges begin at zero would fire every age alert it
// has. Seeding from the index is what stops a deploy paging on-call.
func TestSeedingPublishesTheLastKnownRun(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.SeedBackup("prod-pg", time.Date(2026, 8, 24, 3, 15, 0, 0, time.UTC), 100, 200)
	m.SeedVerify("prod-pg", "structural", time.Date(2026, 8, 24, 5, 0, 0, 0, time.UTC))

	body := scrape(t, m)
	assert.Contains(t, body, `vaultd_backup_last_success_timestamp{target="prod-pg"} 1.7875413e+09`)
	assert.Contains(t, body, `vaultd_verify_last_success_timestamp{level="structural",target="prod-pg"} 1.7875476e+09`)
}

func TestSeedingATargetThatNeverRanPublishesNothing(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.SeedBackup("prod-pg", time.Time{}, 0, 0)

	assert.NotContains(t, scrape(t, m), "vaultd_backup_last_success_timestamp",
		"a zero timestamp would read as 1970 and alert immediately")
}

// A tier that stopped keeping anything has to fall to zero rather than keep
// its last value forever, so the whole target is rewritten at once.
func TestRetentionGaugeReplacesTheWholeTarget(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	m.RetentionObjects("prod-pg", map[string]int{"daily": 7, "weekly": 4})
	m.RetentionObjects("prod-pg", map[string]int{"daily": 5})

	body := scrape(t, m)
	assert.Contains(t, body, `vaultd_retention_objects{target="prod-pg",tier="daily"} 5`)
	assert.NotContains(t, body, `tier="weekly"`)
}

func TestBuildInfoIsExported(t *testing.T) {
	t.Parallel()

	body := scrape(t, metrics.New())
	assert.Contains(t, body, "vaultd_build_info")
}
