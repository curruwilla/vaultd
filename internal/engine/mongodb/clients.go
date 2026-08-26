package mongodb

import (
	"context"

	"github.com/curruwilla/vaultd/internal/engine"
)

// Clients are the MongoDB database tools vaultd shells out to. They are
// versioned independently of the server (100.x), so there is no version rule
// to check — only presence.
var Clients = []string{"mongodump", "mongorestore"}

// FindClients reports every installed copy of one client.
func FindClients(ctx context.Context, name string) []engine.Binary {
	return engine.Scan(ctx, name, nil)
}
