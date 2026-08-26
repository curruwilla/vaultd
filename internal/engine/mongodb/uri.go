package mongodb

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// connInfo is a parsed MongoDB connection URI.
type connInfo struct {
	// Raw is the URI as configured; it is what both the driver and mongodump
	// connect with, and it is never written to disk or to a command line.
	Raw      string
	Hosts    string
	User     string
	Password string
	// Database is the one named in the URI path, empty when the URI names none
	// (which means "the whole deployment").
	Database string
}

// credentialsRE matches the userinfo of a connection URI, wherever it appears
// in a line of client output.
var credentialsRE = regexp.MustCompile(`(mongodb(?:\+srv)?://)[^@/\s]*:[^@/\s]*@`)

func parseURI(raw string) (connInfo, error) {
	if !strings.HasPrefix(raw, "mongodb://") && !strings.HasPrefix(raw, "mongodb+srv://") {
		return connInfo{}, errors.New("the URI must start with mongodb:// or mongodb+srv://")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return connInfo{}, errors.New("the URI is not valid MongoDB syntax")
	}

	info := connInfo{Raw: raw, Hosts: u.Host, Database: strings.TrimPrefix(u.Path, "/")}
	if u.User != nil {
		info.User = u.User.Username()
		info.Password, _ = u.User.Password()
	}
	if info.Hosts == "" {
		return connInfo{}, errors.New("the URI names no host")
	}
	return info, nil
}

// redact removes the credentials from client output. mongodump echoes the URI
// it was given in its error messages, password included, and that output ends
// up in manifests and webhooks (SPEC §15).
func (c connInfo) redact(s string) string {
	if s == "" {
		return s
	}
	if c.Password != "" {
		s = strings.ReplaceAll(s, c.Password, "***")
	}
	return credentialsRE.ReplaceAllString(s, "${1}***:***@")
}

// withDatabase returns the same URI pointed at another database, or at none
// when name is empty.
//
// The authentication database travels with it: MongoDB defaults authSource to
// whatever the URI's path names, so moving the path without saying so would
// authenticate against a database that does not exist.
func (c connInfo) withDatabase(name string) (connInfo, error) {
	u, err := url.Parse(c.Raw)
	if err != nil {
		return connInfo{}, errors.New("the URI is not valid MongoDB syntax")
	}

	if u.User != nil {
		query := u.Query()
		if query.Get("authSource") == "" {
			source := c.Database
			if source == "" {
				source = "admin"
			}
			query.Set("authSource", source)
			u.RawQuery = query.Encode()
		}
	}
	u.Path = "/" + name

	updated := c
	updated.Raw = u.String()
	updated.Database = name
	return updated, nil
}
