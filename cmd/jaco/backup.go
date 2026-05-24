package main

import (
	"context"
	"errors"
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
	rootCmd.AddCommand(backupCmd())
}

func backupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Stream a cluster backup tarball to a local file",
	}
	var server, opToken, caCertPath, outputPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&outputPath, "output", "", "destination file (e.g. cluster.tar.gz); required")
	_ = c.MarkFlagRequired("server")
	_ = c.MarkFlagRequired("output")

	c.RunE = func(_ *cobra.Command, _ []string) error {
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)

		stream, err := pb.NewClusterClient(conn).Backup(ctx, &pb.BackupRequest{})
		if err != nil {
			return cliclient.FormatError(err)
		}

		f, err := os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer f.Close()

		var total int64
		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				// cliclient.FormatError already produces an operator-facing
				// "Error: <code>: <message>" line; wrapping it with another
				// prefix here yields a confusing double "Error:" in stderr.
				return cliclient.FormatError(err)
			}
			n, err := f.Write(chunk.GetData())
			if err != nil {
				return fmt.Errorf("write: %w", err)
			}
			total += int64(n)
		}
		fmt.Printf("Wrote %d bytes to %s\n", total, outputPath)
		return nil
	}
	return c
}
