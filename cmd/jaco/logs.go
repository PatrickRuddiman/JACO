package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(logsCmd())
}

func logsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logs <deployment>/<service>",
		Short: "Stream container logs from every replica of a service",
		Args:  cobra.ExactArgs(1),
	}
	var (
		server, opToken, caCertPath string
		follow                      bool
		since                       string
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they arrive")
	c.Flags().StringVar(&since, "since", "5m", "only lines newer than this duration (e.g. 1h, 30m)")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		deployment, service, err := splitDeploymentService(args[0])
		if err != nil {
			return err
		}
		sinceSeconds, err := parseSinceSeconds(since)
		if err != nil {
			return err
		}

		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()

		ctx, cancel := logsContext(follow)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opToken)

		client := pb.NewDeployClient(conn)
		return runLogs(ctx, client, deployment, service, follow, sinceSeconds, os.Stdout)
	}
	return c
}

// runLogs is the unit-testable body: opens Deploy.Logs and renders each
// LogLine as `[<replica-id>@<host>] <line>` to out.
func runLogs(ctx context.Context, client pb.DeployClient, deployment, service string, follow bool, sinceSeconds int64, out io.Writer) error {
	stream, err := client.Logs(ctx, &pb.LogsRequest{
		Deployment:   deployment,
		Service:      service,
		Follow:       follow,
		SinceSeconds: sinceSeconds,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	for {
		ll, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		fmt.Fprintf(out, "[%s@%s] %s\n", ll.GetReplicaId(), ll.GetHost(), ll.GetLine())
	}
}

func splitDeploymentService(s string) (string, string, error) {
	idx := strings.IndexByte(s, '/')
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("expected <deployment>/<service>, got %q", s)
	}
	return s[:idx], s[idx+1:], nil
}

func parseSinceSeconds(s string) (int64, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since: %w", err)
	}
	return int64(d.Seconds()), nil
}

func logsContext(follow bool) (context.Context, context.CancelFunc) {
	if follow {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), 60*time.Second)
}
