package config_test

import (
	"testing"

	"github.com/curruwilla/vaultd/internal/config"
)

// FuzzParse checks that no input makes the config parser panic. A config file
// is attacker-adjacent input in any environment where it is templated, and a
// panic here takes the daemon down with it (SPEC §16).
func FuzzParse(f *testing.F) {
	f.Add(baseYAML)
	f.Add(inheritanceYAML)
	f.Add("version: 1\n")
	f.Add("version: 1\ntargets: [{name: a, engine: postgres}]\n")
	f.Add("defaults: {timeout: 4h, retention: {weekly: {on: sunday}}}\n")
	f.Add("targets:\n  - name: ${A:-b}\n    dsn: ${file:/etc/hostname}\n")

	f.Fuzz(func(t *testing.T, data string) {
		cfg, diags, err := config.Parse([]byte(data), config.LoadOptions{
			AllowUnsetEnv: true,
			Lookup:        func(string) (string, bool) { return "", false },
		})
		if err != nil {
			return
		}
		if cfg == nil {
			t.Fatal("Parse returned no config and no error")
		}
		// A config that parses must always be renderable back, redacted: the
		// UI and `validate --print-effective` both depend on it.
		if _, err := config.Marshal(cfg); err != nil {
			t.Fatalf("Marshal of a parsed config failed: %v", err)
		}
		_ = diags
	})
}
