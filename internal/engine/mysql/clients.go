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

// FindClients reports every installed copy of one client. MySQL clients are
// not installed per version the way PostgreSQL's are, so this is PATH.
func FindClients(ctx context.Context, name string) []engine.Binary {
	return engine.Scan(ctx, name, nil)
}
