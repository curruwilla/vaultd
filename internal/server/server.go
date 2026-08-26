// Package server is the HTTP surface of `vaultd serve`: the Prometheus
// endpoint, the health probes, the read-mostly API and the embedded UI
// (SPEC §13, §14).
//
// The probes are deliberately open and deliberately different. /healthz says
// the process is alive and is what a supervisor restarts on; /readyz says the
// config parsed and the destination answers, and is what a load balancer or a
// rolling deploy waits for. Making liveness depend on the bucket would restart
// a perfectly healthy daemon every time S3 had a bad minute.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/curruwilla/vaultd/internal/app"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/metrics"
)

const (
	// readHeaderTimeout is the slowloris guard.
	readHeaderTimeout = 10 * time.Second
	// shutdownGrace is how long in-flight requests get on the way down.
	shutdownGrace = 10 * time.Second
	// readyTimeout bounds the reachability check behind /readyz, which must
	// answer quickly or it is no use as a probe.
	readyTimeout = 5 * time.Second
	// readyCache is how long a readiness answer is reused. A probe every two
	// seconds must not become a HEAD against the bucket every two seconds.
	readyCache = 15 * time.Second
)

// Server serves the daemon's HTTP endpoints.
type Server struct {
	App     *app.App
	Metrics *metrics.Metrics
	Log     *slog.Logger
	// Status supplies what the API reports about the scheduler. It is a
	// function rather than a pointer so the server does not have to know how
	// the daemon is wired.
	Status StatusFunc

	once  sync.Once
	mux   *http.ServeMux
	ready readiness
}

// StatusFunc returns the daemon's current view of its schedule.
type StatusFunc func(ctx context.Context) Status

// Handler builds the routes, once.
func (s *Server) Handler() http.Handler {
	s.once.Do(s.routes)
	return s.mux
}

// ListenAndServe runs until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	cfg := s.App.Config().Server

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errs := make(chan error, 1)
	go func() {
		s.log().InfoContext(ctx, "http server listening",
			"address", cfg.Listen, "ui", cfg.UI, "metrics", cfg.Metrics, "auth", string(cfg.Auth.Mode))

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// The shutdown context is detached: the whole point is to finish the
	// requests already in flight, and they would be cancelled by the context
	// that just ended.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutting the http server down: %w", err)
	}
	return <-errs
}

// routes wires the endpoints.
func (s *Server) routes() {
	s.mux = http.NewServeMux()
	cfg := s.App.Config().Server

	// Probes are never authenticated: a liveness check that needs a secret is
	// a liveness check that fails when the secret is rotated.
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)

	if cfg.Metrics && s.Metrics != nil {
		s.mux.Handle("GET /metrics", s.authenticate(s.Metrics.Handler()))
	}

	s.mux.Handle("GET /api/", s.authenticate(s.api()))

	if cfg.UI {
		s.mux.Handle("GET /", s.authenticate(s.ui()))
	}
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyz reports whether this daemon could actually take a backup right now:
// the config is valid and every destination it writes to answers.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ok, detail := s.ready.check(r.Context(), s)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "not ready: %s\n", detail)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// readiness caches the answer so a probe every two seconds does not become a
// request to the bucket every two seconds.
type readiness struct {
	mu      sync.Mutex
	at      time.Time
	ok      bool
	detail  string
	checked bool
}

func (rd *readiness) check(ctx context.Context, s *Server) (bool, string) {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	if rd.checked && time.Since(rd.at) < readyCache {
		return rd.ok, rd.detail
	}

	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	rd.ok, rd.detail = s.reachable(ctx)
	rd.at = time.Now()
	rd.checked = true
	return rd.ok, rd.detail
}

// reachable heads the index object of every destination in use. It is the
// cheapest call that proves credentials, endpoint and bucket all work — and a
// missing index is a perfectly ready daemon that has not run yet, so only a
// transport failure counts against readiness.
func (s *Server) reachable(ctx context.Context) (bool, string) {
	cfg := s.App.Config()

	for i := range cfg.Targets {
		target := &cfg.Targets[i]

		store, err := s.App.Store(ctx, target.Destination)
		if err != nil {
			return false, fmt.Sprintf("destination %s: %s", target.Destination, err)
		}
		layout, err := s.App.Layout(target)
		if err != nil {
			return false, err.Error()
		}

		if _, err := store.Head(ctx, layout.Index()); err != nil && !isNotFound(err) {
			return false, fmt.Sprintf("destination %s: %s", target.Destination, err)
		}
	}
	return true, ""
}

// authenticate wraps a handler in the configured token check.
func (s *Server) authenticate(next http.Handler) http.Handler {
	auth := s.App.Config().Server.Auth
	if auth.Mode != config.AuthToken {
		return next
	}

	token := auth.Token.Reveal()
	throttle := newThrottle()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := clientIP(r)
		if wait, blocked := throttle.blocked(host); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}

		// Constant time, because a token compared byte by byte is a token an
		// attacker can guess one byte at a time.
		if subtle.ConstantTimeCompare([]byte(presented(r)), []byte(token)) != 1 {
			throttle.failed(host)
			w.Header().Set("WWW-Authenticate", `Bearer realm="vaultd"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		throttle.succeeded(host)
		next.ServeHTTP(w, r)
	})
}

// presented pulls the token out of wherever the caller put it: the header for
// an API client, the cookie for a browser that has already signed in.
func presented(r *http.Request) string {
	const prefix = "Bearer "

	if header := r.Header.Get("Authorization"); len(header) > len(prefix) && header[:len(prefix)] == prefix {
		return header[len(prefix):]
	}
	if cookie, err := r.Cookie(TokenCookie); err == nil {
		return cookie.Value
	}
	return ""
}

// TokenCookie is where the UI keeps the token once it has been entered, so a
// page reload does not ask again.
const TokenCookie = "vaultd_token"

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isNotFound(err error) bool {
	return errors.Is(err, core.ErrNotFound)
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
