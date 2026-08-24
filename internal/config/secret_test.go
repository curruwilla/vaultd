package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/config"
)

// TestSecretNeverPrints checks every rendering path a Secret can reach. A leak
// in any one of them puts a production DSN in a log line or a webhook body.
func TestSecretNeverPrints(t *testing.T) {
	const value = "hunter2-do-not-log"
	s := config.Secret(value)

	assert.Equal(t, value, s.Reveal())
	assert.NotContains(t, fmt.Sprintf("%v %s %q %#v", s, s, s, s), value)

	encoded, err := json.Marshal(struct {
		Token config.Secret `json:"token"`
	}{s})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), value)

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("connecting", "dsn", s)
	assert.NotContains(t, buf.String(), value)
}

func TestSecretEmptyStaysEmpty(t *testing.T) {
	assert.Empty(t, config.Secret("").String())
	assert.False(t, config.Secret("").Set())
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "url credentials",
			dsn:  "postgres://backup:hunter2@pg.internal:5432/app?sslmode=require",
			want: "postgres://backup:***@pg.internal:5432/app?sslmode=require",
		},
		{
			name: "no password",
			dsn:  "postgres://backup@pg.internal:5432/app",
			want: "postgres://backup@pg.internal:5432/app",
		},
		{
			name: "password in query",
			dsn:  "mongodb://mongo:27017/?replicaSet=rs0&password=hunter2",
			want: "mongodb://mongo:27017/?password=***&replicaSet=rs0",
		},
		{
			name: "libpq key value",
			dsn:  "host=pg.internal port=5432 user=backup password=hunter2 dbname=app",
			want: "host=pg.internal port=5432 user=backup password=*** dbname=app",
		},
		{
			name: "unrecognized shape is redacted whole",
			dsn:  "hunter2",
			want: "***",
		},
		{
			name: "empty stays empty",
			dsn:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.RedactDSN(tt.dsn)

			assert.Equal(t, tt.want, got)
			if tt.dsn != "" {
				assert.NotContains(t, got, "hunter2")
			}
		})
	}
}
