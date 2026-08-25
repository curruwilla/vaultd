package mysql

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// connInfo is a parsed MySQL or MariaDB connection string.
type connInfo struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	Params   map[string]string
	// driverDSN is the go-sql-driver form, used for our own connection.
	driverDSN string
}

// parseDSN accepts both shapes people actually write: the URL form used
// everywhere else in vaultd's config, and the driver's native form.
//
//	mysql://backup:secret@db.internal:3306/app?tls=true
//	backup:secret@tcp(db.internal:3306)/app?tls=true
func parseDSN(raw string) (connInfo, error) {
	driverDSN := raw
	if isURL(raw) {
		converted, err := urlToDriverDSN(raw)
		if err != nil {
			return connInfo{}, err
		}
		driverDSN = converted
	}

	cfg, err := mysql.ParseDSN(driverDSN)
	if err != nil {
		// The driver echoes the DSN in its errors; keep it out of ours.
		return connInfo{}, errors.New("the connection string is not valid MySQL syntax")
	}
	if cfg.DBName == "" {
		return connInfo{}, errors.New("the connection string names no database")
	}

	host, port, err := splitAddr(cfg.Addr)
	if err != nil {
		return connInfo{}, err
	}

	// The client we shell out to needs a plain TCP connection; a unix socket
	// would need a different flag and is not supported yet.
	if cfg.Net != "" && cfg.Net != "tcp" {
		return connInfo{}, fmt.Errorf("connections over %s are not supported; use tcp", cfg.Net)
	}

	params := map[string]string{}
	for key, value := range cfg.Params {
		params[strings.ToLower(key)] = value
	}
	if cfg.TLSConfig != "" {
		params["tls"] = cfg.TLSConfig
	}

	return connInfo{
		Host:      host,
		Port:      port,
		User:      cfg.User,
		Password:  cfg.Passwd,
		Database:  cfg.DBName,
		Params:    params,
		driverDSN: driverDSN,
	}, nil
}

func isURL(raw string) bool {
	return strings.HasPrefix(raw, "mysql://") || strings.HasPrefix(raw, "mariadb://")
}

func urlToDriverDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("the connection string is not a valid URL")
	}

	var credentials string
	if u.User != nil {
		credentials = u.User.Username()
		if password, ok := u.User.Password(); ok {
			credentials += ":" + password
		}
		credentials += "@"
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":3306"
	}

	dsn := credentials + "tcp(" + host + ")/" + strings.TrimPrefix(u.Path, "/")
	if u.RawQuery != "" {
		dsn += "?" + u.RawQuery
	}
	return dsn, nil
}

func splitAddr(addr string) (string, int, error) {
	host, portText, found := strings.Cut(addr, ":")
	if !found {
		return addr, 3306, nil
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, fmt.Errorf("the connection string has an invalid port %q", portText)
	}
	return host, port, nil
}

// args renders the non-secret connection flags for the client binary.
func (c connInfo) args() []string {
	args := []string{
		"--host=" + c.Host,
		"--port=" + strconv.Itoa(c.Port),
		"--protocol=TCP",
	}
	if c.User != "" {
		args = append(args, "--user="+c.User)
	}
	args = append(args, c.tlsArgs()...)
	return args
}

// tlsArgs mirrors the driver's TLS setting onto the client, so the dump
// connects exactly the way the probe did.
func (c connInfo) tlsArgs() []string {
	var args []string

	switch strings.ToLower(c.Params["tls"]) {
	case "true", "required":
		args = append(args, "--ssl-mode=REQUIRED")
	case "skip-verify":
		args = append(args, "--ssl-mode=REQUIRED")
	case "preferred":
		args = append(args, "--ssl-mode=PREFERRED")
	case "false", "":
		// Leave the client's own default in place.
	default:
		args = append(args, "--ssl-mode=REQUIRED")
	}

	if ca := c.Params["ssl-ca"]; ca != "" {
		args = append(args, "--ssl-ca="+ca)
	}
	return args
}

// env carries the password. It never goes on the command line: argv is
// world-readable on Linux (SPEC §15).
func (c connInfo) env() map[string]string {
	return map[string]string{"MYSQL_PWD": c.Password}
}
