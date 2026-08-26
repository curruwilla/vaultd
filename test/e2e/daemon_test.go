//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/manifest"
)

// daemonTemplate is one target and a server block: the daemon suite is about
// scheduling and locking, and dragging three more engines through it would
// only make it slower and flakier.
const daemonTemplate = `
version: 1

defaults:
  compression: { algo: zstd, level: 1 }
  encryption:  { mode: age, recipients: ["${E2E_AGE_RECIPIENT}"] }
  retention:   { daily: { keep: 7 }, min_keep: 1 }
  timeout: 5m

destinations:
  - name: bucket
    provider: ${E2E_PROVIDER}
    bucket: ${E2E_BUCKET}
    endpoint: ${E2E_ENDPOINT}
    region: ${E2E_REGION}
    access_key_id: ${E2E_ACCESS_KEY_ID}
    secret_access_key: ${E2E_SECRET_ACCESS_KEY}
    prefix: ${E2E_PREFIX}

targets:
  - name: prod-pg
    engine: postgres
    dsn: ${E2E_PG_DSN}
    destination: bucket
    schedule: "0 3 * * *"

server:
  listen: "${E2E_LISTEN}"
  ui: true
  metrics: true
  auth: { mode: token, token: "${E2E_TOKEN}" }
`

func storedBackups(t *testing.T, bucket string) []manifest.Entry {
	t.Helper()

	store := newStore(t, bucket)
	if _, err := store.Head(t.Context(), "e2e/_index/prod-pg.jsonl"); err != nil {
		// No index yet is no backups yet, which is a perfectly good answer to
		// the question and not a reason to fail the test.
		return nil
	}

	var stored []manifest.Entry
	for _, entry := range indexEntries(t, store, "e2e/_index/prod-pg.jsonl") {
		if entry.Succeeded() {
			stored = append(stored, entry)
		}
	}
	return stored
}

// `vaultd run` is what a Kubernetes CronJob invokes. Running it twice in a row
// must not take two backups: what is due comes from the bucket, not from the
// invocation.
func TestRunOnceIsIdempotent(t *testing.T) {
	t.Setenv("E2E_LISTEN", "127.0.0.1:0")
	t.Setenv("E2E_TOKEN", "t0ken")
	configPath, _ := setupWith(t, "vaultd-e2e-run", daemonTemplate)

	stdout, stderr, err := run(t, "run", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "prod-pg")
	require.Len(t, storedBackups(t, "vaultd-e2e-run"), 1)

	stdout, stderr, err = run(t, "run", "-c", configPath)
	require.NoError(t, err, "stderr: %s", stderr)
	assert.Contains(t, stdout, "nothing was due")
	assert.Len(t, storedBackups(t, "vaultd-e2e-run"), 1)
}

// The M7 acceptance gate, against a real bucket: two processes racing for the
// same target produce one backup. This is the test that proves the store's
// conditional writes actually behave — the in-memory one can only prove the
// logic.
func TestTwoConcurrentRunsProduceOneBackup(t *testing.T) {
	t.Setenv("E2E_LISTEN", "127.0.0.1:0")
	t.Setenv("E2E_TOKEN", "t0ken")
	configPath, _ := setupWith(t, "vaultd-e2e-replicas", daemonTemplate)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		outputs []string
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			stdout, _, err := run(t, "run", "-c", configPath)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				outputs = append(outputs, stdout)
			}
		}()
	}
	wg.Wait()

	require.Len(t, outputs, 2, "neither replica may fail; the loser reports why instead")
	assert.Len(t, storedBackups(t, "vaultd-e2e-replicas"), 1,
		"the database must be dumped exactly once")

	// One of them says it did the work, the other says it could not.
	combined := strings.Join(outputs, "")
	assert.True(t,
		strings.Contains(combined, "locked") || strings.Contains(combined, "not_due") ||
			strings.Contains(combined, "nothing was due"),
		"the replica that lost has to say so: %s", combined)
}

// A manual backup takes the same lock the daemon does, so running one by hand
// while `vaultd serve` is up cannot dump the database twice (SPEC §11).
//
// Unlike a scheduled run, the loser fails rather than shrugging: a person
// asked for this, and telling them nothing happened is the wrong answer.
func TestAManualBackupTakesTheTargetLock(t *testing.T) {
	t.Setenv("E2E_LISTEN", "127.0.0.1:0")
	t.Setenv("E2E_TOKEN", "t0ken")
	configPath, _ := setupWith(t, "vaultd-e2e-manual-lock", daemonTemplate)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		results int
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, _, err := run(t, "backup", "prod-pg", "-c", configPath)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results++
		}()
	}
	wg.Wait()

	// Two invocations at once, and the database is dumped once. Whether the
	// loser was refused the lock or got it a moment later is a race the test
	// must not depend on — what matters is that the bucket holds one backup
	// per successful invocation, and never two from one dump.
	assert.Len(t, storedBackups(t, "vaultd-e2e-manual-lock"), results)
	assert.GreaterOrEqual(t, results, 1, "at least one invocation has to succeed")

	for _, err := range errs {
		assert.Contains(t, err.Error(), "already being backed up",
			"a manual backup that loses the lock says so")
	}
}

