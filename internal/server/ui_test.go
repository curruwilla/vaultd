package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shell has to load before it can ask for a token, so it is not behind
// one. It carries no data: everything a viewer would care about comes from
// /api, which is guarded.
func TestTheUIShellLoadsWithoutAToken(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	for _, path := range []string{"/", "/app.js", "/style.css"} {
		recorder := get(t, s, path, "")
		assert.Equal(t, http.StatusOK, recorder.Code, path)
		assert.NotEmpty(t, recorder.Body.String(), path)
	}
}

// The SPA routes on the fragment, but a reload of a deep link still has to
// come back with the app rather than a 404.
func TestDeepLinksServeTheApp(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)

	recorder := get(t, s, "/t/prod-pg", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "<title>vaultd</title>")
}

// The page loads only what it ships with and talks only to its own origin.
func TestTheShellCarriesItsContentSecurityPolicy(t *testing.T) {
	t.Parallel()

	s, _ := newServer(t)
	recorder := get(t, s, "/", "")

	policy := recorder.Header().Get("Content-Security-Policy")
	assert.Contains(t, policy, "default-src 'none'")
	assert.Contains(t, policy, "script-src 'self'")
	assert.Contains(t, policy, "frame-ancestors 'none'")
	assert.NotContains(t, policy, "unsafe-inline")
}
