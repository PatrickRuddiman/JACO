package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	rootCmd.AddCommand(statusCmd())
}

func statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status [deployment[/service]]",
		Short: "Print a snapshot of cluster deployments, replicas, and routes",
		Args:  cobra.MaximumNArgs(1),
	}
	var server, opToken, caCertPath string
	var watch bool
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&opToken, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	c.Flags().BoolVarP(&watch, "watch", "w", false, "re-render on every state change (Ctrl-C to exit)")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, args []string) error {
		if opToken == "" {
			opToken = os.Getenv("JACO_TOKEN")
		}
		if opToken == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		dep, svc := "", ""
		if len(args) == 1 {
			parts := strings.SplitN(args[0], "/", 2)
			dep = parts[0]
			if len(parts) == 2 {
				svc = parts[1]
			}
		}
		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			"authorization", "Bearer "+opToken)
		deployClient := pb.NewDeployClient(conn)
		if !watch {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return runStatus(ctx, deployClient, dep, svc, os.Stdout)
		}
		watchClient := pb.NewWatchClient(conn)
		return runStatusWatch(ctx, deployClient, watchClient, dep, svc, os.Stdout)
	}
	return c
}

// runStatus prints a single snapshot.
func runStatus(ctx context.Context, client pb.DeployClient, deployment, service string, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.DeployStatusRequest{
		DeploymentFilter: deployment,
		ServiceFilter:    service,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	return renderStatus(out, resp)
}

// runStatusWatch prints the initial snapshot, then re-renders on each
// SubscribeEvent received. Each snapshot is separated by a `---` marker so
// tests can count them.
func runStatusWatch(ctx context.Context, deploy pb.DeployClient, watch pb.WatchClient, deployment, service string, out io.Writer) error {
	if err := runStatus(ctx, deploy, deployment, service, out); err != nil {
		return err
	}
	stream, err := watch.Subscribe(ctx, &pb.SubscribeRequest{
		EntityTypes:      []string{"deployments", "replicas_observed", "routes"},
		DeploymentFilter: deployment,
	})
	if err != nil {
		return cliclient.FormatError(err)
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return cliclient.FormatError(err)
		}
		// Re-fetch the snapshot on any event. (Resync sends the same trigger
		// — re-fetch is idempotent.)
		fmt.Fprintln(out, "---")
		if rerr := runStatus(ctx, deploy, deployment, service, out); rerr != nil {
			return rerr
		}
	}
}

// renderStatus prints three tables: deployments, replicas, routes.
func renderStatus(out io.Writer, resp *pb.DeployStatusResponse) error {
	// Deployments table.
	depHeaders := []string{"DEPLOYMENT", "REVISION", "PREVIOUS", "STATUS"}
	var depRows [][]string
	for _, d := range resp.GetDeployments() {
		depRows = append(depRows, []string{
			d.GetName(),
			strconv.FormatUint(d.GetAppliedRevision(), 10),
			strconv.FormatUint(d.GetPreviousRevision(), 10),
			strings.TrimPrefix(d.GetStatus().String(), "DEPLOYMENT_STATUS_"),
		})
	}
	fmt.Fprintln(out, "Deployments:")
	if err := cliclient.RenderTable(out, depHeaders, depRows); err != nil {
		return err
	}

	// Replicas table.
	repHeaders := []string{"REPLICA_ID", "STATE", "HOST", "CONTAINER_ID", "LAST_HEALTH_AT"}
	var repRows [][]string
	for _, r := range resp.GetReplicas() {
		last := ""
		if t := r.GetLastHealthAt(); t != nil {
			last = t.AsTime().UTC().Format(time.RFC3339)
		}
		repRows = append(repRows, []string{
			r.GetId(),
			strings.TrimPrefix(r.GetState().String(), "REPLICA_STATE_"),
			r.GetHost(),
			r.GetContainerId(),
			last,
		})
	}
	fmt.Fprintln(out, "\nReplicas:")
	if err := cliclient.RenderTable(out, repHeaders, repRows); err != nil {
		return err
	}

	// Routes table.
	rtHeaders := []string{"DOMAIN", "DEPLOYMENT", "SERVICE", "PORT", "TLS"}
	var rtRows [][]string
	for _, rt := range resp.GetRoutes() {
		tls := "off"
		if rt.GetTlsAuto() {
			tls = "auto"
		}
		rtRows = append(rtRows, []string{
			rt.GetDomain(),
			rt.GetDeployment(),
			rt.GetService(),
			strconv.FormatInt(int64(rt.GetPort()), 10),
			tls,
		})
	}
	fmt.Fprintln(out, "\nRoutes:")
	return cliclient.RenderTable(out, rtHeaders, rtRows)
}
