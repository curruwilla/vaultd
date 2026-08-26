package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/doctor"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/retention"
	"github.com/curruwilla/vaultd/internal/scheduler"
)

// downloadTTL is how long a presigned download URL lives. It is a bearer token
// for the most sensitive object vaultd touches, so it is measured in minutes:
// long enough to click, short enough that a URL in a chat log expires before
// anybody gets to it.
const downloadTTL = 5 * time.Minute

// doctorCache is how long a doctor report is reused. The Config screen shows
// it, and rerunning a full connectivity sweep on every page load would put
// real load on every database in the config.
const doctorCache = 60 * time.Second

// actions routes the mutating half of the API.
//
// It is deliberately small (SPEC §13). Backup now, verify now and prune are
// the three things an operator wants a button for; restore into production is
// not among them, and is reachable only from the CLI with --confirm.
func (s *Server) actions() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/targets/{name}/backup", s.actionBackup)
	mux.HandleFunc("POST /api/targets/{name}/verify", s.actionVerify)
	mux.HandleFunc("POST /api/targets/{name}/prune", s.actionPrune)

	return mux
}

//nolint:contextcheck // the run outlives the request on purpose; see startRun.
func (s *Server) actionBackup(w http.ResponseWriter, r *http.Request) {
	s.startRun(w, r, scheduler.KindBackup)
}

//nolint:contextcheck // the run outlives the request on purpose; see startRun.
func (s *Server) actionVerify(w http.ResponseWriter, r *http.Request) {
	s.startRun(w, r, scheduler.KindVerify)
}

// startRun begins a manual job in the background and answers with the run to
// watch.
//
// It answers immediately because a backup takes minutes to hours: holding the
// request open would mean the browser, every proxy in between and the daemon
// all had to agree to wait, and the first one to disagree would look like a
// failed backup.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request, kind scheduler.Kind) {
	if s.Exec == nil {
		writeError(w, http.StatusNotImplemented, "this vaultd is not running a scheduler, so it cannot start runs")
		return
	}

	target, ok := s.App.Config().Target(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "no target named %q", r.PathValue("name"))
		return
	}
	if kind == scheduler.KindVerify && target.Verify == nil {
		writeError(w, http.StatusBadRequest, "target %q declares no verification", target.Name)
		return
	}

	run, started := s.runs.begin(target.Name, string(kind))
	if !started {
		// Not an error: the operator clicked twice, or two of them clicked.
		// Handing back the run already going is the useful answer.
		writeJSON(w, http.StatusConflict, run)
		return
	}

	job := scheduler.Manual(target.Name, kind)
	if kind == scheduler.KindVerify {
		job.Level = target.Verify.Level
	}

	go s.execute(run.ID, job)
	writeJSON(w, http.StatusAccepted, run)
}

// execute runs the job on the daemon's context, capturing its log.
func (s *Server) execute(runID string, job scheduler.Job) {
	// A copy, so this run's logger does not become every run's logger.
	exec := *s.Exec
	exec.Log = slog.New(newRunHandler(s.log().Handler(), s.runs, runID))

	result := exec.Run(s.base(), job)

	state := RunSucceeded
	switch {
	case result.Err != nil:
		state = RunFailed
	case result.Outcome != scheduler.OutcomeRan:
		state = RunSkipped
	}

	detail := result.Detail
	if detail == "" {
		detail = string(result.Outcome)
	}
	s.runs.finish(runID, state, detail, result.BackupID, result.Err)
}

// actionPrune previews a prune, and applies it only against the plan that was
// previewed.
//
// The dry run is mandatory (SPEC §13), and the token that carries it is a
// digest of the exact backups the preview listed. So "apply" cannot mean "some
// other plan computed a moment later": if a backup finished in between, the
// digest changes and the operator is shown the new plan instead.
func (s *Server) actionPrune(w http.ResponseWriter, r *http.Request) {
	target, ok := s.App.Config().Target(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "no target named %q", r.PathValue("name"))
		return
	}

	runner, err := s.App.Pruner(r.Context(), target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	plan, _, err := runner.Plan(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	view := &RetentionView{
		Keep:    rowsOf(plan.Keep),
		Delete:  rowsOf(plan.Delete),
		Blocked: plan.Blocked,
		Freed:   plan.Bytes(),
	}
	response := PruneResponse{Plan: view, Token: planToken(plan)}

	confirm := r.URL.Query().Get("token")
	if confirm == "" {
		response.DryRun = true
		writeJSON(w, http.StatusOK, response)
		return
	}
	if confirm != response.Token {
		writeError(w, http.StatusConflict,
			"the plan changed since it was previewed; look at it again before applying")
		return
	}
	if len(plan.Delete) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}

	// The daemon's context, not the request's: a browser that navigates away
	// mid-apply must not leave objects deleted and the index unwritten.
	objects, err := runner.Apply(s.base(), plan, nil) //nolint:contextcheck
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	response.Applied = true
	response.Objects = objects
	writeJSON(w, http.StatusOK, response)
}

