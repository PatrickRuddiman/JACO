package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/logging"
)

var (
	flagContext  string
	flagOutput   string
	flagServer   string
	flagQuiet    bool
	flagVerbose  bool
	flagLogLevel string
)

// version is the CLI release string baked in at build time via
// `-ldflags '-X main.version=…'` (see build/release.sh). Mirrors
// cmd/jacod/main.go so a single ldflag covers both binaries.
var version = "dev"

// cliRoot is the CLI's bare root logger (no subsystem attr). Configured once
// in PersistentPreRun from --log-level / --verbose / JACO_LOG; defaults to
// WARN so normal operator output (rendered via cliclient) stays uncluttered.
var cliRoot = logging.NewCLI(os.Stderr, slog.LevelWarn)

// Logger returns the CLI's logger tagged subsystem=jaco for the CLI's own
// work. cliclient derives its own subsystem from the bare root via WithLogger.
func Logger() *slog.Logger { return logging.Subsystem(cliRoot, "jaco") }

var rootCmd = &cobra.Command{
	Use:           "jaco",
	Version:       version,
	Short:         "JACO — multi-node container orchestrator",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		// Level precedence: --log-level > --verbose > JACO_LOG > WARN.
		level := logging.LevelFromEnv(slog.LevelWarn)
		if flagVerbose {
			level = slog.LevelDebug
		}
		if flagLogLevel != "" {
			level = logging.ParseLevel(flagLogLevel, level)
		}
		cliRoot = logging.NewCLI(os.Stderr, level)
	},
	Run: func(cmd *cobra.Command, _ []string) {
		_ = cmd.Help()
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagContext, "context", "", "cluster context name (overrides current_context in clusters.yaml)")
	pf.StringVarP(&flagOutput, "output", "o", "table", "output format: table|json|yaml")
	pf.StringVar(&flagServer, "server", "", "single-shot server address override; bypasses context")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "suppress non-essential output")
	pf.BoolVarP(&flagVerbose, "verbose", "v", false, "debug-level logs to stderr")
	pf.StringVar(&flagLogLevel, "log-level", "", "log level: debug|info|warn|error (overrides JACO_LOG; default warn)")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