// `--dry-run` answers the question a CronJob's operator asks before enabling
// it, and touches nothing.
func TestRunDryRunTakesNoBackup(t *testing.T) {
	t.Setenv("E2E_LISTEN", "127.0.0.1:0")
	t.Setenv("E2E_TOKEN", "t0ken")
	configPath, _ := setupWith(t, "vaultd-e2e-dryrun", daemonTemplate)

	stdout, stderr, err := run(t, "run", "-c", configPath, "--dry-run")
	require.NoError(t, err, "stderr: %s", stderr)

	assert.Contains(t, stdout, "prod-pg")
	// Nothing at all: no object, and no index either. A dry run that wrote a
	// lock or an index would have changed the bucket it was asked to describe.
	assert.Empty(t, objectKeys(t, newStore(t, "vaultd-e2e-dryrun"), "e2e/"))
}

// The daemon's HTTP surface: probes open, metrics and API behind the token.
func TestServeServesProbesMetricsAndAPI(t *testing.T) {
	address := freePort(t)

	t.Setenv("E2E_LISTEN", address)
	t.Setenv("E2E_TOKEN", "t0ken")
	configPath, _ := setupWith(t, "vaultd-e2e-serve", daemonTemplate)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	done := make(chan error, 1)
	go func() {
		_, _, err := runContext(ctx, t, "serve", "-c", configPath, "--interval", "1h")
		done <- err
	}()

	base := "http://" + address
	waitForHTTP(t, base+"/healthz")

	assert.Equal(t, http.StatusOK, status(t, base+"/healthz", ""))
	assert.Equal(t, http.StatusOK, status(t, base+"/readyz", ""))
	assert.Equal(t, http.StatusUnauthorized, status(t, base+"/metrics", ""))

	body := fetch(t, base+"/metrics", "t0ken")
	assert.Contains(t, body, "vaultd_build_info")

	body = fetch(t, base+"/api/status", "t0ken")
	assert.Contains(t, body, "prod-pg")

	// The effective config is served with every secret redacted (SPEC §15).
	body = fetch(t, base+"/api/config", "t0ken")
	assert.Contains(t, body, "prod-pg")
	assert.NotContains(t, body, "t0ken")
	assert.NotContains(t, body, env.secretKey)

	// The UI shell loads without a token — it has to, before it can ask for
	// one — and a deep link reloads into the app rather than a 404.
	assert.Contains(t, fetch(t, base+"/", ""), "<title>vaultd</title>")
	assert.Contains(t, fetch(t, base+"/t/prod-pg", ""), "<title>vaultd</title>")
	assert.Contains(t, fetch(t, base+"/app.js", ""), "vaultd UI")

	// "Back up now" goes through the same executor the schedule does, takes
	// the same lock and lands in the same index — which is why it may find
	// the daemon's own first-tick run already holding the target. Either way
	// the button starts a run that finishes, and one backup exists after it.
	run := postJSON(t, base+"/api/targets/prod-pg/backup", "t0ken")
	id, ok := run["id"].(string)
	require.True(t, ok, "the response has to name the run to watch: %v", run)

	finished := awaitRun(t, base, id)
	assert.Contains(t, []any{"succeeded", "skipped"}, finished["state"], "%v", finished)

	awaitBackup(t, "vaultd-e2e-serve")

	stop()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not shut down")
	}
}

func postJSON(t *testing.T, url, token string) map[string]any {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))

	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// awaitBackup waits for a backup to land, which the daemon may be taking on
// its own schedule at the same moment the test asks for one.
func awaitBackup(t *testing.T, bucket string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if len(storedBackups(t, bucket)) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no backup was stored")
}

func awaitRun(t *testing.T, base, id string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var run map[string]any
		require.NoError(t, json.Unmarshal([]byte(fetch(t, base+"/api/runs/"+id, "t0ken")), &run))

		if run["state"] != "running" {
			return run
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("run %s never finished", id)
	return nil
}

// freePort reserves a port by binding it and letting go, which is close enough
// for a test that starts one server.
func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	return listener.Addr().String()
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status(t, url, "") == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never came up", url)
}

func status(t *testing.T, url, token string) int {
	t.Helper()

	resp, err := do(t, url, token)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func fetch(t *testing.T, url, token string) string {
	t.Helper()

	resp, err := do(t, url, token)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", url, body)
	return string(body)
}

func do(t *testing.T, url, token string) (*http.Response, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	return client.Do(req)
}
