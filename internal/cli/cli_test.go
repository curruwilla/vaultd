package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfig = `
version: 1
destinations:
  - name: r2
    provider: r2
    bucket: db-backups
    endpoint: https://acc.r2.cloudflarestorage.com
    access_key_id: key
    secret_access_key: s3cret-value
targets:
  - name: prod-pg
    engine: postgres
    dsn: ${PG_DSN}
    destination: r2
    schedule: "0 3 * * *"
    encryption: { mode: none }
`

// run executes the CLI in-process and reports what a user would see.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	root := NewRootCommand(&out, &errOut)
	root.SetArgs(args)

	err := root.Execute()

	code = ExitOK
	if err != nil {
		code = ExitError
		var exit *exitError
		if errors.As(err, &exit) {
			code = exit.code
		}
		if !errors.Is(err, errSilent) {
			errOut.WriteString("error: " + err.Error() + "\n")
		}
	}
	return out.String(), errOut.String(), code
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vaultd.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestValidateAcceptsAValidConfig(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://backup@pg:5432/app")
	path := writeConfig(t, validConfig)

	stdout, _, code := run(t, "validate", "-c", path)

	assert.Equal(t, ExitOK, code)
	assert.Contains(t, stdout, "is valid")
	assert.Contains(t, stdout, "1 target")
}

func TestValidateFailsOnInvalidConfig(t *testing.T) {
	path := writeConfig(t, validConfig) // ${PG_DSN} is not set

	stdout, stderr, code := run(t, "validate", "-c", path)

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "${PG_DSN} is not set")
	assert.Empty(t, stdout, "an invalid config must not print an ok line")
}

func TestValidateAllowUnsetEnv(t *testing.T) {
	path := writeConfig(t, validConfig)

	_, stderr, code := run(t, "validate", "-c", path, "--allow-unset-env")

	// The reference resolves to an empty string, which then fails a real
	// check: the target has no DSN.
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "substituted with an empty string")
	assert.Contains(t, stderr, "has no dsn")
}

func TestValidateJSON(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://backup@pg:5432/app")
	path := writeConfig(t, validConfig)

	stdout, _, code := run(t, "validate", "-c", path, "--json")

	require.Equal(t, ExitOK, code)

	var report validateReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	assert.True(t, report.OK)
	assert.Equal(t, 1, report.Summary.Targets)
	assert.Equal(t, 0, report.Summary.Errors)
	assert.NotZero(t, report.Summary.Warnings, "mode: none should warn")
}

func TestValidateReportsMissingFile(t *testing.T) {
	_, stderr, code := run(t, "validate", "-c", filepath.Join(t.TempDir(), "absent.yaml"))

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "does not exist")
}

func TestValidateReportsYAMLSyntax(t *testing.T) {
	path := writeConfig(t, "version: 1\n  targets: [")

	_, stderr, code := run(t, "validate", "-c", path)

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "is not valid YAML")
}

func TestValidatePrintEffectiveRedactsSecrets(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://backup:hunter2@pg:5432/app")
	path := writeConfig(t, validConfig)

	stdout, _, code := run(t, "validate", "-c", path, "--print-effective")

	require.Equal(t, ExitOK, code)
	assert.Contains(t, stdout, "effective config")
	assert.NotContains(t, stdout, "hunter2")
	assert.NotContains(t, stdout, "s3cret-value")
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	_, _, code := run(t, "validate", "--nope")

	assert.Equal(t, ExitUsage, code)
}

// Every command that needs a config refuses to do anything without one. For
// the daemon it matters most: a `serve` that started on a missing config would
// sit there looking healthy while backing nothing up.
func TestCommandsRefuseAMissingConfig(t *testing.T) {
	for _, args := range [][]string{
		{"serve"},
		{"run"},
		{"doctor"},
		{"backup", "prod-pg"},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, stderr, code := run(t, args...)

			assert.Equal(t, ExitError, code)
			assert.Contains(t, stderr, "does not exist")
		})
	}
}

func TestCommandsCheckTheirArguments(t *testing.T) {
	_, _, code := run(t, "backup") // needs exactly one target

	assert.Equal(t, ExitError, code)
}

func TestVersion(t *testing.T) {
	stdout, _, code := run(t, "version")

	assert.Equal(t, ExitOK, code)
	assert.Contains(t, stdout, "vaultd")
}

func TestBareInvocationShowsHelp(t *testing.T) {
	stdout, _, code := run(t)

	assert.Equal(t, ExitOK, code)
	assert.Contains(t, stdout, "Usage:")
}

func TestBackupRejectsAnUnknownTarget(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://backup@pg:5432/app")
	path := writeConfig(t, validConfig)

	_, stderr, code := run(t, "backup", "staging-pg", "-c", path)

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, `target "staging-pg" is not declared`)
	assert.Contains(t, stderr, "declared targets are prod-pg")
}

// TestBackupRefusesAnInvalidConfig matters more than it looks: a backup taken
// from a config that does not validate is exactly the kind of thing nobody
// notices until a restore.
func TestBackupRefusesAnInvalidConfig(t *testing.T) {
	path := writeConfig(t, validConfig) // ${PG_DSN} is not set

	_, stderr, code := run(t, "backup", "prod-pg", "-c", path)

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "${PG_DSN} is not set")
	assert.Contains(t, stderr, "fix it before running a backup")
}

func TestBackupNeedsATarget(t *testing.T) {
	_, _, code := run(t, "backup")

	assert.Equal(t, ExitError, code)
}

func TestListRejectsAnUnknownTarget(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://backup@pg:5432/app")
	path := writeConfig(t, validConfig)

	_, stderr, code := run(t, "list", "nope", "-c", path)

	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, `target "nope" is not declared`)
}
