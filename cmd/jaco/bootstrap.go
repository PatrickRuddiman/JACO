package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
)

var bootstrapName string

func init() {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Initialize a new JACO cluster on this node",
		RunE:  runBootstrap,
	}
	cmd.Flags().StringVar(&bootstrapName, "name", "", "node hostname / raft local-id (required)")
	_ = cmd.MarkFlagRequired("name")
	rootCmd.AddCommand(cmd)
}

func runBootstrap(_ *cobra.Command, _ []string) error {
	dataDir := os.Getenv("JACO_DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/jaco"
	}
	res, err := bootstrap.Run(bootstrap.Options{
		DataDir: dataDir,
		Name:    bootstrapName,
	})
	if err != nil {
		return err
	}
	fmt.Println("Operator token (save this; not recoverable):", res.OperatorToken)
	return nil
}
