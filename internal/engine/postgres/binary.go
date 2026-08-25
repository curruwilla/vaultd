package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/curruwilla/vaultd/internal/engine"
)

// searchTemplates are where distributions keep versioned PostgreSQL clients.
// %d is the major version. They are tried before PATH, because a host with
// several server versions installed usually has the wrong one first on PATH.
var searchTemplates = []string{
	"/usr/lib/postgresql/%d/bin",          // Debian, Ubuntu, PGDG
	"/usr/pgsql-%d/bin",                   // RHEL, Rocky, PGDG
	"/opt/homebrew/opt/postgresql@%d/bin", // macOS, Apple silicon
	"/usr/local/opt/postgresql@%d/bin",    // macOS, Intel
}

// majorSearchRange is how far above the server's major version to look. A
// newer client can always dump an older server; the reverse cannot be trusted.
const majorSearchRange = 5

// resolveBinary finds a client of at least serverMajor.
//
// The rule is pg_dump >= server (SPEC §3): a 16 client cannot represent
// everything a 17 server holds, and the failure would surface at restore time,
// which is the worst possible moment to discover it.
func (d *Dumper) resolveBinary(ctx context.Context, name string, serverMajor int) (engine.Binary, error) {
	var found []engine.Binary

	if d.opts.BinDir != "" {
		binary, err := probeCandidate(ctx, name, filepath.Join(d.opts.BinDir, name))
		if err != nil {
			return engine.Binary{}, fmt.Errorf("bin_dir %s: %w", d.opts.BinDir, err)
		}
		if binary.Major < serverMajor {
			return engine.Binary{}, versionError(name, serverMajor, []engine.Binary{binary})
		}
		return binary, nil
	}

	// Exact major first, then newer ones.
	for major := serverMajor; major <= serverMajor+majorSearchRange; major++ {
		for _, template := range searchTemplates {
			candidate := filepath.Join(fmt.Sprintf(template, major), name)
			binary, err := probeCandidate(ctx, name, candidate)
			if err != nil {
				continue
			}
			if binary.Major >= serverMajor {
				return binary, nil
			}
			found = append(found, binary)
		}
	}

	if path, err := exec.LookPath(name); err == nil {
		if binary, err := probeCandidate(ctx, name, path); err == nil {
			if binary.Major >= serverMajor {
				return binary, nil
			}
			found = append(found, binary)
		}
	}

	return engine.Binary{}, versionError(name, serverMajor, found)
}

func probeCandidate(ctx context.Context, name, path string) (engine.Binary, error) {
	info, err := os.Stat(path)
	if err != nil {
		return engine.Binary{}, err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return engine.Binary{}, fmt.Errorf("%s is not executable", path)
	}
	return engine.ProbeBinary(ctx, name, path)
}

// versionError explains what is missing in the terms an operator can act on:
// which server, which client version is needed, and what was actually found.
func versionError(name string, serverMajor int, found []engine.Binary) error {
	if len(found) == 0 {
		return fmt.Errorf("server is PG %d, need %s >= %d, found none installed "+
			"(install postgresql-client-%d, or run the vaultd container image)",
			serverMajor, name, serverMajor, serverMajor)
	}

	versions := make([]string, 0, len(found))
	for _, binary := range found {
		versions = append(versions, binary.Version)
	}
	return fmt.Errorf("server is PG %d, need %s >= %d, found %s",
		serverMajor, name, serverMajor, strings.Join(versions, ", "))
}
