package config

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// redacted is what a Secret renders as anywhere outside Reveal.
const redacted = "***"

// Secret is a string that never prints itself. Every rendering path a secret
// can reach — fmt, slog, JSON, YAML — is overridden, so leaking one takes an
// explicit Reveal call that is greppable in review (SPEC §15).
type Secret string

// Reveal returns the underlying value. Call it only when handing the secret to
// the process that needs it (a driver, an HTTP header, an exec argument).
func (s Secret) Reveal() string { return string(s) }

// Set returns true when the secret carries a value.
func (s Secret) Set() bool { return s != "" }

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

func (s Secret) GoString() string { return strconv.Quote(s.String()) }

func (s Secret) MarshalYAML() (any, error) { return s.String(), nil }

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// RedactDSN masks the credentials inside a connection string while keeping it
// recognizable: user, password and any password-ish query parameter go away,
// host/port/database stay. A string that does not parse as a URL is redacted
// whole rather than risking a leak.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}

	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return redactKeyValueDSN(dsn)
	}

	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), redacted)
		}
	}

	if q := u.Query(); len(q) > 0 {
		changed := false
		for key := range q {
			if isSecretKey(key) {
				q.Set(key, redacted)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}

	// url.String escapes the asterisks in the redaction token; put them back
	// so a redacted DSN still reads as one in a log line.
	return strings.ReplaceAll(u.String(), url.QueryEscape(redacted), redacted)
}

// redactKeyValueDSN handles libpq/MySQL style "key=value key=value" strings.
func redactKeyValueDSN(dsn string) string {
	fields := strings.Fields(dsn)
	if len(fields) == 0 {
		return redacted
	}

	out := make([]string, 0, len(fields))
	for _, field := range fields {
		key, _, found := strings.Cut(field, "=")
		if !found {
			return redacted // not a key=value DSN; do not guess
		}
		if isSecretKey(key) {
			field = key + "=" + redacted
		}
		out = append(out, field)
	}
	return strings.Join(out, " ")
}

func isSecretKey(key string) bool {
	switch strings.ToLower(key) {
	case "password", "passwd", "pwd", "secret", "token", "access_key", "secret_key", "sslpassword":
		return true
	}
	return false
}
