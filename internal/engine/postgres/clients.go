package postgres

import (
	"context"
	"fmt"

	"github.com/curruwilla/vaultd/internal/engine"
)

// Clients are the PostgreSQL binaries vaultd shells out to: the dump, the
// cluster-wide dump behind include_globals, and the restore.
var Clients = []string{"pg_dump", "pg_dumpall", "pg_restore"}

// The majors doctor sweeps when reporting what is installed. It starts below
// every supported server and runs well past the current release, because the
// interesting answer is "you have 15 and 16, your server is 17".
const (
	minSearchMajor = 12
	maxSearchMajor = 30
)

// SearchDirs lists the version-specific directories distributions install
// PostgreSQL clients into. Nothing is stat'ed here; the caller does that.
func SearchDirs() []string {
	dirs := make([]string, 0, (maxSearchMajor-minSearchMajor+1)*len(searchTemplates))
	for major := maxSearchMajor; major >= minSearchMajor; major-- {
		for _, template := range searchTemplates {
			dirs = append(dirs, fmt.Sprintf(template, major))
		}
	}
	return dirs
}

// FindClients reports every installed copy of one client, newest major first.
func FindClients(ctx context.Context, name string) []engine.Binary {
	return engine.Scan(ctx, name, SearchDirs())
}
