package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// SchemaVersion is the only config `version:` this build accepts.
const SchemaVersion = 1

// Config is a parsed vaultd.yaml. Blocks that a target may inherit from
// `defaults` are pointers so that "absent" is distinguishable from "zero";
// ApplyDefaults resolves them in place.
type Config struct {
	Version       int            `yaml:"version"`
	Defaults      Defaults       `yaml:"defaults,omitempty"`
	Destinations  []Destination  `yaml:"destinations,omitempty"`
	Targets       []Target       `yaml:"targets,omitempty"`
	VerifyTargets []VerifyTarget `yaml:"verify_targets,omitempty"`
	Notifiers     []Notifier     `yaml:"notifiers,omitempty"`
	Server        Server         `yaml:"server,omitempty"`

	// Path is where this config was read from. Not a YAML field.
	Path string `yaml:"-"`
}

// Defaults holds the settings every target inherits unless it overrides them.
type Defaults struct {
	Compression *Compression `yaml:"compression,omitempty"`
	Encryption  *Encryption  `yaml:"encryption,omitempty"`
	Retention   *Retention   `yaml:"retention,omitempty"`
	Timeout     *Duration    `yaml:"timeout,omitempty"`
	Notify      []string     `yaml:"notify,omitempty"`
	Spool       Spool        `yaml:"spool,omitempty"`
	OnOverlap   Overlap      `yaml:"on_overlap,omitempty"`
	RowEstimate RowEstimate  `yaml:"row_estimate,omitempty"`
}

// Destination is an object storage bucket backups are written to.
type Destination struct {
	Name            string   `yaml:"name"`
	Provider        Provider `yaml:"provider"`
	Bucket          string   `yaml:"bucket"`
	Endpoint        string   `yaml:"endpoint,omitempty"`
	Region          string   `yaml:"region,omitempty"`
	Prefix          string   `yaml:"prefix,omitempty"`
	AccessKeyID     Secret   `yaml:"access_key_id,omitempty"`
	SecretAccessKey Secret   `yaml:"secret_access_key,omitempty"`
	StorageClass    string   `yaml:"storage_class,omitempty"`
	ForcePathStyle  bool     `yaml:"force_path_style,omitempty"`
}

// Target is one database vaultd backs up.
type Target struct {
	Name        string      `yaml:"name"`
	Engine      core.Engine `yaml:"engine"`
	DSN         Secret      `yaml:"dsn,omitempty"`
	URI         Secret      `yaml:"uri,omitempty"`
	Destination string      `yaml:"destination"`
	Schedule    string      `yaml:"schedule,omitempty"`
	Options     Options     `yaml:"options,omitempty"`
	Verify      *VerifySpec `yaml:"verify,omitempty"`

	// Overrides of the matching `defaults` block.
	Compression *Compression `yaml:"compression,omitempty"`
	Encryption  *Encryption  `yaml:"encryption,omitempty"`
	Retention   *Retention   `yaml:"retention,omitempty"`
	Timeout     *Duration    `yaml:"timeout,omitempty"`
	Notify      []string     `yaml:"notify,omitempty"`
	Spool       Spool        `yaml:"spool,omitempty"`
	OnOverlap   Overlap      `yaml:"on_overlap,omitempty"`
	RowEstimate RowEstimate  `yaml:"row_estimate,omitempty"`
}

// Conn returns the connection string of a target, whichever field carries it.
func (t Target) Conn() Secret {
	if t.Engine == core.EngineMongoDB {
		return t.URI
	}
	return t.DSN
}

// Options are the per-engine dump knobs (SPEC §4.1). Each field is only valid
// for the engines named in its comment; validation rejects the rest.
type Options struct {
	// postgres
	ExcludeTableData []string `yaml:"exclude_table_data,omitempty"`
	IncludeGlobals   *bool    `yaml:"include_globals,omitempty"`
	// mysql, mariadb
	OnNonInnoDB NonInnoDB `yaml:"on_non_innodb,omitempty"`
	// mongodb
	Oplog *bool `yaml:"oplog,omitempty"`
}

// VerifySpec configures restore verification for one target (SPEC §8).
type VerifySpec struct {
	Level      VerifyLevel `yaml:"level"`
	Schedule   string      `yaml:"schedule,omitempty"`
	Into       string      `yaml:"into,omitempty"`
	Assertions []Assertion `yaml:"assertions,omitempty"`
}

// Assertion is one check run against a restored database.
type Assertion struct {
	Type   AssertionType `yaml:"type,omitempty"`
	Tables []string      `yaml:"tables,omitempty"`
	// Tolerance is the accepted relative drift for row_count, 0 meaning exact.
	Tolerance *float64  `yaml:"tolerance,omitempty"`
	SQL       string    `yaml:"sql,omitempty"`
	Expect    any       `yaml:"expect,omitempty"`
	Value     *Duration `yaml:"value,omitempty"`
}

