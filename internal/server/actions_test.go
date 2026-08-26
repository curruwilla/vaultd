package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/scheduler"
	"github.com/curruwilla/vaultd/internal/server"
)

// fakeDumper stands in for pg_dump.
type fakeDumper struct {
	dumps atomic.Int32
	delay time.Duration
}

func (f *fakeDumper) Probe(context.Context) (core.ServerInfo, error) {
	return core.ServerInfo{Engine: core.EnginePostgres, Version: "17.2"}, nil
}

func (f *fakeDumper) Dump(ctx context.Context, w io.Writer) (core.DumpResult, error) {
	f.dumps.Add(1)

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return core.DumpResult{}, ctx.Err()
		}
	}
	_, err := w.Write(bytes.Repeat([]byte("PGDMP\n"), 32))
	return core.DumpResult{DumperVersion: "pg_dump 17.2"}, err
}

// runnable is a server wired to a scheduler, the way `vaultd serve` wires one.
func runnable(t *testing.T, dumper core.Dumper) *server.Server {
	t.Helper()

	s, _ := newServer(t)
	s.App.SetDumper("prod-pg", dumper)
	s.Exec = &scheduler.Executor{App: s.App, LockTTL: time.Minute}
	return s
}

func post(t *testing.T, s *server.Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer s3kret-token")

	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	return recorder
}

// A backup takes minutes to hours, so the button cannot hold the request open:
// it answers with the run to watch.
func TestBackupNowAnswersImmediatelyWithARunToWatch(t *testing.T) {
	t.Parallel()

	dumper := &fakeDumper{}
	s := runnable(t, dumper)

	recorder := post(t, s, "/api/targets/prod-pg/backup")
	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())

	var run server.Run
	decode(t, recorder, &run)
	assert.Equal(t, "prod-pg", run.Target)
	assert.Equal(t, server.RunRunning, run.State)

	finished := waitForRun(t, s, run.ID)
	assert.Equal(t, server.RunSucceeded, finished.State)
	assert.NotEmpty(t, finished.BackupID)
	assert.Equal(t, int32(1), dumper.dumps.Load())

	// The run carries its own log, which is the reason the screen is worth
	// opening at all.
	assert.NotEmpty(t, finished.Log)
}

// Two clicks are one run. The bucket lock covers other processes; this is the
// local half, so the same UI does not start work nobody watched begin.
func TestASecondClickJoinsTheRunAlreadyGoing(t *testing.T) {
	t.Parallel()

	dumper := &fakeDumper{delay: 300 * time.Millisecond}
	s := runnable(t, dumper)

	first := post(t, s, "/api/targets/prod-pg/backup")
	require.Equal(t, http.StatusAccepted, first.Code)

	second := post(t, s, "/api/targets/prod-pg/backup")
	assert.Equal(t, http.StatusConflict, second.Code)

	var run server.Run
	decode(t, first, &run)
	waitForRun(t, s, run.ID)
	assert.Equal(t, int32(1), dumper.dumps.Load())
}

// The daemon has to say plainly that it cannot start runs, rather than
// accepting a click and doing nothing with it.
func TestBackupNowIsRefusedWithoutAScheduler(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	assert.Equal(t, http.StatusNotImplemented, post(t, s, "/api/targets/prod-pg/backup").Code)
}

func TestVerifyNowNeedsADeclaredVerification(t *testing.T) {
	t.Parallel()

	s := runnable(t, &fakeDumper{})
	assert.Equal(t, http.StatusBadRequest, post(t, s, "/api/targets/prod-pg/verify").Code)
}

// The dry run is mandatory, and the token that carries it is a digest of the
// exact backups the preview listed: "apply" cannot mean some other plan
// computed a moment later.
func TestPruneNeedsThePlanItPreviewed(t *testing.T) {
	t.Parallel()

	s := runnable(t, &fakeDumper{})

	preview := post(t, s, "/api/targets/prod-pg/prune")
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())

	var response server.PruneResponse
	decode(t, preview, &response)
	assert.True(t, response.DryRun)
	assert.NotEmpty(t, response.Token)

	stale := post(t, s, "/api/targets/prod-pg/prune?token=0000000000000000")
	assert.Equal(t, http.StatusConflict, stale.Code)
	assert.Contains(t, stale.Body.String(), "changed since it was previewed")

	applied := post(t, s, "/api/targets/prod-pg/prune?token="+response.Token)
	assert.Equal(t, http.StatusOK, applied.Code)
}

// The daemon does not proxy the bytes and does not hand the browser a
// credential — but a store that cannot presign has to say so, not fail oddly.
func TestDownloadNeedsAStoreThatCanPresign(t *testing.T) {
	t.Parallel()

	s := runnable(t, &fakeDumper{})

	recorder := post(t, s, "/api/targets/prod-pg/backup")
	require.Equal(t, http.StatusAccepted, recorder.Code)

	var run server.Run
	decode(t, recorder, &run)
	finished := waitForRun(t, s, run.ID)
	require.Equal(t, server.RunSucceeded, finished.State)

	answer := get(t, s, "/api/backups/prod-pg/"+finished.BackupID+"/download", "s3kret-token")
	assert.Equal(t, http.StatusNotImplemented, answer.Code)
	assert.Contains(t, answer.Body.String(), "cannot issue download links")
}

// The Backup screen offers the command, never the restore: v1 restores into an
// explicit destination from the CLI, with --confirm (SPEC §13).
func TestTheBackupScreenOffersTheRestoreCommandAndNoRestore(t *testing.T) {
	t.Parallel()

	s := runnable(t, &fakeDumper{})

	recorder := post(t, s, "/api/targets/prod-pg/backup")
	require.Equal(t, http.StatusAccepted, recorder.Code)

	var run server.Run
	decode(t, recorder, &run)
	finished := waitForRun(t, s, run.ID)

	var detail server.BackupDetail
	decode(t, get(t, s, "/api/backups/prod-pg/"+finished.BackupID, "s3kret-token"), &detail)

	assert.Contains(t, detail.RestoreCommand, "vaultd restore "+finished.BackupID)
	assert.Contains(t, detail.RestoreCommand, "--confirm")

	assert.Equal(t, http.StatusNotFound, post(t, s, "/api/targets/prod-pg/restore").Code,
		"there is no restore endpoint to find")
}

// The effective config is what vaultd resolved, not the file on disk: the file
// is full of ${VAR} and says nothing about what a target inherited.
func TestTheConfigEndpointServesResolvedYAML(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	var effective server.EffectiveConfig
	decode(t, get(t, s, "/api/config", "s3kret-token"), &effective)

	assert.Contains(t, effective.YAML, "name: prod-pg")
	assert.Contains(t, effective.YAML, `dsn: "***"`)
	assert.NotContains(t, effective.YAML, "hunter2")
	assert.NotContains(t, effective.YAML, "s3kret-token")
}

func waitForRun(t *testing.T, s *server.Server, id string) server.Run {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var run server.Run
		recorder := get(t, s, "/api/runs/"+id, "s3kret-token")
		require.Equal(t, http.StatusOK, recorder.Code)
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &run))

		if run.State != server.RunRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("run %s never finished", id)
	return server.Run{}
}
