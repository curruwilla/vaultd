package config

import (
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/robfig/cron/v3"

	"github.com/curruwilla/vaultd/internal/core"
)

// nameRE constrains names that end up inside object keys and lock keys.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// identRE constrains the database prefix a verify target may create with.
var identRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// cronParser accepts the standard 5-field cron syntax plus @daily-style
// descriptors — the same dialect the scheduler will run.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Validate runs every semantic check that does not need the network:
// cross-references, cron syntax, age recipients, per-engine options, and the
// mandatory-encryption rule (decision D5). Call it after ApplyDefaults.
func (c *Config) Validate() Diagnostics {
	var ds Diagnostics

	c.validateVersion(&ds)
	c.validateDestinations(&ds)
	c.validateTargets(&ds)
	c.validateVerifyTargets(&ds)
	c.validateNotifiers(&ds)
	c.validateServer(&ds)

	return ds
}

func (c *Config) validateVersion(ds *Diagnostics) {
	switch {
	case c.Version == 0:
		ds.errorf("version", "config has no version; add `version: %d` at the top", SchemaVersion)
	case c.Version != SchemaVersion:
		ds.errorf("version", "unsupported config version %d; this build understands version %d", c.Version, SchemaVersion)
	}
}

func (c *Config) validateDestinations(ds *Diagnostics) {
	if len(c.Destinations) == 0 {
		ds.errorf("destinations", "config has no destinations; declare at least one bucket")
		return
	}

	seen := map[string]bool{}
	for i, dest := range c.Destinations {
		p := index("destinations", i)

		switch {
		case dest.Name == "":
			ds.errorf(p+".name", "destination #%d has no name", i+1)
		case seen[dest.Name]:
			ds.errorf(p+".name", "duplicate destination name %q", dest.Name)
		case !nameRE.MatchString(dest.Name):
			ds.errorf(p+".name", "destination name %q must match %s", dest.Name, nameRE)
		}
		seen[dest.Name] = true

		if !valid(dest.Provider, Providers) {
			ds.errorf(p+".provider", "destination %q has provider %q; use one of %s", dest.Name, dest.Provider, oneOf(Providers))
		}
		if dest.Bucket == "" {
			ds.errorf(p+".bucket", "destination %q has no bucket", dest.Name)
		}

		switch dest.Provider {
		case ProviderR2, ProviderMinIO, ProviderGCSInterop:
			if dest.Endpoint == "" {
				ds.errorf(p+".endpoint", "destination %q (%s) needs an endpoint", dest.Name, dest.Provider)
			}
		case ProviderS3:
			if dest.Region == "" {
				ds.errorf(p+".region", "destination %q (s3) needs a region", dest.Name)
			}
		}

		if dest.Endpoint != "" {
			if u, err := url.Parse(dest.Endpoint); err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
				ds.errorf(p+".endpoint", "destination %q has an invalid endpoint %q; expected an absolute http(s) URL", dest.Name, dest.Endpoint)
			}
		}

		if dest.Provider == ProviderR2 && dest.StorageClass != "" {
			ds.warnf(p+".storage_class", "destination %q: storage_class is ignored on r2, which only offers Standard and Infrequent Access", dest.Name)
		}

		switch {
		case dest.AccessKeyID.Set() != dest.SecretAccessKey.Set():
			ds.errorf(p, "destination %q sets only one half of its credentials; set both access_key_id and secret_access_key, or neither", dest.Name)
		case !dest.AccessKeyID.Set() && dest.Provider != ProviderS3:
			ds.warnf(p, "destination %q has no credentials; the AWS credential chain rarely resolves for provider %s", dest.Name, dest.Provider)
		}
	}
}

