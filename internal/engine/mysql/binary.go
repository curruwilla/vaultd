package mysql

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/curruwilla/vaultd/internal/engine"
)

// resolveClient finds a dump client that belongs to the same fork as the
// server.
//
// Unlike PostgreSQL, where an older pg_dump cannot represent a newer server's
// archive and the mismatch has to be fatal (SPEC §3), MySQL and MariaDB emit
// portable SQL text: an older client usually works and only risks missing
// newer syntax, so a version gap is a warning. Dumping MariaDB with Oracle's
// client is a different matter — the flags and the output diverge — so a
// flavor mismatch is refused.
func (d *Dumper) resolveClient(ctx context.Context, server serverVersion) (engine.Binary, string, error) {
	var mismatched []string

	for _, name := range clientNames(server.Flavor) {
		for _, path := range d.candidatePaths(name) {
			binary, version, err := probeClient(ctx, name, path)
			if err != nil {
				continue
			}

			if version.Flavor != server.Flavor {
				mismatched = append(mismatched, fmt.Sprintf("%s is %s's (%s)", path, version.Flavor, version))
				continue
			}

			var warning string
			if version.Major < server.Major {
				warning = fmt.Sprintf("%s is version %s, older than the %s %s server; it may not understand every construct in this database",
					binary.Name, version, server.Flavor, server)
			}
			return binary, warning, nil
		}
	}

	return engine.Binary{}, "", clientError(server, mismatched)
}

// candidatePaths lists where to look for one client name, best first.
func (d *Dumper) candidatePaths(name string) []string {
	if d.opts.BinDir != "" {
		return []string{filepath.Join(d.opts.BinDir, name)}
	}

	path, err := exec.LookPath(name)
	if err != nil {
		return nil
	}
	return []string{path}
}

func probeClient(ctx context.Context, name, path string) (engine.Binary, serverVersion, error) {
	info, err := os.Stat(path)
	if err != nil {
		return engine.Binary{}, serverVersion{}, err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return engine.Binary{}, serverVersion{}, fmt.Errorf("%s is not executable", path)
	}

	out, err := exec.CommandContext(ctx, path, "--version").Output() //nolint:gosec // path comes from our own lookup
	if err != nil {
		return engine.Binary{}, serverVersion{}, fmt.Errorf("running %s --version: %w", path, err)
	}

	version, err := parseClientVersion(string(out))
	if err != nil {
		return engine.Binary{}, serverVersion{}, err
	}

	return engine.Binary{
		Name:    name,
		Path:    path,
		Version: version.String(),
		Major:   version.Major,
	}, version, nil
}

func clientError(server serverVersion, mismatched []string) error {
	packageHint := "mysql-client"
	if server.Flavor == FlavorMariaDB {
		packageHint = "mariadb-client"
	}

	if len(mismatched) > 0 {
		return fmt.Errorf("server is %s %s, but no %s client was found: %s (install %s, or run the vaultd container image)",
			server.Flavor, server, server.Flavor, strings.Join(mismatched, "; "), packageHint)
	}
	return fmt.Errorf("server is %s %s, need %s, found none installed (install %s, or run the vaultd container image)",
		server.Flavor, server, strings.Join(clientNames(server.Flavor), " or "), packageHint)
}
