package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
)

func init() {
	rootCmd.AddCommand(restoreCmd())
}

func restoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore",
		Short: "Restore a JACO cluster from a backup tarball into this node's data dir",
		Long: "Restores onto a fresh data directory. Replaces `jaco bootstrap` for the\n" +
			"receiving node. After restore, run `jaco serve` to bring the node online; it\n" +
			"will emit a RESTORE_COMPLETED audit event on first boot.",
	}
	var input, name string
	c.Flags().StringVar(&input, "input", "", "path to backup tarball (cluster.tar.gz); required")
	c.Flags().StringVar(&name, "name", "", "hostname / raft local-id for this node; required")
	_ = c.MarkFlagRequired("input")
	_ = c.MarkFlagRequired("name")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		dataDir := os.Getenv("JACO_DATA_DIR")
		if dataDir == "" {
			dataDir = "/var/lib/jaco"
		}

		f, err := os.Open(input)
		if err != nil {
			return fmt.Errorf("open input: %w", err)
		}
		defer f.Close()

		if err := backup.Import(backup.ImportOptions{
			DataDir:     dataDir,
			Reader:      f,
			LocalID:     name,
			JacoVersion: "0.0.1-dev",
		}); err != nil {
			return err
		}
		fmt.Printf("Restored into %s; run `jaco serve` to bring node %s online.\n", dataDir, name)
		return nil
	}
	return c
}