func (c *Config) validateTargets(ds *Diagnostics) {
	if len(c.Targets) == 0 {
		ds.errorf("targets", "config has no targets; declare at least one database to back up")
		return
	}

	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		p := index("targets", i)

		switch {
		case t.Name == "":
			ds.errorf(p+".name", "target #%d has no name", i+1)
		case seen[t.Name]:
			ds.errorf(p+".name", "duplicate target name %q", t.Name)
		case !nameRE.MatchString(t.Name):
			ds.errorf(p+".name", "target name %q must match %s; it is used in object keys", t.Name, nameRE)
		}
		seen[t.Name] = true

		if !t.Engine.Valid() {
			ds.errorf(p+".engine", "target %q has engine %q; use one of %s", t.Name, t.Engine, oneOf(core.Engines))
		}

		c.validateTargetConn(ds, p, t)
		c.validateTargetDestination(ds, p, t)

		if t.Schedule == "" {
			ds.warnf(p+".schedule", "target %q has no schedule; it will only run when invoked manually", t.Name)
		} else if _, err := cronParser.Parse(t.Schedule); err != nil {
			ds.errorf(p+".schedule", "target %q has an invalid schedule %q: %s", t.Name, t.Schedule, err)
		}

		if t.Timeout != nil && t.Timeout.Duration() <= 0 {
			ds.errorf(p+".timeout", "target %q has a non-positive timeout %s", t.Name, t.Timeout)
		}

		validateOptions(ds, p, t)
		validateCompression(ds, p+".compression", t.Name, t.Compression)
		validateEncryption(ds, p+".encryption", t.Name, t.Encryption)
		validateRetention(ds, p+".retention", t.Name, t.Retention)
		validateEnum(ds, p+".spool", t.Name, "spool", t.Spool, Spools)
		validateEnum(ds, p+".on_overlap", t.Name, "on_overlap", t.OnOverlap, Overlaps)
		validateEnum(ds, p+".row_estimate", t.Name, "row_estimate", t.RowEstimate, RowEstimates)

		for j, notifier := range t.Notify {
			if _, ok := c.Notifier(notifier); !ok {
				ds.errorf(index(p+".notify", j), "target %q notifies %q, which is not declared under notifiers", t.Name, notifier)
			}
		}

		c.validateTargetVerify(ds, p, t)
	}
}

func (c *Config) validateTargetConn(ds *Diagnostics, p string, t *Target) {
	if t.Engine == core.EngineMongoDB {
		if !t.URI.Set() {
			ds.errorf(p+".uri", "target %q (mongodb) has no uri", t.Name)
		}
		if t.DSN.Set() {
			ds.errorf(p+".dsn", "target %q (mongodb) sets dsn; use uri instead", t.Name)
		}
		return
	}

	if !t.DSN.Set() {
		ds.errorf(p+".dsn", "target %q (%s) has no dsn", t.Name, t.Engine)
	}
	if t.URI.Set() {
		ds.errorf(p+".uri", "target %q (%s) sets uri; use dsn instead", t.Name, t.Engine)
	}
}

func (c *Config) validateTargetDestination(ds *Diagnostics, p string, t *Target) {
	if t.Destination == "" {
		ds.errorf(p+".destination", "target %q has no destination", t.Name)
		return
	}
	if _, ok := c.Destination(t.Destination); !ok {
		ds.errorf(p+".destination", "target %q writes to destination %q, which is not declared", t.Name, t.Destination)
	}
}

func (c *Config) validateTargetVerify(ds *Diagnostics, p string, t *Target) {
	v := t.Verify
	if v == nil {
		return
	}
	p += ".verify"

	if !valid(v.Level, VerifyLevels) {
		ds.errorf(p+".level", "target %q has verify level %q; use one of %s", t.Name, v.Level, oneOf(VerifyLevels))
	}
	if v.Schedule != "" {
		if _, err := cronParser.Parse(v.Schedule); err != nil {
			ds.errorf(p+".schedule", "target %q has an invalid verify schedule %q: %s", t.Name, v.Schedule, err)
		}
	}

	if v.Level == VerifyRestore {
		switch into, ok := c.VerifyTarget(v.Into); {
		case v.Into == "":
			ds.errorf(p+".into", "target %q verifies at level restore but has no `into`; name a verify target to restore into", t.Name)
		case !ok:
			ds.errorf(p+".into", "target %q restores into verify target %q, which is not declared", t.Name, v.Into)
		case into.Engine != t.Engine:
			ds.errorf(p+".into", "target %q (%s) restores into verify target %q (%s); engines must match", t.Name, t.Engine, into.Name, into.Engine)
		}
	} else if v.Into != "" {
		ds.warnf(p+".into", "target %q sets `into` but verifies at level %s, which never restores", t.Name, v.Level)
	}

	for j, a := range v.Assertions {
		validateAssertion(ds, index(p+".assertions", j), t.Name, a)
	}
}

