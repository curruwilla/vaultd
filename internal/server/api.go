package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/curruwilla/vaultd/internal/buildinfo"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/manifest"
	"github.com/curruwilla/vaultd/internal/retention"
)

// Status is the daemon's view of its own schedule, supplied by whoever runs
// the scheduler.
type Status struct {
	StartedAt time.Time   `json:"started_at"`
	Uptime    string      `json:"uptime"`
	Jobs      []JobStatus `json:"jobs"`
}

// JobStatus is one scheduled job as the API reports it.
type JobStatus struct {
	Target   string    `json:"target"`
	Kind     string    `json:"kind"`
	Schedule string    `json:"schedule"`
	LastRun  time.Time `json:"last_run,omitempty"`
	Next     time.Time `json:"next,omitempty"`
}

// TargetSummary is one row of the overview grid.
type TargetSummary struct {
	Name        string      `json:"name"`
	Engine      core.Engine `json:"engine"`
	Destination string      `json:"destination"`
	Schedule    string      `json:"schedule,omitempty"`
	Health      Health      `json:"health"`
	Reason      string      `json:"reason"`

	LastBackupAt   time.Time `json:"last_backup_at,omitempty"`
	LastBackupID   string    `json:"last_backup_id,omitempty"`
	AgeSeconds     int64     `json:"age_seconds,omitempty"`
	Bytes          int64     `json:"bytes,omitempty"`
	PlaintextBytes int64     `json:"plaintext_bytes,omitempty"`
	Backups        int       `json:"backups"`
	TotalBytes     int64     `json:"total_bytes"`

	VerifyLevel string    `json:"verify_level,omitempty"`
	VerifiedAt  time.Time `json:"verified_at,omitempty"`
	VerifyOK    *bool     `json:"verify_ok,omitempty"`

	LastFailure string    `json:"last_failure,omitempty"`
	FailedAt    time.Time `json:"failed_at,omitempty"`

	// Error is set when this target's own state could not be read. The rest of
	// the grid still renders: one unreachable bucket must not blank the page.
	Error string `json:"error,omitempty"`
}

// TargetDetail is the target screen: the timeline, and what the next prune
// would do to it.
type TargetDetail struct {
	TargetSummary
	Entries   []manifest.Entry `json:"entries"`
	Retention *RetentionView   `json:"retention,omitempty"`
}

// RetentionView is the projected effect of the policy as it stands, which is
// the answer to the only question anybody asks a retention screen: what
// disappears next.
type RetentionView struct {
	Keep    []RetentionRow `json:"keep"`
	Delete  []RetentionRow `json:"delete"`
	Blocked string         `json:"blocked,omitempty"`
	Freed   int64          `json:"freed"`
}

// RetentionRow is one backup and why it survives, or does not.
type RetentionRow struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Bytes  int64     `json:"bytes"`
	Reason string    `json:"reason"`
}

// api routes the read-only surface.
func (s *Server) api() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/version", s.apiVersion)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /api/targets", s.apiTargets)
	mux.HandleFunc("GET /api/targets/{name}", s.apiTarget)
	mux.HandleFunc("GET /api/backups/{target}/{id}", s.apiBackup)
	mux.HandleFunc("GET /api/config", s.apiConfig)

	return mux
}

func (s *Server) apiVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if s.Status == nil {
		writeJSON(w, http.StatusOK, Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.Status(r.Context()))
}

// apiTargets is the overview. Every target is summarized independently and a
// failure on one becomes that row's `error` rather than the response's.
func (s *Server) apiTargets(w http.ResponseWriter, r *http.Request) {
	cfg := s.App.Config()

	summaries := make([]TargetSummary, 0, len(cfg.Targets))
	for i := range cfg.Targets {
		summaries = append(summaries, s.summarize(r.Context(), &cfg.Targets[i]))
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (s *Server) apiTarget(w http.ResponseWriter, r *http.Request) {
	target, ok := s.App.Config().Target(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "no target named %q", r.PathValue("name"))
		return
	}

	detail := TargetDetail{TargetSummary: s.summarize(r.Context(), target)}

	entries, err := s.entries(r.Context(), target)
	if err != nil {
		detail.Error = err.Error()
		writeJSON(w, http.StatusOK, detail)
		return
	}

	sort.Slice(entries, func(a, b int) bool { return entries[a].FinishedAt.After(entries[b].FinishedAt) })
	detail.Entries = entries
	detail.Retention = s.projectRetention(r.Context(), target)

	writeJSON(w, http.StatusOK, detail)
}

// apiBackup returns the full manifest of one backup.
func (s *Server) apiBackup(w http.ResponseWriter, r *http.Request) {
	target, ok := s.App.Config().Target(r.PathValue("target"))
	if !ok {
		writeError(w, http.StatusNotFound, "no target named %q", r.PathValue("target"))
		return
	}

	idx, err := s.App.Index(r.Context(), target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	entries, _, err := idx.Entries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	id := r.PathValue("id")
	for _, entry := range entries {
		if entry.ID != id || !entry.Succeeded() {
			continue
		}

		m, err := idx.Manifest(r.Context(), entry.ManifestKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "%s", err)
			return
		}
		writeJSON(w, http.StatusOK, m)
		return
	}

	writeError(w, http.StatusNotFound, "no backup %q in %s", id, target.Name)
}

// apiConfig renders the effective configuration with every secret redacted.
//
// The UI never serves the raw YAML (SPEC §15): config.Secret redacts itself
// through MarshalJSON, so what leaves here is what a screenshot may safely
// contain.
func (s *Server) apiConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.App.Config())
}