// VerifyTarget is the staging instance L2 restores into (decision D3).
type VerifyTarget struct {
	Name           string      `yaml:"name"`
	Engine         core.Engine `yaml:"engine"`
	DSN            Secret      `yaml:"dsn,omitempty"`
	URI            Secret      `yaml:"uri,omitempty"`
	DatabasePrefix string      `yaml:"database_prefix,omitempty"`
	MaxConcurrent  int         `yaml:"max_concurrent,omitempty"`
}

// Conn returns the connection string of a verify target.
func (v VerifyTarget) Conn() Secret {
	if v.Engine == core.EngineMongoDB {
		return v.URI
	}
	return v.DSN
}

// Notifier delivers events to an external endpoint (SPEC §12).
type Notifier struct {
	Name        string       `yaml:"name"`
	Type        NotifierType `yaml:"type,omitempty"`
	URL         string       `yaml:"url,omitempty"`
	Secret      Secret       `yaml:"secret,omitempty"`
	Events      []Event      `yaml:"events,omitempty"`
	Template    Template     `yaml:"template,omitempty"`
	DedupWindow *Duration    `yaml:"dedup_window,omitempty"`
}

// Server configures `vaultd serve`.
type Server struct {
	Listen  string     `yaml:"listen,omitempty"`
	UI      bool       `yaml:"ui,omitempty"`
	Auth    ServerAuth `yaml:"auth,omitempty"`
	Metrics bool       `yaml:"metrics,omitempty"`
}

// ServerAuth is the UI/API authentication block.
type ServerAuth struct {
	Mode  AuthMode `yaml:"mode"`
	Token Secret   `yaml:"token,omitempty"`
}

// Compression settings for the pipeline stage before encryption.
type Compression struct {
	Algo  CompressionAlgo `yaml:"algo"`
	Level int             `yaml:"level"`
	Long  bool            `yaml:"long,omitempty"`
}

// Encryption settings. Declaring this block is mandatory (decision D5); the
// escape hatch is an explicit mode: none.
type Encryption struct {
	Mode       EncryptionMode `yaml:"mode"`
	Recipients []string       `yaml:"recipients,omitempty"`
	Passphrase Secret         `yaml:"passphrase,omitempty"`
}

// Retention is the GFS policy of a target (SPEC §7).
type Retention struct {
	Hourly  *TierRule    `yaml:"hourly,omitempty"`
	Daily   *TierRule    `yaml:"daily,omitempty"`
	Weekly  *WeeklyRule  `yaml:"weekly,omitempty"`
	Monthly *MonthlyRule `yaml:"monthly,omitempty"`
	Yearly  *TierRule    `yaml:"yearly,omitempty"`
	// MinKeep is the absolute floor: prune never goes below it, whatever the
	// tier rules say.
	MinKeep int `yaml:"min_keep"`
}

// Empty reports whether the policy retains nothing by tier, i.e. every backup
// is kept forever.
func (r *Retention) Empty() bool {
	if r == nil {
		return true
	}
	return r.Hourly == nil && r.Daily == nil && r.Weekly == nil && r.Monthly == nil && r.Yearly == nil
}

// TierRule keeps the N most recent backups of a tier.
type TierRule struct {
	Keep int `yaml:"keep"`
}

// WeeklyRule promotes the backup taken on weekday On.
type WeeklyRule struct {
	Keep int     `yaml:"keep"`
	On   Weekday `yaml:"on"`
}

// MonthlyRule promotes the backup taken on day-of-month On.
type MonthlyRule struct {
	Keep int `yaml:"keep"`
	On   int `yaml:"on"`
}

// Enumerations. Each is a string type with a fixed set of values so that
// validation can report the accepted ones instead of a bare "invalid value".

type Provider string

const (
	ProviderS3         Provider = "s3"
	ProviderR2         Provider = "r2"
	ProviderMinIO      Provider = "minio"
	ProviderGCSInterop Provider = "gcs-interop"
)

var Providers = []Provider{ProviderS3, ProviderR2, ProviderMinIO, ProviderGCSInterop}

type CompressionAlgo string

const (
	CompressionZstd CompressionAlgo = "zstd"
	CompressionGzip CompressionAlgo = "gzip"
	CompressionNone CompressionAlgo = "none"
)

var CompressionAlgos = []CompressionAlgo{CompressionZstd, CompressionGzip, CompressionNone}

type EncryptionMode string

const (
	EncryptionAge        EncryptionMode = "age"
	EncryptionPassphrase EncryptionMode = "passphrase"
	EncryptionNone       EncryptionMode = "none"
)

var EncryptionModes = []EncryptionMode{EncryptionAge, EncryptionPassphrase, EncryptionNone}

type VerifyLevel string

const (
	VerifyIntegrity  VerifyLevel = "integrity"
	VerifyStructural VerifyLevel = "structural"
	VerifyRestore    VerifyLevel = "restore"
)

