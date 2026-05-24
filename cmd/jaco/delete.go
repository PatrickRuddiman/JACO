package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(deleteCmd())
}

func deleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <deployment>",
		Short: "Delete a deployment (cascades Routes and ReplicaDesired)",
		Args:  cobra.ExactArgs(1),
	}
	var server, opToken, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	_ = c.MarkFlagRequired("server")

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		caCertPEM, err := readCACert(caCertPath)
		if err != nil {
			return err
		}
		conn, err := dialServer(server, caCertPEM)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)
		return runDelete(ctx, pb.NewDeployClient(conn), args[0], os.Stdout)
	}
	return c
}

func runDelete(ctx context.Context, client pb.DeployClient, deployment string, out io.Writer) error {
	if _, err := client.Delete(ctx, &pb.DeleteRequest{Deployment: deployment}); err != nil {
		return cliclient.FormatError(err)
	}
	fmt.Fprintf(out, "Deleted deployment: %s\n", deployment)
	return nil
}