func validateAssertion(ds *Diagnostics, p, target string, a Assertion) {
	if !valid(a.Type, AssertionTypes) {
		ds.errorf(p+".type", "target %q has assertion type %q; use one of %s", target, a.Type, oneOf(AssertionTypes))
		return
	}

	switch a.Type {
	case AssertRowCount:
		if a.Tolerance != nil && (*a.Tolerance < 0 || *a.Tolerance > 1) {
			ds.errorf(p+".tolerance", "target %q has row_count tolerance %v; it is a fraction between 0 and 1", target, *a.Tolerance)
		}
	case AssertQuery:
		if strings.TrimSpace(a.SQL) == "" {
			ds.errorf(p+".sql", "target %q has a query assertion with no sql", target)
		}
		if a.Expect == nil {
			ds.errorf(p+".expect", "target %q has a query assertion with no expect value", target)
		}
	case AssertMaxAge:
		if a.Value == nil || a.Value.Duration() <= 0 {
			ds.errorf(p+".value", "target %q has a max_age assertion with no positive value (for example 26h)", target)
		}
	case AssertTableCount:
		if len(a.Tables) > 0 {
			ds.warnf(p+".tables", "target %q lists tables on a table_count assertion, which always counts them all", target)
		}
	}
}

func validateOptions(ds *Diagnostics, p string, t *Target) {
	o := t.Options
	p += ".options"

	postgresOnly := len(o.ExcludeTableData) > 0 || o.IncludeGlobals != nil
	if postgresOnly && t.Engine != core.EnginePostgres {
		ds.errorf(p, "target %q (%s) sets postgres-only options (exclude_table_data, include_globals)", t.Name, t.Engine)
	}
	for j, glob := range o.ExcludeTableData {
		if _, err := path.Match(glob, ""); err != nil {
			ds.errorf(index(p+".exclude_table_data", j), "target %q has an invalid exclude_table_data pattern %q: %s", t.Name, glob, err)
		}
	}

	if o.OnNonInnoDB != "" {
		switch {
		case t.Engine != core.EngineMySQL && t.Engine != core.EngineMariaDB:
			ds.errorf(p+".on_non_innodb", "target %q (%s) sets on_non_innodb, which only applies to mysql and mariadb", t.Name, t.Engine)
		case !valid(o.OnNonInnoDB, NonInnoDBModes):
			ds.errorf(p+".on_non_innodb", "target %q has on_non_innodb %q; use one of %s", t.Name, o.OnNonInnoDB, oneOf(NonInnoDBModes))
		}
	}

	if o.Oplog != nil && t.Engine != core.EngineMongoDB {
		ds.errorf(p+".oplog", "target %q (%s) sets oplog, which only applies to mongodb", t.Name, t.Engine)
	}
}

func validateCompression(ds *Diagnostics, p, target string, c *Compression) {
	if c == nil {
		return
	}
	if !valid(c.Algo, CompressionAlgos) {
		ds.errorf(p+".algo", "target %q compresses with %q; use one of %s", target, c.Algo, oneOf(CompressionAlgos))
		return
	}

	switch c.Algo {
	case CompressionZstd:
		if c.Level < 1 || c.Level > 19 {
			ds.errorf(p+".level", "target %q has zstd level %d; zstd levels run from 1 to 19", target, c.Level)
		}
	case CompressionGzip:
		if c.Level < 1 || c.Level > 9 {
			ds.errorf(p+".level", "target %q has gzip level %d; gzip levels run from 1 to 9", target, c.Level)
		}
		if c.Long {
			ds.warnf(p+".long", "target %q sets long on gzip, which has no long-distance matching", target)
		}
	case CompressionNone:
		ds.warnf(p+".algo", "target %q stores its dump uncompressed", target)
	}
}

