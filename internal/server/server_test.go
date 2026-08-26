package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/metrics"
	"github.com/curruwilla/vaultd/internal/server"
	"github.com/curruwilla/vaultd/internal/storage/memory"
)

const configYAML = `
version: 1
defaults:
  encryption: { mode: none }
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret-value
    prefix: prod
targets:
  - name: prod-pg
    engine: postgres
    dsn: postgres://backup:hunter2@pg:5432/app
    destination: r2
    schedule: "0 3 * * *"
server:
  listen: ":8080"
  ui: true
  metrics: true
  auth: { mode: token, token: s3kret-token }
`

func newServer(t *testing.T) (*server.Server, *memory.Store) {
	t.Helper()

	cfg, diags, err := config.Parse([]byte(configYAML), config.LoadOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "%v", diags)

	store := memory.New()
	application := app.New(cfg, nil)
	application.SetStore("r2", store)

	return &server.Server{App: application, Metrics: metrics.New()}, store
}

func get(t *testing.T, s *server.Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	return recorder
}

// A liveness probe that needs a secret is a liveness probe that fails when the
// secret is rotated, so the probes are open on purpose.
func TestProbesNeedNoToken(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	assert.Equal(t, http.StatusOK, get(t, s, "/healthz", "").Code)
	assert.Equal(t, http.StatusOK, get(t, s, "/readyz", "").Code)
}

// Liveness must not depend on the bucket: restarting a healthy daemon every
// time S3 has a bad minute makes things worse, not better.
func TestLivenessIsIndependentOfTheBucket(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	s.App.SetStore("r2", &unreachable{})

	assert.Equal(t, http.StatusOK, get(t, s, "/healthz", "").Code)
	assert.Equal(t, http.StatusServiceUnavailable, get(t, s, "/readyz", "").Code,
		"readiness is what a rolling deploy waits on, and it does depend on the bucket")
}

// A target that has never run has no index object, and that is a perfectly
// ready daemon rather than an unreachable bucket.
func TestReadinessAcceptsAMissingIndex(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	assert.Equal(t, http.StatusOK, get(t, s, "/readyz", "").Code)
}

func TestTheAPINeedsTheToken(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	assert.Equal(t, http.StatusUnauthorized, get(t, s, "/api/targets", "").Code)
	assert.Equal(t, http.StatusUnauthorized, get(t, s, "/api/targets", "wrong").Code)
	assert.Equal(t, http.StatusOK, get(t, s, "/api/targets", "s3kret-token").Code)
}

func TestMetricsAreBehindTheToken(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	assert.Equal(t, http.StatusUnauthorized, get(t, s, "/metrics", "").Code)

	body := get(t, s, "/metrics", "s3kret-token")
	require.Equal(t, http.StatusOK, body.Code)
	assert.Contains(t, body.Body.String(), "vaultd_build_info")
}

// Guessing a token has to get slower, not stay free.
func TestRepeatedBadTokensAreThrottled(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	var last int
	for range 12 {
		last = get(t, s, "/api/targets", "wrong").Code
	}
	assert.Equal(t, http.StatusTooManyRequests, last)
}

// The UI never serves the raw YAML: what leaves here is what a screenshot may
// safely contain (SPEC §15).
func TestTheConfigEndpointRedactsEverySecret(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	body := get(t, s, "/api/config", "s3kret-token").Body.String()

	assert.Contains(t, body, "prod-pg")
	assert.NotContains(t, body, "hunter2")
	assert.NotContains(t, body, "s3cret-value")
	assert.NotContains(t, body, "s3kret-token")
}

func TestOverviewReportsWhatTheIndexHolds(t *testing.T) {
	t.Parallel()

	s, store := newServer(t)
	writeIndex(t, store, manifest.Entry{
		ID:          "01A",
		Target:      "prod-pg",
		Outcome:     manifest.OutcomeSucceeded,
		FinishedAt:  time.Now().UTC().Add(-time.Hour),
		Key:         "prod/prod-pg/x.pgdump",
		ManifestKey: "prod/prod-pg/x.manifest.json",
		Bytes:       2048,
	})

	var summaries []server.TargetSummary
	decode(t, get(t, s, "/api/targets", "s3kret-token"), &summaries)

	require.Len(t, summaries, 1)
	assert.Equal(t, "prod-pg", summaries[0].Name)
	assert.Equal(t, "01A", summaries[0].LastBackupID)
	assert.Equal(t, 1, summaries[0].Backups)
	assert.Equal(t, int64(2048), summaries[0].TotalBytes)
}

// The traffic light has to be explainable, so every colour carries its reason.
func TestHealthRules(t *testing.T) {
	t.Parallel()

	target := &config.Target{Name: "prod-pg", Schedule: "0 3 * * *"}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	fresh := &manifest.Entry{Outcome: manifest.OutcomeSucceeded, FinishedAt: now.Add(-2 * time.Hour)}
	stale := &manifest.Entry{Outcome: manifest.OutcomeSucceeded, FinishedAt: now.Add(-72 * time.Hour)}
	failed := &manifest.Entry{Outcome: manifest.OutcomeFailed, FinishedAt: now.Add(-time.Hour), Phase: "dump"}

	assert.Equal(t, server.HealthUnknown, server.Assess(target, nil, nil, now).Health)
	assert.Equal(t, server.HealthGreen, server.Assess(target, fresh, fresh, now).Health)
	assert.Equal(t, server.HealthRed, server.Assess(target, stale, stale, now).Health,
		"past max_age is red, and max_age comes from the schedule when nothing says otherwise")
	assert.Equal(t, server.HealthRed, server.Assess(target, failed, fresh, now).Health,
		"the newest backup is fine but the newest run failed")

	verified := *fresh
	no := false
	verified.VerifyOK = &no
	verified.VerifyLevel = "structural"
	assert.Equal(t, server.HealthAmber, server.Assess(target, &verified, &verified, now).Health)
}

// A target whose bucket cannot be read becomes that row's error. One
// unreachable destination must not blank the whole grid.
func TestOneBrokenTargetDoesNotBreakTheOverview(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	s.App.SetStore("r2", &unreachable{})

	recorder := get(t, s, "/api/targets", "s3kret-token")
	require.Equal(t, http.StatusOK, recorder.Code)

	var summaries []server.TargetSummary
	decode(t, recorder, &summaries)

	require.Len(t, summaries, 1)
	assert.NotEmpty(t, summaries[0].Error)
	assert.Equal(t, server.HealthUnknown, summaries[0].Health)
}

func writeIndex(t *testing.T, store *memory.Store, entries ...manifest.Entry) {
	t.Helper()

	var body []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		require.NoError(t, err)
		body = append(append(body, line...), '\n')
	}

	_, err := store.Put(t.Context(), "prod/_index/prod-pg.jsonl", strings.NewReader(string(body)), core.PutOptions{})
	require.NoError(t, err)
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), into))
}
