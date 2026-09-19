package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/backup"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

func init() {
	rootCmd.AddCommand(restoreCmd())
}

func restoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore",
		Short: "Restore a JACO cluster from a backup tarball into this node's data dir",
		Long: "Authenticates and restores an encrypted backup onto a fresh data directory.\n" +
			"Requires the independently provisioned external state keyring. Convert legacy\n" +
			"plaintext archives with `jaco state reencrypt-backup` first.\n" +
			"After restore, configure jacod with the same keyring and data directory,\n" +
			"then start the daemon. The original archive is not changed.",
	}
	var input, name, keyFile, dataDir string
	c.Flags().StringVar(&input, "input", "", "path to backup tarball (cluster.tar.gz); required")
	c.Flags().StringVar(&name, "name", "", "hostname / raft local-id for this node; required")
	c.Flags().StringVar(&keyFile, "key-file", "", "external state keyring; defaults to JACO_STATE_KEY_FILE/service credential")
	c.Flags().StringVar(&dataDir, "data-dir", defaultStateDataDir(), "fresh application data directory")
	_ = c.MarkFlagRequired("input")
	_ = c.MarkFlagRequired("name")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		keys, err := seal.LoadFile(keyFile, dataDir)
		if err != nil {
			return err
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
			Keys:        keys,
			Logger:      Logger(),
		}); err != nil {
			return err
		}
		fmt.Printf("Restored into %s; run `jaco serve` to bring node %s online.\n", dataDir, name)
		return nil
	}
	return c
}
