package mysql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Flavor distinguishes the two forks. They speak almost the same protocol and
// almost the same client flags, and the differences are exactly where backups
// break: MariaDB has no GTID purge, and its client is a separate binary.
type Flavor string

const (
	FlavorMySQL   Flavor = "mysql"
	FlavorMariaDB Flavor = "mariadb"
)

// serverVersion is a parsed version of either fork.
type serverVersion struct {
	Raw    string
	Major  int
	Minor  int
	Patch  int
	Flavor Flavor
}

// Num renders the version the way PostgreSQL numbers its own: 8.0.46 → 80046.
func (v serverVersion) Num() int { return v.Major*10000 + v.Minor*100 + v.Patch }

// AtLeast reports whether the version is at or above the given one.
func (v serverVersion) AtLeast(major, minor, patch int) bool {
	return v.Num() >= major*10000+minor*100+patch
}

func (v serverVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

var (
	// versionRE matches a bare version anywhere in a string.
	versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)
	// distribRE matches the server version a MariaDB client was built for,
	// which is not the same number as the client's own: the 10.11 client
	// reports "Ver 10.19 Distrib 10.11.14-MariaDB".
	distribRE = regexp.MustCompile(`Distrib\s+(\d+)\.(\d+)(?:\.(\d+))?`)
)

// parseServerVersion reads `select version()` output. version_comment is the
// tiebreaker: some MariaDB builds report a plain version and only name
// themselves in the comment.
func parseServerVersion(raw, comment string) (serverVersion, error) {
	match := versionRE.FindStringSubmatch(raw)
	if match == nil {
		return serverVersion{}, fmt.Errorf("cannot read a version out of %q", raw)
	}

	flavor := FlavorMySQL
	if mentionsMariaDB(raw) || mentionsMariaDB(comment) {
		flavor = FlavorMariaDB
	}

	return serverVersion{
		Raw:    raw,
		Major:  atoi(match[1]),
		Minor:  atoi(match[2]),
		Patch:  atoi(match[3]),
		Flavor: flavor,
	}, nil
}

// parseClientVersion reads a client's --version output.
func parseClientVersion(output string) (serverVersion, error) {
	if mentionsMariaDB(output) {
		// Prefer the "Distrib" number: it is the server version this client
		// belongs to, and the one worth comparing against a server.
		if match := distribRE.FindStringSubmatch(output); match != nil {
			return serverVersion{
				Raw:    strings.TrimSpace(output),
				Major:  atoi(match[1]),
				Minor:  atoi(match[2]),
				Patch:  atoi(match[3]),
				Flavor: FlavorMariaDB,
			}, nil
		}
	}

	match := versionRE.FindStringSubmatch(output)
	if match == nil {
		return serverVersion{}, fmt.Errorf("cannot read a version out of %q", strings.TrimSpace(output))
	}

	flavor := FlavorMySQL
	if mentionsMariaDB(output) {
		flavor = FlavorMariaDB
	}
	return serverVersion{
		Raw:    strings.TrimSpace(output),
		Major:  atoi(match[1]),
		Minor:  atoi(match[2]),
		Patch:  atoi(match[3]),
		Flavor: flavor,
	}, nil
}

func mentionsMariaDB(s string) bool { return strings.Contains(strings.ToLower(s), "mariadb") }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
