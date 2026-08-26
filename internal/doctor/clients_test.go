package doctor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/curruwilla/vaultd/internal/engine"
)

// The hint an operator acts on differs by image: a slim build is missing the
// client because of what it is, and the fix is a different image. Getting this
// backwards would tell somebody running the fat image to switch to the fat
// image (decision D1).
func TestSlimImagesAreTheOnesThatCanBeMissingAnEngine(t *testing.T) {
	t.Parallel()

	for variant, isSlim := range map[string]bool{
		"pg17":       true,
		"mysql8":     true,
		"mariadb11":  true,
		"mongo7":     true,
		"all":        false,
		"latest":     false,
		"standalone": false,
		"":           false,
	} {
		assert.Equal(t, isSlim, slim(variant), variant)
	}
}

// A distribution wrapper on PATH and the versioned binary it dispatches to are
// two paths to one client. Reporting "18.6, 18.6" reads as a bug in vaultd.
func TestVersionsAreListedOnce(t *testing.T) {
	t.Parallel()

	found := []engine.Binary{
		{Name: "pg_dump", Path: "/usr/lib/postgresql/18/bin/pg_dump", Version: "18.6"},
		{Name: "pg_dump", Path: "/usr/lib/postgresql/17/bin/pg_dump", Version: "17.11"},
		{Name: "pg_dump", Path: "/usr/bin/pg_dump", Version: "18.6"},
	}

	assert.Equal(t, []string{"18.6", "17.11"}, versionsOf(found))
}
