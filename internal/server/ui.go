package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/curruwilla/vaultd/web"
)

// ui serves the single-page application (SPEC §13).
//
// Every route that is not an asset returns index.html, because the SPA routes
// on the fragment and a reload of #/t/prod-pg must not 404. The assets are in
// the binary, so there is nothing to mount, nothing to keep in step with a
// deployment, and nothing loaded from a CDN.
func (s *Server) ui() http.Handler {
	assets := http.FileServerFS(web.Assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		if name == "" || name == "." {
			s.index(w, r)
			return
		}
		if _, err := fs.Stat(web.Assets, name); err != nil {
			// Not an asset and not the root: an old bookmark, or a probe.
			// The SPA is the useful answer either way.
			s.index(w, r)
			return
		}

		// The assets are versioned with the binary, not by name, so they are
		// revalidated rather than cached: an upgraded daemon must not be shown
		// through the previous build's JavaScript.
		w.Header().Set("Cache-Control", "no-cache")
		assets.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	body, err := web.Assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "the UI is not built into this binary", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// The page loads only what it ships with, talks only to its own origin,
	// and refuses to be framed. It has no inline script, so the policy can be
	// this tight without unsafe-inline.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	http.ServeContent(w, r, "index.html", buildTime(), strings.NewReader(string(body)))
}
