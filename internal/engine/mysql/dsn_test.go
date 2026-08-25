package mysql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDSNURLForm(t *testing.T) {
	info, err := parseDSN("mysql://backup:hunter2@db.internal:3307/app?tls=true")
	require.NoError(t, err)

	assert.Equal(t, "db.internal", info.Host)
	assert.Equal(t, 3307, info.Port)
	assert.Equal(t, "backup", info.User)
	assert.Equal(t, "hunter2", info.Password)
	assert.Equal(t, "app", info.Database)
	assert.Contains(t, info.args(), "--ssl-mode=REQUIRED")
}

func TestParseDSNDriverForm(t *testing.T) {
	info, err := parseDSN("backup:hunter2@tcp(db.internal:3306)/app")
	require.NoError(t, err)

	assert.Equal(t, "db.internal", info.Host)
	assert.Equal(t, 3306, info.Port)
	assert.Equal(t, "app", info.Database)
}

func TestParseDSNDefaultsThePort(t *testing.T) {
	info, err := parseDSN("mysql://backup@db.internal/app")
	require.NoError(t, err)

	assert.Equal(t, 3306, info.Port)
}

func TestParseDSNRequiresADatabase(t *testing.T) {
	_, err := parseDSN("mysql://backup@db.internal:3306/")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no database")
}

func TestParseDSNRejectsUnixSockets(t *testing.T) {
	_, err := parseDSN("backup@unix(/var/run/mysqld/mysqld.sock)/app")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "use tcp")
}

func TestParseDSNKeepsTheStringOutOfErrors(t *testing.T) {
	_, err := parseDSN("backup:hunter2@nonsense")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestTLSMapping(t *testing.T) {
	tests := map[string]string{
		"mysql://u@h:3306/d?tls=true":        "--ssl-mode=REQUIRED",
		"mysql://u@h:3306/d?tls=preferred":   "--ssl-mode=PREFERRED",
		"mysql://u@h:3306/d?tls=skip-verify": "--ssl-mode=REQUIRED",
	}

	for dsn, want := range tests {
		t.Run(dsn, func(t *testing.T) {
			info, err := parseDSN(dsn)
			require.NoError(t, err)

			assert.Contains(t, info.args(), want)
		})
	}
}

func TestTLSDisabledLeavesTheClientDefault(t *testing.T) {
	info, err := parseDSN("mysql://u@h:3306/d?tls=false")
	require.NoError(t, err)

	for _, arg := range info.args() {
		assert.NotContains(t, arg, "--ssl-mode")
	}
}
