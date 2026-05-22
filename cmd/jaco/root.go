package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	flagContext string
	flagOutput  string
	flagServer  string
	flagQuiet   bool
	flagVerbose bool
)

var rootCmd = &cobra.Command{
	Use:           "jaco",
	Short:         "JACO — multi-node container orchestrator",
	SilenceUsage:  true,
	SilenceErrors: true,
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
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
