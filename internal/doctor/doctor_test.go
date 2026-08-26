package doctor_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/doctor"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

const configYAML = `
version: 1
defaults:
  encryption: { mode: none }
  notify: [ops]
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret
    prefix: prod
targets:
  - name: prod-pg
    engine: postgres
    dsn: postgres://backup@127.0.0.1:1/app
    destination: r2
    schedule: "0 3 * * *"
notifiers:
  - name: ops
    type: webhook
    url: %s
    secret: hmac
    events: [backup.failed]
`

func load(t *testing.T, notifierURL string) *config.Config {
	t.Helper()

	cfg, diags, err := config.Parse([]byte(strings.Replace(configYAML, "%s", notifierURL, 1)), config.LoadOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "%v", diags)
	return cfg
}

func find(t *testing.T, report *doctor.Report, name string) doctor.Check {
	t.Helper()

	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("no check named %q in %+v", name, report.Checks)
	return doctor.Check{}
}

// The bucket checks are the ones with real substance: they prove the store
// honours the two conditional writes the lock and the index are built on.
func TestDestinationChecksExerciseTheConditionalWrites(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := load(t, server.URL)
	application := app.New(cfg, nil)
	store := memory.New()
	application.SetStore("r2", store)

	report := (&doctor.Doctor{App: application}).Run(context.Background())

	assert.Equal(t, doctor.StatusOK, find(t, report, "r2: conditional write").Status)
	assert.Equal(t, doctor.StatusOK, find(t, report, "r2: read back").Status)
	assert.Equal(t, doctor.StatusOK, find(t, report, "r2: compare-and-swap").Status)
	assert.Equal(t, doctor.StatusOK, find(t, report, "r2: delete").Status)

	// Whatever else it did, doctor does not leave its canary behind.
	for key := range store.Objects() {
		assert.NotContains(t, key, "_doctor", "the canary object must be cleaned up")
	}
}

// A store that ignores If-None-Match would hand the target lock to every
// caller at once, so it has to be called out rather than passed over.
func TestConditionalWriteFailureIsFatal(t *testing.T) {
	t.Parallel()

	cfg := load(t, "https://example.invalid/hook")
	application := app.New(cfg, nil)
	application.SetStore("r2", &alwaysCreates{Store: memory.New()})

	report := (&doctor.Doctor{App: application}).Run(context.Background())

	check := find(t, report, "r2: conditional write")
	assert.Equal(t, doctor.StatusFail, check.Status)
	assert.Contains(t, check.Detail, "If-None-Match")
	assert.False(t, report.OK())
}

// A target whose server is unreachable is what doctor exists to find.
func TestUnreachableTargetFails(t *testing.T) {
	t.Parallel()

	cfg := load(t, "https://example.invalid/hook")
	application := app.New(cfg, nil)
	application.SetStore("r2", memory.New())

	report := (&doctor.Doctor{App: application}).Run(context.Background())

	assert.Equal(t, doctor.StatusFail, find(t, report, "prod-pg").Status)
	assert.False(t, report.OK())
}

// The default run dials the notifier and posts nothing: a health check that
// pages on-call every time it runs is a health check people mute.
func TestNotifierIsDialledNotPosted(t *testing.T) {
	t.Parallel()

	var posted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := load(t, server.URL)
	application := app.New(cfg, nil)
	application.SetStore("r2", memory.New())

	report := (&doctor.Doctor{App: application}).Run(context.Background())

	assert.Equal(t, doctor.StatusOK, find(t, report, "ops").Status)
	assert.Zero(t, posted, "doctor must not deliver anything without --notify")
}

func TestNotifySendsASignedDelivery(t *testing.T) {
	t.Parallel()

	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Vaultd-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := load(t, server.URL)
	application := app.New(cfg, nil)
	application.SetStore("r2", memory.New())

	report := (&doctor.Doctor{App: application, Notify: true}).Run(context.Background())

	assert.Equal(t, doctor.StatusOK, find(t, report, "ops").Status)
	assert.NotEmpty(t, got, "the test delivery is signed like a real one")
}

func TestReportOKIgnoresWarnings(t *testing.T) {
	t.Parallel()

	report := &doctor.Report{Checks: []doctor.Check{
		{Status: doctor.StatusOK},
		{Status: doctor.StatusWarn},
	}}
	assert.True(t, report.OK(), "a warning is something to fix, not something that stops tonight's backup")

	report.Checks = append(report.Checks, doctor.Check{Status: doctor.StatusFail})
	assert.False(t, report.OK())
}

// alwaysCreates is a store whose conditional write is a lie: it reports the
// key as created every time. It stands in for an S3 implementation without
// If-None-Match support.
type alwaysCreates struct{ *memory.Store }

func (s *alwaysCreates) PutIfAbsent(ctx context.Context, key string, b []byte) (bool, error) {
	if _, _, err := s.PutIfMatch(ctx, key, b, ""); err != nil {
		return false, err
	}
	return true, nil
}

var _ core.Store = (*alwaysCreates)(nil)