// validateEncryption enforces decision D5: a target without a declared
// encryption block fails validation rather than silently uploading plaintext.
func validateEncryption(ds *Diagnostics, p, target string, e *Encryption) {
	if e == nil {
		ds.errorf(p, "target %q has no encryption; set encryption.recipients or opt out with encryption.mode=none", target)
		return
	}
	if !valid(e.Mode, EncryptionModes) {
		ds.errorf(p+".mode", "target %q has encryption mode %q; use one of %s", target, e.Mode, oneOf(EncryptionModes))
		return
	}

	switch e.Mode {
	case EncryptionAge:
		if len(e.Recipients) == 0 {
			ds.errorf(p+".recipients", "target %q encrypts with age but lists no recipients", target)
		}
		for j, r := range e.Recipients {
			if _, err := age.ParseX25519Recipient(r); err != nil {
				ds.errorf(index(p+".recipients", j), "target %q has an unparseable age recipient %q: %s", target, r, err)
			}
		}
		if e.Passphrase.Set() {
			ds.errorf(p+".passphrase", "target %q sets both age recipients and a passphrase; pick one mode", target)
		}
	case EncryptionPassphrase:
		if !e.Passphrase.Set() {
			ds.errorf(p+".passphrase", "target %q encrypts with a passphrase but none is set", target)
		}
		if len(e.Recipients) > 0 {
			ds.errorf(p+".recipients", "target %q sets recipients on a passphrase mode; use mode: age instead", target)
		}
	case EncryptionNone:
		if len(e.Recipients) > 0 || e.Passphrase.Set() {
			ds.errorf(p, "target %q opts out of encryption but still carries keys; remove them or set a real mode", target)
		}
		ds.warnf(p, "target %q uploads its dump unencrypted (encryption.mode=none)", target)
	}
}

func validateRetention(ds *Diagnostics, p, target string, r *Retention) {
	if r.Empty() {
		ds.warnf(p, "target %q has no retention policy; every backup is kept forever", target)
		return
	}

	tiers := map[string]int{"hourly": tierKeep(r.Hourly), "daily": tierKeep(r.Daily), "yearly": tierKeep(r.Yearly)}
	if r.Weekly != nil {
		tiers["weekly"] = r.Weekly.Keep
		if r.Weekly.On < 0 || r.Weekly.On > 6 {
			ds.errorf(p+".weekly.on", "target %q has an invalid weekly.on %v", target, int(r.Weekly.On))
		}
	}
	if r.Monthly != nil {
		tiers["monthly"] = r.Monthly.Keep
		if r.Monthly.On < 1 || r.Monthly.On > 28 {
			ds.errorf(p+".monthly.on", "target %q has monthly.on %d; use 1 to 28 so every month has that day", target, r.Monthly.On)
		}
	}

	total := 0
	for tier, keep := range tiers {
		if keep < 0 {
			ds.errorf(p+"."+tier+".keep", "target %q keeps %d %s backups; keep cannot be negative", target, keep, tier)
		}
		total += keep
	}
	if total == 0 {
		ds.errorf(p, "target %q declares retention tiers but keeps nothing; prune would delete every backup", target)
	}

	if r.MinKeep < 1 {
		ds.errorf(p+".min_keep", "target %q has min_keep %d; the floor must keep at least one backup", target, r.MinKeep)
	}
}

func tierKeep(t *TierRule) int {
	if t == nil {
		return 0
	}
	return t.Keep
}

func (c *Config) validateVerifyTargets(ds *Diagnostics) {
	used := map[string]bool{}
	for _, t := range c.Targets {
		if t.Verify != nil && t.Verify.Into != "" {
			used[t.Verify.Into] = true
		}
	}

	seen := map[string]bool{}
	for i, v := range c.VerifyTargets {
		p := index("verify_targets", i)

		switch {
		case v.Name == "":
			ds.errorf(p+".name", "verify target #%d has no name", i+1)
		case seen[v.Name]:
			ds.errorf(p+".name", "duplicate verify target name %q", v.Name)
		}
		seen[v.Name] = true

		if !v.Engine.Valid() {
			ds.errorf(p+".engine", "verify target %q has engine %q; use one of %s", v.Name, v.Engine, oneOf(core.Engines))
		}
		if !v.Conn().Set() {
			field := "dsn"
			if v.Engine == core.EngineMongoDB {
				field = "uri"
			}
			ds.errorf(p+"."+field, "verify target %q has no %s", v.Name, field)
		}

		switch {
		case v.DatabasePrefix == "":
			ds.errorf(p+".database_prefix", "verify target %q has no database_prefix; vaultd refuses to create or drop databases without one", v.Name)
		case !identRE.MatchString(v.DatabasePrefix):
			ds.errorf(p+".database_prefix", "verify target %q has database_prefix %q, which is not a valid identifier prefix", v.Name, v.DatabasePrefix)
		}

		if v.MaxConcurrent < 1 {
			ds.errorf(p+".max_concurrent", "verify target %q has max_concurrent %d; it must be at least 1", v.Name, v.MaxConcurrent)
		}
		if !used[v.Name] {
			ds.warnf(p, "verify target %q is not referenced by any target", v.Name)
		}
	}
}

