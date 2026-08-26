// Package metrics is the Prometheus surface of vaultd (SPEC §14).
//
// The metric that matters most is the least exciting one:
// vaultd_backup_last_success_timestamp. Every other series describes a run
// that happened, and a backup tool fails by runs not happening at all — so the
// alert an operator actually needs is on the age of that timestamp, not on a
// failure counter that stays flat when the scheduler is dead.
//
// That is also why the daemon seeds the timestamps from the index at startup:
// a restarted process whose gauges begin at zero would fire every age alert it
// has, and an alert that cries wolf on every deploy gets muted.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/curruwilla/vaultd/internal/buildinfo"
)

// Kind labels the two sizes a backup has: what the database produced, and what
// ended up in the bucket. The ratio between them is the compression the
// pipeline achieved, and a sudden change in it is worth looking at.
const (
	KindCompressed = "compressed"
	KindPlain      = "plain"
)

// durationBuckets span a second to a little over four hours. Backups are
// minutes-to-hours work, so the default Prometheus buckets — which top out at
// ten seconds — would put every real run in +Inf.
var durationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400}

// Metrics holds every collector vaultd exports, and the registry they live in.
//
// It is a value passed around rather than a package-level default: two
// registries in one process is what lets a test assert on its own metrics
// without the rest of the suite leaking into them.
type Metrics struct {
	registry *prometheus.Registry

	backupDuration    *prometheus.HistogramVec
	backupBytes       *prometheus.GaugeVec
	backupLastSuccess *prometheus.GaugeVec
	backupFailures    *prometheus.CounterVec
	verifyLastSuccess *prometheus.GaugeVec
	retentionObjects  *prometheus.GaugeVec
	scheduleMissed    *prometheus.CounterVec
	runsInFlight      *prometheus.GaugeVec
}

// New builds the collectors and registers them.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),

		backupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vaultd_backup_duration_seconds",
			Help:    "How long a backup took, from probe to stored manifest.",
			Buckets: durationBuckets,
		}, []string{"target", "engine"}),

		backupBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultd_backup_bytes",
			Help: "Size of the most recent backup: compressed is what the bucket holds, plain is what the database produced.",
		}, []string{"target", "kind"}),

		backupLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultd_backup_last_success_timestamp",
			Help: "Unix time of the most recent successful backup. Alert on its age.",
		}, []string{"target"}),

		backupFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vaultd_backup_failures_total",
			Help: "Backups that failed, by the phase they died in.",
		}, []string{"target", "phase"}),

		verifyLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultd_verify_last_success_timestamp",
			Help: "Unix time of the most recent verification that passed, by level.",
		}, []string{"target", "level"}),

		retentionObjects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultd_retention_objects",
			Help: "Backups currently retained, by the tier that keeps them.",
		}, []string{"target", "tier"}),

		scheduleMissed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vaultd_schedule_missed_total",
			Help: "Scheduled runs that were skipped because the previous one was still going.",
		}, []string{"target", "kind"}),

		runsInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultd_runs_in_flight",
			Help: "Runs executing right now, by kind.",
		}, []string{"kind"}),
	}

	m.registry.MustRegister(
		m.backupDuration,
		m.backupBytes,
		m.backupLastSuccess,
		m.backupFailures,
		m.verifyLastSuccess,
		m.retentionObjects,
		m.scheduleMissed,
		m.runsInFlight,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	return m
}

// Registry exposes the underlying registry, for tests and for anything that
// needs to gather without an HTTP round trip.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler serves the exposition format at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A collector that errors should say so in the response rather than
		// take the endpoint down: a scrape failure hides every other metric.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// BackupSucceeded records one stored backup.
func (m *Metrics) BackupSucceeded(target, engine string, d time.Duration, compressed, plain int64, at time.Time) {
	m.backupDuration.WithLabelValues(target, engine).Observe(d.Seconds())
	m.backupBytes.WithLabelValues(target, KindCompressed).Set(float64(compressed))
	m.backupBytes.WithLabelValues(target, KindPlain).Set(float64(plain))
	m.backupLastSuccess.WithLabelValues(target).Set(float64(at.Unix()))
}

// BackupFailed records one failed run against the phase it died in.
//
// Nothing is observed on the duration histogram: a run that failed has no
// duration worth comparing against the ones that worked, and mixing them would
// make the p99 of a healthy target move every time an unhealthy one broke.
func (m *Metrics) BackupFailed(target, phase string) {
	m.backupFailures.WithLabelValues(target, phase).Inc()
}

// VerifySucceeded records a verification that passed.
func (m *Metrics) VerifySucceeded(target, level string, at time.Time) {
	m.verifyLastSuccess.WithLabelValues(target, level).Set(float64(at.Unix()))
}

// SeedBackup publishes what the index already knows about a target, so a
// restarted daemon does not look like a target that has never run.
func (m *Metrics) SeedBackup(target string, at time.Time, compressed, plain int64) {
	if at.IsZero() {
		return
	}
	m.backupLastSuccess.WithLabelValues(target).Set(float64(at.Unix()))
	if compressed > 0 {
		m.backupBytes.WithLabelValues(target, KindCompressed).Set(float64(compressed))
	}
	if plain > 0 {
		m.backupBytes.WithLabelValues(target, KindPlain).Set(float64(plain))
	}
}

// SeedVerify is the same for the verification timestamps.
func (m *Metrics) SeedVerify(target, level string, at time.Time) {
	if at.IsZero() {
		return
	}
	m.verifyLastSuccess.WithLabelValues(target, level).Set(float64(at.Unix()))
}

// RetentionObjects publishes how many backups each tier is keeping. Every tier
// of the target is rewritten at once, because a tier that stopped keeping
// anything has to fall to zero rather than keep its last value forever.
func (m *Metrics) RetentionObjects(target string, byTier map[string]int) {
	m.retentionObjects.DeletePartialMatch(prometheus.Labels{"target": target})
	for tier, count := range byTier {
		m.retentionObjects.WithLabelValues(target, tier).Set(float64(count))
	}
}

// ScheduleMissed records a run the scheduler skipped.
func (m *Metrics) ScheduleMissed(target, kind string) {
	m.scheduleMissed.WithLabelValues(target, kind).Inc()
}

// RunStarted and RunFinished bracket a run for the in-flight gauge.
func (m *Metrics) RunStarted(kind string)  { m.runsInFlight.WithLabelValues(kind).Inc() }
func (m *Metrics) RunFinished(kind string) { m.runsInFlight.WithLabelValues(kind).Dec() }

// buildInfo is the usual constant-1 gauge carrying the version in its labels,
// so a dashboard can tell which build produced a series.
func buildInfo() prometheus.Collector {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultd_build_info",
		Help: "Build metadata of the running binary; the value is always 1.",
	}, []string{"version", "commit", "go_version"})

	info := buildinfo.Get()
	gauge.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)
	return gauge
}
