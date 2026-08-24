// Package cli builds the vaultd command tree.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/curruwilla/vaultd/internal/buildinfo"
	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/logging"
)

// Exit codes. They are part of the CLI contract: a cron job or a k8s Job reads
// them, so they may not be reshuffled casually.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// exitError carries an exit code alongside the error that caused it.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErrorf(format string, args ...any) error {
	return &exitError{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

// silentError is already reported in full; main only turns it into an exit code.
var errSilent = errors.New("")

// globals holds the flags every command shares.
type globals struct {
	configPath    string
	logLevel      string
	logFormat     string
	allowUnsetEnv bool

	out io.Writer
	err io.Writer
}

// load reads the config the way every command that needs one should.
func (g *globals) load() (*config.Config, config.Diagnostics, error) {
	return config.Load(g.configPath, config.LoadOptions{AllowUnsetEnv: g.allowUnsetEnv})
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRootCommand(os.Stdout, os.Stderr)

	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	code := ExitError
	var exit *exitError
	if errors.As(err, &exit) {
		code = exit.code
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return code
}

// NewRootCommand builds the command tree writing to out and errOut.
func NewRootCommand(out, errOut io.Writer) *cobra.Command {
	g := &globals{out: out, err: errOut}

	root := &cobra.Command{
		Use:   "vaultd",
		Short: "Database backups to S3-compatible storage",
		Long: "vaultd backs up MySQL, MariaDB, PostgreSQL and MongoDB to S3-compatible\n" +
			"storage: streaming dumps, compression, age encryption, GFS retention and\n" +
			"restore verification, all driven by one declarative config file.",
		Version:       buildinfo.Short(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare `vaultd` should show help rather than succeed silently.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageErrorf("%s: %w", cmd.CommandPath(), err)
	})

	flags := root.PersistentFlags()
	flags.StringVarP(&g.configPath, "config", "c", envOr("VAULTD_CONFIG", config.DefaultPath), "path to the config file")
	flags.StringVar(&g.logLevel, "log-level", envOr("VAULTD_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	flags.StringVar(&g.logFormat, "log-format", envOr("VAULTD_LOG_FORMAT", "text"), "log format: text, json")
	flags.BoolVar(&g.allowUnsetEnv, "allow-unset-env", false, "treat unresolved ${VAR} references as warnings instead of errors")

	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		_, err := logging.Setup(errOut, g.logLevel, g.logFormat)
		if err != nil {
			return usageErrorf("%w", err)
		}
		return nil
	}

	root.AddCommand(
		newValidateCommand(g),
		newVersionCommand(g),
	)
	root.AddCommand(newPlannedCommands(g)...)

	return root
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}