func (c *Config) validateNotifiers(ds *Diagnostics) {
	used := map[string]bool{}
	for _, t := range c.Targets {
		for _, n := range t.Notify {
			used[n] = true
		}
	}

	seen := map[string]bool{}
	for i, n := range c.Notifiers {
		p := index("notifiers", i)

		switch {
		case n.Name == "":
			ds.errorf(p+".name", "notifier #%d has no name", i+1)
		case seen[n.Name]:
			ds.errorf(p+".name", "duplicate notifier name %q", n.Name)
		}
		seen[n.Name] = true

		if !valid(n.Type, NotifierTypes) {
			ds.errorf(p+".type", "notifier %q has type %q; use one of %s", n.Name, n.Type, oneOf(NotifierTypes))
		}
		if !valid(n.Template, Templates) {
			ds.errorf(p+".template", "notifier %q has template %q; use one of %s", n.Name, n.Template, oneOf(Templates))
		}

		switch u, err := url.Parse(n.URL); {
		case n.URL == "":
			ds.errorf(p+".url", "notifier %q has no url", n.Name)
		case err != nil || u.Host == "":
			ds.errorf(p+".url", "notifier %q has an invalid url", n.Name)
		case u.Scheme != "https" && u.Scheme != "http":
			ds.errorf(p+".url", "notifier %q has url scheme %q; use https", n.Name, u.Scheme)
		case u.Scheme == "http":
			ds.warnf(p+".url", "notifier %q posts over plain http; the HMAC signature protects integrity, not the payload", n.Name)
		}

		if !n.Secret.Set() {
			ds.warnf(p+".secret", "notifier %q has no secret; its deliveries go out unsigned", n.Name)
		}
		if len(n.Events) == 0 {
			ds.errorf(p+".events", "notifier %q subscribes to no events", n.Name)
		}
		for j, e := range n.Events {
			if !valid(e, Events) {
				ds.errorf(index(p+".events", j), "notifier %q subscribes to unknown event %q; known events are %s", n.Name, e, oneOf(Events))
			}
		}
		if n.DedupWindow != nil && n.DedupWindow.Duration() < 0 {
			ds.errorf(p+".dedup_window", "notifier %q has a negative dedup_window", n.Name)
		}
		if !used[n.Name] {
			ds.warnf(p, "notifier %q is not referenced by any target", n.Name)
		}
	}
}

func (c *Config) validateServer(ds *Diagnostics) {
	s := c.Server
	if !s.UI && !s.Metrics {
		return
	}

	if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		ds.errorf("server.listen", "server.listen %q is not a host:port address", s.Listen)
	}
	if !valid(s.Auth.Mode, AuthModes) {
		ds.errorf("server.auth.mode", "server auth mode %q; use one of %s", s.Auth.Mode, oneOf(AuthModes))
		return
	}

	switch {
	case s.Auth.Mode == AuthToken && !s.Auth.Token.Set():
		ds.errorf("server.auth.token", "server auth mode is token but no token is set")
	case s.Auth.Mode == AuthNone && s.UI:
		ds.warnf("server.auth.mode", "the UI is served without authentication (server.auth.mode=none)")
	}
}

func validateEnum[T ~string](ds *Diagnostics, p, target, field string, value T, values []T) {
	if !valid(value, values) {
		ds.errorf(p, "target %q has %s %q; use one of %s", target, field, string(value), oneOf(values))
	}
}

func index(p string, i int) string { return p + "[" + strconv.Itoa(i) + "]" }
