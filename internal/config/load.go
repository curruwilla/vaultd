package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/goccy/go-yaml"
)

// DefaultPath is where vaultd looks for its config when none is given.
const DefaultPath = "vaultd.yaml"

// LoadOptions tunes a single Load. The zero value reads the real environment
// and filesystem, which is what the CLI wants.
type LoadOptions struct {
	// AllowUnsetEnv turns unresolvable ${VAR} references into warnings, so a
	// config can be validated where the secrets are not present.
	AllowUnsetEnv bool
	// Lookup and ReadFile override the environment and filesystem in tests.
	Lookup   func(string) (string, bool)
	ReadFile func(string) ([]byte, error)
}

// Load reads, interpolates, merges and validates a config file.
//
// The returned error is fatal — the file is missing or is not valid YAML. Any
// other problem comes back as diagnostics, with the parsed config alongside,
// so that a caller can report every finding in one pass. Load never touches
// the network: that is `vaultd doctor`.
func Load(path string, opts LoadOptions) (*Config, Diagnostics, error) {
	if path == "" {
		path = DefaultPath
	}

	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}

	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("config file %s does not exist", path)
		}
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}

	cfg, diags, err := Parse(data, opts)
	if err != nil {
		// Returned unwrapped so that FormatError can still render the
		// offending YAML line; the caller knows the path already.
		return nil, nil, err
	}
	cfg.Path = path
	return cfg, diags, nil
}

// SyntaxError marks a config file that could not be parsed as YAML at all, as
// opposed to one that is missing, unreadable, or semantically wrong. Only a
// syntax error has a source line worth printing.
type SyntaxError struct {
	Err error
	// Hint is an extra line explaining a mistake the raw text makes likely.
	Hint string
}

func (e *SyntaxError) Error() string { return e.Err.Error() }

func (e *SyntaxError) Unwrap() error { return e.Err }

// interpolationHint explains the trap a config with ${...} references can fall
// into. Interpolation happens after parsing — which is what keeps a secret
// containing a colon or a brace from breaking the document — so a reference
// can only stand where a string is expected.
func interpolationHint(data []byte) string {
	if !bytes.Contains(data, []byte("${")) {
		return ""
	}
	return "note: ${VAR} is expanded after the file is parsed, so a reference only fits where text is expected:\n" +
		"      a number or a boolean cannot be templated, and inside { } a reference must be quoted."
}

// Parse is Load without the file read; it is the seam the tests use.
func Parse(data []byte, opts LoadOptions) (cfg *Config, diags Diagnostics, err error) {
	// goccy/go-yaml v1.19.2 panics on some malformed documents (a tag on a
	// sequence field, for one: `targets: !0 0`). A bad config file must fail
	// the command, never take the daemon down with it, so the panic is turned
	// back into the parse error it should have been. Found by FuzzParse.
	defer func() {
		if r := recover(); r != nil {
			cfg, diags, err = nil, nil, &SyntaxError{
				Err: fmt.Errorf("the decoder failed on this document (%v)", r),
			}
		}
	}()

	return parse(data, opts)
}

func parse(data []byte, opts LoadOptions) (*Config, Diagnostics, error) {
	var cfg Config
	// Strict decoding rejects unknown and duplicated keys: a typo in a config
	// that silently does nothing is exactly the failure mode backups cannot
	// afford.
	if err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict()); err != nil {
		return nil, nil, &SyntaxError{Err: err, Hint: interpolationHint(data)}
	}

	in := newInterpolator()
	in.AllowUnset = opts.AllowUnsetEnv
	if opts.Lookup != nil {
		in.Lookup = opts.Lookup
	}
	if opts.ReadFile != nil {
		in.ReadFile = opts.ReadFile
	}

	diags := in.interpolate(&cfg)
	cfg.ApplyDefaults()
	diags = append(diags, cfg.Validate()...)

	return &cfg, diags, nil
}

// FormatError renders a YAML syntax error with the offending source line, the
// way goccy prints it, and returns "" for every other kind of error so callers
// can tell the two apart. Colors are left off: the CLI decides.
func FormatError(err error) string {
	var syntax *SyntaxError
	if !errors.As(err, &syntax) {
		return ""
	}
	formatted := yaml.FormatError(syntax.Err, false, true)
	if syntax.Hint != "" {
		formatted += "\n" + syntax.Hint
	}
	return formatted
}

// Marshal renders a config back to YAML with every secret redacted — the
// "effective config" view the CLI and, later, the UI show (SPEC §13).
func Marshal(cfg *Config) ([]byte, error) {
	return yaml.MarshalWithOptions(cfg, yaml.Indent(2), yaml.IndentSequence(true))
}
