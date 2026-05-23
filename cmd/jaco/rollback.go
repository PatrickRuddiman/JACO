package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(rollbackCmd())
}

func rollbackCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rollback <deployment>",
		Short: "Roll a deployment back to its previous revision",
		Args:  cobra.ExactArgs(1),
	}
	var server, opToken, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)
		return runRollback(ctx, pb.NewDeployClient(conn), args[0], os.Stdout)
	}
	return c
}

func runRollback(ctx context.Context, client pb.DeployClient, deployment string, out io.Writer) error {
	resp, err := client.Rollback(ctx, &pb.RollbackRequest{Deployment: deployment})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Rolled back to revision: %d\n", resp.GetRevision())
	return nil
}
