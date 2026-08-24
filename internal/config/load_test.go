package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/curruwilla/vaultd/internal/config"
)

// baseYAML is the smallest config that validates. Tests append or replace
// fragments of it so each case shows only what it is about.
const baseYAML = `
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
    dsn: postgres://backup@pg:5432/app
    destination: r2
    schedule: "0 3 * * *"
    encryption: { mode: none }
`

// withTarget rebuilds baseYAML with extra lines appended to its single target.
func withTarget(extra string) string {
	return baseYAML + indent(extra, "    ")
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.Trim(s, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

// parse runs the full pipeline the CLI runs, with the environment stubbed out.
func parse(t *testing.T, yaml string) (*config.Config, config.Diagnostics) {
	t.Helper()

	cfg, diags, err := config.Parse([]byte(yaml), config.LoadOptions{
		Lookup: func(string) (string, bool) { return "", false },
	})
	require.NoError(t, err, "config should be syntactically valid YAML")
	return cfg, diags
}

func TestParseValidConfig(t *testing.T) {
	cfg, diags := parse(t, baseYAML)

	assert.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
	require.Len(t, cfg.Targets, 1)
	assert.Equal(t, "prod-pg", cfg.Targets[0].Name)
	// mode: none and an absent retention policy are legal but never silent.
	assert.Equal(t, 2, diags.Count(config.SeverityWarn))
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing version",
			yaml: strings.Replace(baseYAML, "version: 1", "", 1),
			want: "config has no version",
		},
		{
			name: "future version",
			yaml: strings.Replace(baseYAML, "version: 1", "version: 2", 1),
			want: "unsupported config version 2",
		},
		{
			name: "undeclared encryption (D5)",
			yaml: strings.Replace(baseYAML, "    encryption: { mode: none }", "", 1),
			want: "has no encryption; set encryption.recipients or opt out with encryption.mode=none",
		},
		{
			name: "age mode without recipients",
			yaml: strings.Replace(baseYAML, "encryption: { mode: none }", "encryption: { mode: age }", 1),
			want: "encrypts with age but lists no recipients",
		},
		{
			name: "unparseable age recipient",
			yaml: strings.Replace(baseYAML, "encryption: { mode: none }", `encryption: { mode: age, recipients: ["age1nope"] }`, 1),
			want: "unparseable age recipient",
		},
		{
			name: "unknown destination",
			yaml: strings.Replace(baseYAML, "destination: r2", "destination: nowhere", 1),
			want: `writes to destination "nowhere", which is not declared`,
		},
		{
			name: "invalid cron",
			yaml: strings.Replace(baseYAML, `schedule: "0 3 * * *"`, `schedule: "0 3 * *"`, 1),
			want: "invalid schedule",
		},
		{
			name: "invalid engine",
			yaml: strings.Replace(baseYAML, "engine: postgres", "engine: cassandra", 1),
			want: `has engine "cassandra"`,
		},
		{
			name: "mongo option on postgres target",
			yaml: withTarget("options: { oplog: true }"),
			want: "sets oplog, which only applies to mongodb",
		},
		{
			name: "postgres option on mysql target",
			yaml: strings.Replace(withTarget(`options: { exclude_table_data: ["a.b"] }`),
				"engine: postgres", "engine: mysql", 1),
			want: "sets postgres-only options",
		},
		{
			name: "mongo target using dsn",
			yaml: strings.Replace(baseYAML, "engine: postgres", "engine: mongodb", 1),
			want: "(mongodb) sets dsn; use uri instead",
		},
		{
			name: "unknown notifier reference",
			yaml: withTarget("notify: [pager]"),
			want: `notifies "pager", which is not declared under notifiers`,
		},
		{
			name: "restore verify without a target",
			yaml: withTarget("verify:\n  level: restore"),
			want: "verifies at level restore but has no `into`",
		},
		{
			name: "zstd level out of range",
			yaml: withTarget("compression: { algo: zstd, level: 42 }"),
			want: "zstd levels run from 1 to 19",
		},
		{
			name: "retention that keeps nothing",
			yaml: withTarget("retention: { daily: { keep: 0 } }"),
			want: "keeps nothing; prune would delete every backup",
		},
		{
			name: "monthly day that some months lack",
			yaml: withTarget("retention: { monthly: { keep: 6, on: 31 } }"),
			want: "use 1 to 28 so every month has that day",
		},
		{
			name: "unresolvable variable",
			yaml: strings.Replace(baseYAML, "dsn: postgres://backup@pg:5432/app", "dsn: ${MISSING_DSN}", 1),
			want: "${MISSING_DSN} is not set in the environment",
		},
		{
			name: "duplicate target name",
			yaml: baseYAML + `  - name: prod-pg
    engine: mysql
    dsn: mysql://backup@mysql:3306/app
    destination: r2
    encryption: { mode: none }
`,
			want: `duplicate target name "prod-pg"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := parse(t, tt.yaml)

			require.True(t, diags.HasErrors(), "expected an error, got: %v", diags)
			assert.Contains(t, render(diags), tt.want)
		})
	}
}

func TestParseWarns(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no schedule",
			yaml: strings.Replace(baseYAML, `    schedule: "0 3 * * *"`, "", 1),
			want: "will only run when invoked manually",
		},
		{
			name: "no retention",
			yaml: baseYAML,
			want: "every backup is kept forever",
		},
		{
			name: "storage class on r2",
			yaml: strings.Replace(baseYAML, "    bucket: db-backups", "    bucket: db-backups\n    storage_class: GLACIER", 1),
			want: "storage_class is ignored on r2",
		},
		{
			name: "unencrypted upload",
			yaml: baseYAML,
			want: "uploads its dump unencrypted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diags := parse(t, tt.yaml)

			assert.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
			assert.Contains(t, render(diags), tt.want)
		})
	}
}

// TestParseRejectsUnknownKeys guards the strict decoder: a typo that silently
// does nothing is the worst outcome for a backup tool.
func TestParseRejectsUnknownKeys(t *testing.T) {
	_, _, err := config.Parse([]byte(baseYAML+"    compression_level: 3\n"), config.LoadOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compression_level")
}

func TestParseRejectsInvalidYAML(t *testing.T) {
	_, _, err := config.Parse([]byte("version: 1\n  targets: ["), config.LoadOptions{})

	require.Error(t, err)
	assert.NotEmpty(t, config.FormatError(err))
}

func TestLoadMissingFile(t *testing.T) {
	_, _, err := config.Load(t.TempDir()+"/absent.yaml", config.LoadOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// TestVerifyTargetEngineMustMatch covers the cross-reference that decides
// whether an L2 restore can even run (SPEC §8).
func TestVerifyTargetEngineMustMatch(t *testing.T) {
	yaml := withTarget("verify:\n  level: restore\n  into: staging") + `
verify_targets:
  - name: staging
    engine: mysql
    dsn: mysql://root@staging:3306/
    database_prefix: vaultd_verify_
`

	_, diags := parse(t, yaml)

	require.True(t, diags.HasErrors())
	assert.Contains(t, render(diags), "engines must match")
}

func TestVerifyTargetNeedsDatabasePrefix(t *testing.T) {
	yaml := withTarget("verify:\n  level: restore\n  into: staging") + `
verify_targets:
  - name: staging
    engine: postgres
    dsn: postgres://vaultd@staging:5432/postgres
`

	_, diags := parse(t, yaml)

	require.True(t, diags.HasErrors())
	assert.Contains(t, render(diags), "refuses to create or drop databases without one")
}

func TestMarshalRedactsSecrets(t *testing.T) {
	cfg, diags := parse(t, baseYAML)
	require.False(t, diags.HasErrors())

	out, err := config.Marshal(cfg)
	require.NoError(t, err)

	assert.NotContains(t, string(out), "s3cret-value", "the secret access key leaked into the effective config")
	assert.Contains(t, string(out), `secret_access_key: "***"`)
}

func render(diags config.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(d.String() + "\n")
	}
	return b.String()
}
