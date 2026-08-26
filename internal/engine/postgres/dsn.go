package postgres

import (
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// connInfo is a parsed PostgreSQL connection string, split into the parts a
// client binary needs.
type connInfo struct {
	Host     string
	Port     uint16
	User     string
	Password string
	Database string
	// TLS and timeout settings, carried through verbatim from the DSN.
	Params map[string]string
	// Raw is the original string, used for the pgx connection.
	Raw string
}

// sslParams are the DSN keys that must reach the client binary for TLS to
// behave the same way there as it does for our own connection.
var sslParams = map[string]string{
	"sslmode":         "PGSSLMODE",
	"sslrootcert":     "PGSSLROOTCERT",
	"sslcert":         "PGSSLCERT",
	"sslkey":          "PGSSLKEY",
	"sslpassword":     "PGSSLPASSWORD",
	"connect_timeout": "PGCONNECT_TIMEOUT",
}

// parseDSN validates a connection string and splits it up. Parsing goes
// through pgx, so anything the driver accepts — URLs, libpq key=value strings,
// service files — is accepted here too.
func parseDSN(raw string) (connInfo, error) {
	cfg, err := pgconn.ParseConfig(raw)
	if err != nil {
		// The error text from pgx can contain the DSN; do not propagate it.
		return connInfo{}, errors.New("the connection string is not valid PostgreSQL syntax")
	}

	info := connInfo{
		Host:     cfg.Host,
		Port:     cfg.Port,
		User:     cfg.User,
		Password: cfg.Password,
		Database: cfg.Database,
		Params:   extractParams(raw),
		Raw:      raw,
	}
	if info.Host == "" {
		return connInfo{}, errors.New("the connection string names no host")
	}
	if info.Database == "" {
		return connInfo{}, errors.New("the connection string names no database")
	}
	return info, nil
}

// env renders the connection as environment variables for a client binary.
// Nothing goes on the command line: argv is world-readable on Linux, and a
// password in it would be visible to every process on the host (SPEC §15).
func (c connInfo) env() map[string]string {
	vars := map[string]string{
		"PGHOST":     c.Host,
		"PGPORT":     strconv.Itoa(int(c.Port)),
		"PGUSER":     c.User,
		"PGPASSWORD": c.Password,
		"PGDATABASE": c.Database,
		"PGAPPNAME":  "vaultd",
	}
	for key, value := range c.Params {
		if envName, ok := sslParams[key]; ok {
			vars[envName] = value
		}
	}
	return vars
}

// withDatabase returns the same connection pointed at another database, which
// is what pg_dumpall needs to read cluster-wide objects.
func (c connInfo) withDatabase(name string) connInfo {
	c.Database = name
	return c
}

// extractParams pulls the TLS and timeout settings out of a DSN in either
// supported shape.
func extractParams(raw string) map[string]string {
	params := map[string]string{}

	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		u, err := url.Parse(raw)
		if err != nil {
			return params
		}
		for key, values := range u.Query() {
			if len(values) > 0 {
				params[strings.ToLower(key)] = values[0]
			}
		}
		return params
	}

	for _, field := range strings.Fields(raw) {
		key, value, found := strings.Cut(field, "=")
		if found {
			params[strings.ToLower(key)] = strings.Trim(value, `'"`)
		}
	}
	return params
}

// reservedKeywords are the connection settings withDatabaseDSN writes itself.
// Everything else the original DSN carried — TLS, timeouts, application
// settings — rides along untouched.
var reservedKeywords = map[string]bool{
	"host": true, "hostaddr": true, "port": true,
	"user": true, "password": true, "dbname": true,
}

// withDatabaseDSN renders this connection pointed at another database, in
// libpq's keyword/value syntax.
//
// That syntax rather than a URL because it needs no percent-encoding: a
// password holding a '/' or a '@' survives it unchanged, and the verify
// target's credentials are not ours to re-encode.
func (c connInfo) withDatabaseDSN(database string) string {
	pairs := []string{
		"host=" + quoteDSNValue(c.Host),
		"port=" + quoteDSNValue(strconv.Itoa(int(c.Port))),
		"dbname=" + quoteDSNValue(database),
	}
	if c.User != "" {
		pairs = append(pairs, "user="+quoteDSNValue(c.User))
	}
	if c.Password != "" {
		pairs = append(pairs, "password="+quoteDSNValue(c.Password))
	}

	extra := make([]string, 0, len(c.Params))
	for key := range c.Params {
		if !reservedKeywords[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		pairs = append(pairs, key+"="+quoteDSNValue(c.Params[key]))
	}

	return strings.Join(pairs, " ")
}

// quoteDSNValue renders one value the way libpq reads it back: single-quoted,
// with backslashes and quotes escaped.
func quoteDSNValue(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}
