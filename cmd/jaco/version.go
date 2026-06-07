package main

import (
	"github.com/spf13/cobra"
)

// newVersionCmd returns a `jaco version` subcommand that prints the same bare
// version string as `jaco --version` and `jacod --version`. Both flag and
// subcommand exist because Cobra (≥ v1.10) only wires `--version` automatically
// when rootCmd.Version is set — the `version` subcommand is opt-in.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the jaco CLI version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println(rootCmd.Version)
		},
	}
}

func init() {
	// Match `jacod --version`'s `fmt.Println(version)` output: bare version
	// line, no "jaco version " prefix.
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.AddCommand(newVersionCmd())
}