// PruneResponse is a plan and what was done with it.
type PruneResponse struct {
	Plan *RetentionView `json:"plan"`
	// Token identifies this exact plan; applying requires sending it back.
	Token   string `json:"token"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Applied bool   `json:"applied,omitempty"`
	Objects int    `json:"objects,omitempty"`
}

// planToken digests the backups a plan deletes, in order.
func planToken(plan retention.Plan) string {
	sum := sha256.New()
	for _, decision := range plan.Delete {
		sum.Write([]byte(decision.Backup.ID))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// apiRuns lists the runs this daemon started.
func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runs.list(r.URL.Query().Get("target")))
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q; the daemon remembers the last %d",
			r.PathValue("id"), runHistory)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// apiDownload hands back a short-lived URL to one backup object.
//
// The daemon does not proxy the bytes: a 4TB download through the process
// serving the UI would evict everything else it was doing. Nor does it hand
// the browser any credential — the URL is scoped to one object and expires.
func (s *Server) apiDownload(w http.ResponseWriter, r *http.Request) {
	target, ok := s.App.Config().Target(r.PathValue("target"))
	if !ok {
		writeError(w, http.StatusNotFound, "no target named %q", r.PathValue("target"))
		return
	}

	entries, err := s.entries(r.Context(), target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	id := r.PathValue("id")
	index := slices.IndexFunc(entries, func(e manifest.Entry) bool { return e.ID == id && e.Succeeded() })
	if index < 0 {
		writeError(w, http.StatusNotFound, "no backup %q in %s", id, target.Name)
		return
	}

	store, err := s.App.Store(r.Context(), target.Destination)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	presigner, ok := store.(core.Presigner)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"destination %q cannot issue download links", target.Destination)
		return
	}

	key := entries[index].Key
	if which := r.URL.Query().Get("object"); which == "manifest" {
		key = entries[index].ManifestKey
	} else if which == "globals" && entries[index].GlobalsKey != "" {
		key = entries[index].GlobalsKey
	}

	url, err := presigner.Presign(r.Context(), key, downloadTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":        url,
		"key":        key,
		"expires_in": int(downloadTTL.Seconds()),
	})
}

// apiDoctor runs the connectivity sweep the Config screen shows, reusing a
// recent answer rather than dialling every database on every page load.
func (s *Server) apiDoctor(w http.ResponseWriter, r *http.Request) {
	s.doctorMu.Lock()
	defer s.doctorMu.Unlock()

	if s.doctorReport != nil && time.Since(s.doctorAt) < doctorCache && r.URL.Query().Get("refresh") == "" {
		writeJSON(w, http.StatusOK, s.doctorReport)
		return
	}

	// Never from the UI: a health check that posts to a pager every time
	// somebody opens a page is a health check people mute.
	report := (&doctor.Doctor{App: s.App, Log: s.log(), Notify: false}).Run(r.Context())

	s.doctorReport = report
	s.doctorAt = time.Now()
	writeJSON(w, http.StatusOK, report)
}

// restoreCommand is the CLI invocation the Backup screen offers to copy. The
// UI never restores anything itself (SPEC §13): what it hands over is the
// command, with the destination left for a human to fill in.
func restoreCommand(target *config.Target, id string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "vaultd restore %s --to '<dsn>' --confirm", id)
	if target.Encryption != nil && target.Encryption.Mode == config.EncryptionAge {
		b.WriteString(" --identity-file /path/to/key.txt")
	}
	return b.String()
}