// summarize builds one overview row.
func (s *Server) summarize(ctx context.Context, target *config.Target) TargetSummary {
	summary := TargetSummary{
		Name:        target.Name,
		Engine:      target.Engine,
		Destination: target.Destination,
		Schedule:    target.Schedule,
	}
	if target.Verify != nil {
		summary.VerifyLevel = string(target.Verify.Level)
	}

	entries, err := s.entries(ctx, target)
	if err != nil {
		summary.Health = HealthUnknown
		summary.Error = err.Error()
		summary.Reason = "the index could not be read"
		return summary
	}

	latest, latestSuccess := newest(entries)
	now := time.Now().UTC()

	assessment := Assess(target, latest, latestSuccess, now)
	summary.Health = assessment.Health
	summary.Reason = assessment.Reason

	for _, entry := range entries {
		if entry.Succeeded() {
			summary.Backups++
			summary.TotalBytes += entry.Bytes
		}
	}

	if latestSuccess != nil {
		summary.LastBackupAt = latestSuccess.FinishedAt
		summary.LastBackupID = latestSuccess.ID
		summary.AgeSeconds = int64(now.Sub(latestSuccess.FinishedAt).Seconds())
		summary.Bytes = latestSuccess.Bytes
		summary.PlaintextBytes = latestSuccess.PlaintextBytes
		summary.VerifyOK = latestSuccess.VerifyOK
		if latestSuccess.VerifiedAt != nil {
			summary.VerifiedAt = *latestSuccess.VerifiedAt
		}
	}
	if latest != nil && !latest.Succeeded() {
		summary.LastFailure = latest.Error
		summary.FailedAt = latest.FinishedAt
	}

	return summary
}

// projectRetention answers "what disappears at the next prune". It is a plan,
// so it deletes nothing.
func (s *Server) projectRetention(ctx context.Context, target *config.Target) *RetentionView {
	runner, err := s.App.Pruner(ctx, target)
	if err != nil {
		return nil
	}

	plan, _, err := runner.Plan(ctx)
	if err != nil {
		return nil
	}

	return &RetentionView{
		Keep:    rowsOf(plan.Keep),
		Delete:  rowsOf(plan.Delete),
		Blocked: plan.Blocked,
		Freed:   plan.Bytes(),
	}
}

func rowsOf(decisions []retention.Decision) []RetentionRow {
	rows := make([]RetentionRow, 0, len(decisions))
	for _, decision := range decisions {
		rows = append(rows, RetentionRow{
			ID:     decision.Backup.ID,
			At:     decision.Backup.At,
			Bytes:  decision.Backup.Bytes,
			Reason: decision.Why(),
		})
	}
	return rows
}

func (s *Server) entries(ctx context.Context, target *config.Target) ([]manifest.Entry, error) {
	idx, err := s.App.Index(ctx, target)
	if err != nil {
		return nil, err
	}
	entries, _, err := idx.Entries(ctx)
	return entries, err
}

// newest returns the most recent run and the most recent successful one. They
// differ exactly when the last attempt failed, which is the case the traffic
// light cares about most.
func newest(entries []manifest.Entry) (latest, latestSuccess *manifest.Entry) {
	for i := range entries {
		entry := &entries[i]

		if latest == nil || entry.FinishedAt.After(latest.FinishedAt) {
			latest = entry
		}
		if entry.Succeeded() && (latestSuccess == nil || entry.FinishedAt.After(latestSuccess.FinishedAt)) {
			latestSuccess = entry
		}
	}
	return latest, latestSuccess
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The API is read-mostly and same-origin; refusing to be framed costs
	// nothing and closes the clickjacking route to the action endpoints.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}
