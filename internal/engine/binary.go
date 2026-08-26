package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// versionRE matches the version in a client's --version output, whatever
// vendor decoration surrounds it:
//
//	pg_dump (PostgreSQL) 17.2 (Debian 17.2-1.pgdg120+1)
//	mysqldump  Ver 8.0.39 for Linux on x86_64
var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// Binary is a resolved client executable.
type Binary struct {
	Name    string
	Path    string
	Version string
	Major   int
}

// String renders the binary for a manifest: "pg_dump 17.2".
func (b Binary) String() string { return b.Name + " " + b.Version }

// ParseVersion extracts the version and major number from --version output.
func ParseVersion(output string) (version string, major int, err error) {
	match := versionRE.FindStringSubmatch(output)
	if match == nil {
		return "", 0, fmt.Errorf("cannot read a version out of %q", strings.TrimSpace(output))
	}

	major, err = strconv.Atoi(match[1])
	if err != nil {
		return "", 0, fmt.Errorf("cannot read a version out of %q", strings.TrimSpace(output))
	}
	return match[0], major, nil
}

// ProbeBinary runs `<path> --version` and reports what it is. The timeout is
// short: a client binary that does not answer in seconds is broken, and the
// caller is usually trying several candidates.
func ProbeBinary(ctx context.Context, name, path string) (Binary, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return Binary{}, fmt.Errorf("running %s --version: %w", path, err)
	}

	version, major, err := ParseVersion(string(out))
	if err != nil {
		return Binary{}, fmt.Errorf("%s: %w", path, err)
	}
	return Binary{Name: name, Path: path, Version: version, Major: major}, nil
}

// Scan reports every usable copy of a client binary: the versioned
// directories first, then whatever PATH resolves to, deduplicated by path.
//
// It is what `vaultd doctor` reports and what the error messages promise —
// "found none installed" has to be a statement about the whole search, not
// about one directory.
func Scan(ctx context.Context, name string, dirs []string) []Binary {
	var (
		found []Binary
		seen  = map[string]bool{}
	)

	candidates := make([]string, 0, len(dirs)+1)
	for _, dir := range dirs {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	if path, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, path)
	}

	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true

		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		binary, err := ProbeBinary(ctx, name, candidate)
		if err != nil {
			continue
		}
		found = append(found, binary)
	}
	return found
}
