package mysql

import (
	"context"

	"github.com/curruwilla/vaultd/internal/engine"
)

// Clients are the binaries a target of this fork needs: the dump client, and
// the interactive one a restore feeds the dump back through.
func Clients(flavor Flavor) []string {
	if flavor == FlavorMariaDB {
		return []string{"mariadb-dump", "mariadb"}
	}
	return []string{"mysqldump", "mysql"}
}

// Client is an installed client, with the fork it actually belongs to.
//
// The fork is the point. MariaDB installs `mysqldump` as a compatibility
// symlink onto its own `mariadb-dump`, so "is mysqldump installed" is not the
// question worth answering: a host can have the name and not the client, and
// the mismatch would only surface when a dump aborts partway through.
type Client struct {
	engine.Binary
	Flavor Flavor
}

// FindClients reports every installed copy of one client name, with the fork
// each one turns out to be. MySQL clients are not installed per version the
// way PostgreSQL's are, so the search is PATH.
func FindClients(ctx context.Context, name string) []Client {
	found := engine.Scan(ctx, name, nil)

	out := make([]Client, 0, len(found))
	for _, binary := range found {
		_, version, err := probeClient(ctx, binary.Name, binary.Path)
		if err != nil {
			continue
		}
		out = append(out, Client{Binary: binary, Flavor: version.Flavor})
	}
	return out
}
