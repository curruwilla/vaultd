package server

import (
	"net/http"
)

// ui serves the single-page application (SPEC §13).
//
// The assets are compiled into the binary, so `vaultd serve` needs nothing on
// disk and the UI cannot drift out of step with the API it talks to. Until the
// UI milestone lands there is nothing to serve, and saying so plainly beats a
// blank page that looks like a broken deploy.
func (s *Server) ui() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(
			"vaultd is running.\n\n" +
				"The UI lands in milestone M8. The API is up in the meantime:\n" +
				"  /api/status   what the scheduler is doing\n" +
				"  /api/targets  the overview\n" +
				"  /api/config   the effective config, secrets redacted\n" +
				"  /metrics      Prometheus\n"))
	})
}