var VerifyLevels = []VerifyLevel{VerifyIntegrity, VerifyStructural, VerifyRestore}

type AssertionType string

const (
	AssertTableCount AssertionType = "table_count"
	AssertRowCount   AssertionType = "row_count"
	AssertQuery      AssertionType = "query"
	AssertMaxAge     AssertionType = "max_age"
)

var AssertionTypes = []AssertionType{AssertTableCount, AssertRowCount, AssertQuery, AssertMaxAge}

type NonInnoDB string

const (
	NonInnoDBWarn NonInnoDB = "warn"
	NonInnoDBFail NonInnoDB = "fail"
	NonInnoDBLock NonInnoDB = "lock"
)

var NonInnoDBModes = []NonInnoDB{NonInnoDBWarn, NonInnoDBFail, NonInnoDBLock}

type Spool string

const (
	SpoolNone Spool = "none"
	SpoolDisk Spool = "disk"
)

var Spools = []Spool{SpoolNone, SpoolDisk}

type Overlap string

const (
	OverlapSkip  Overlap = "skip"
	OverlapQueue Overlap = "queue"
	OverlapFail  Overlap = "fail"
)

var Overlaps = []Overlap{OverlapSkip, OverlapQueue, OverlapFail}

// RowEstimate picks how manifest row counts are gathered. Default is
// `estimate` (decision D7): cheap catalog statistics rather than count(*).
type RowEstimate string

const (
	RowEstimateExact    RowEstimate = "exact"
	RowEstimateEstimate RowEstimate = "estimate"
	RowEstimateOff      RowEstimate = "off"
)

var RowEstimates = []RowEstimate{RowEstimateExact, RowEstimateEstimate, RowEstimateOff}

type NotifierType string

const NotifierWebhook NotifierType = "webhook"

var NotifierTypes = []NotifierType{NotifierWebhook}

type Template string

const (
	TemplateGeneric Template = "generic"
	TemplateSlack   Template = "slack"
	TemplateDiscord Template = "discord"
)

var Templates = []Template{TemplateGeneric, TemplateSlack, TemplateDiscord}

type AuthMode string

const (
	AuthToken AuthMode = "token"
	AuthNone  AuthMode = "none"
)

var AuthModes = []AuthMode{AuthToken, AuthNone}

// Event is a notification event name (SPEC §12).
type Event string

const (
	EventBackupStarted    Event = "backup.started"
	EventBackupSucceeded  Event = "backup.succeeded"
	EventBackupFailed     Event = "backup.failed"
	EventVerifySucceeded  Event = "verify.succeeded"
	EventVerifyFailed     Event = "verify.failed"
	EventRetentionPruned  Event = "retention.pruned"
	EventRetentionBlocked Event = "retention.blocked"
	EventScheduleMissed   Event = "schedule.missed"
	EventStorageError     Event = "storage.error"
)

var Events = []Event{
	EventBackupStarted, EventBackupSucceeded, EventBackupFailed,
	EventVerifySucceeded, EventVerifyFailed,
	EventRetentionPruned, EventRetentionBlocked,
	EventScheduleMissed, EventStorageError,
}

// Duration is a time.Duration written as a YAML string ("4h", "30m").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(b []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: use a Go duration such as 30m, 4h, 26h", raw)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Weekday is a lowercase weekday name ("sunday").
type Weekday time.Weekday

var weekdayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

func (w Weekday) String() string {
	if int(w) < 0 || int(w) > 6 {
		return fmt.Sprintf("weekday(%d)", int(w))
	}
	return weekdayNames[int(w)]
}

func (w *Weekday) UnmarshalYAML(b []byte) error {
	raw := strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"'`))
	for i, name := range weekdayNames {
		if raw == name {
			*w = Weekday(i)
			return nil
		}
	}
	return fmt.Errorf("invalid weekday %q: use one of %s", raw, strings.Join(weekdayNames, ", "))
}

func (w Weekday) MarshalYAML() (any, error) { return w.String(), nil }

// Lookup helpers used by the CLI and, later, the scheduler.

func (c *Config) Target(name string) (*Target, bool) {
	for i := range c.Targets {
		if c.Targets[i].Name == name {
			return &c.Targets[i], true
		}
	}
	return nil, false
}

func (c *Config) Destination(name string) (*Destination, bool) {
	for i := range c.Destinations {
		if c.Destinations[i].Name == name {
			return &c.Destinations[i], true
		}
	}
	return nil, false
}

func (c *Config) VerifyTarget(name string) (*VerifyTarget, bool) {
	for i := range c.VerifyTargets {
		if c.VerifyTargets[i].Name == name {
			return &c.VerifyTargets[i], true
		}
	}
	return nil, false
}

func (c *Config) Notifier(name string) (*Notifier, bool) {
	for i := range c.Notifiers {
		if c.Notifiers[i].Name == name {
			return &c.Notifiers[i], true
		}
	}
	return nil, false
}
