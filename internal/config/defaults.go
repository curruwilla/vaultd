package config

import (
	"strings"
	"time"
)

// Built-in defaults, applied when neither the target nor the `defaults` block
// sets a value. Encryption is deliberately absent: it has no safe default and
// must be declared (decision D5).
var (
	builtinCompression = Compression{Algo: CompressionZstd, Level: 3, Long: true}
	builtinTimeout     = Duration(4 * time.Hour)
	builtinSpool       = SpoolNone
	builtinOverlap     = OverlapSkip
	builtinRowEstimate = RowEstimateEstimate
	builtinMinKeep     = 1
	builtinListen      = ":8080"
)

// defaultLevels are the compression levels used when `level` is omitted.
var defaultLevels = map[CompressionAlgo]int{
	CompressionZstd: 3,
	CompressionGzip: 6,
	CompressionNone: 0,
}

// ApplyDefaults resolves inheritance in place: every target ends up holding
// the settings it will actually run with, so validation and every later stage
// read one struct instead of re-deriving the merge.
func (c *Config) ApplyDefaults() {
	c.applyDefaultsBlock()

	for i := range c.Destinations {
		c.Destinations[i].applyDefaults()
	}
	for i := range c.Targets {
		c.Targets[i].applyDefaults(&c.Defaults)
	}
	for i := range c.VerifyTargets {
		if c.VerifyTargets[i].MaxConcurrent == 0 {
			c.VerifyTargets[i].MaxConcurrent = 1
		}
	}
	for i := range c.Notifiers {
		if c.Notifiers[i].Type == "" {
			c.Notifiers[i].Type = NotifierWebhook
		}
		if c.Notifiers[i].Template == "" {
			c.Notifiers[i].Template = TemplateGeneric
		}
	}

	if c.Server.Listen == "" {
		c.Server.Listen = builtinListen
	}
	if c.Server.Auth.Mode == "" {
		c.Server.Auth.Mode = AuthToken
	}
}

func (c *Config) applyDefaultsBlock() {
	d := &c.Defaults
	if d.Compression == nil {
		compression := builtinCompression
		d.Compression = &compression
	}
	d.Compression.applyDefaults()

	if d.Timeout == nil {
		timeout := builtinTimeout
		d.Timeout = &timeout
	}
	if d.Spool == "" {
		d.Spool = builtinSpool
	}
	if d.OnOverlap == "" {
		d.OnOverlap = builtinOverlap
	}
	if d.RowEstimate == "" {
		d.RowEstimate = builtinRowEstimate
	}
	d.Retention.applyDefaults()
}

func (dest *Destination) applyDefaults() {
	if dest.Provider == ProviderR2 && dest.Region == "" {
		dest.Region = "auto"
	}
	dest.Prefix = strings.Trim(dest.Prefix, "/")
}

func (t *Target) applyDefaults(d *Defaults) {
	if t.Compression == nil {
		t.Compression = d.Compression.clone()
	}
	t.Compression.applyDefaults()

	if t.Encryption == nil {
		t.Encryption = d.Encryption.clone()
	}
	if t.Retention == nil {
		t.Retention = d.Retention.clone()
	}
	t.Retention.applyDefaults()

	if t.Timeout == nil {
		timeout := *d.Timeout
		t.Timeout = &timeout
	}
	if t.Notify == nil {
		t.Notify = append([]string(nil), d.Notify...)
	}
	if t.Spool == "" {
		t.Spool = d.Spool
	}
	if t.OnOverlap == "" {
		t.OnOverlap = d.OnOverlap
	}
	if t.RowEstimate == "" {
		t.RowEstimate = d.RowEstimate
	}
	t.Verify.applyDefaults()
}

func (c *Compression) applyDefaults() {
	if c == nil {
		return
	}
	if c.Algo == "" {
		c.Algo = builtinCompression.Algo
	}
	if c.Level == 0 {
		c.Level = defaultLevels[c.Algo]
	}
}

func (c *Compression) clone() *Compression {
	if c == nil {
		return nil
	}
	out := *c
	return &out
}

func (e *Encryption) clone() *Encryption {
	if e == nil {
		return nil
	}
	out := *e
	out.Recipients = append([]string(nil), e.Recipients...)
	return &out
}

func (r *Retention) applyDefaults() {
	if r == nil {
		return
	}
	if r.MinKeep == 0 {
		r.MinKeep = builtinMinKeep
	}
	if r.Monthly != nil && r.Monthly.On == 0 {
		r.Monthly.On = 1
	}
}

func (r *Retention) clone() *Retention {
	if r == nil {
		return nil
	}
	out := *r
	out.Hourly = cloneTier(r.Hourly)
	out.Daily = cloneTier(r.Daily)
	out.Yearly = cloneTier(r.Yearly)
	if r.Weekly != nil {
		weekly := *r.Weekly
		out.Weekly = &weekly
	}
	if r.Monthly != nil {
		monthly := *r.Monthly
		out.Monthly = &monthly
	}
	return &out
}

func cloneTier(t *TierRule) *TierRule {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

func (v *VerifySpec) applyDefaults() {
	if v == nil {
		return
	}
	if v.Level == "" {
		v.Level = VerifyIntegrity
	}
}
