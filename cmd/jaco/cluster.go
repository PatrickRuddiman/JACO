package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// DefaultDaemonSocket is the standard path the daemon listens on (matches
// internal/daemon/config.DefaultUnixSocket). Operators can override via
// --socket on any cluster-* subcommand or via JACO_SOCKET.
const DefaultDaemonSocket = "/var/run/jaco/jaco.sock"

func init() {
	rootCmd.AddCommand(clusterCmd())
}

func clusterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cluster",
		Short: "Local-daemon cluster control (init, status)",
	}
	c.AddCommand(clusterInitCmd(), clusterStatusCmd())
	return c
}

func clusterInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new JACO cluster on this node",
		Long: `Initialize a new JACO cluster on this node.

This RPCs the local jacod over its unix socket; the daemon does the actual
work (generates the cluster id + CA, raft-bootstraps as a single voter,
mints the first operator token). The token is printed once on success and
cannot be recovered.`,
	}
	var socket, clusterName string
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket")
	c.Flags().StringVar(&clusterName, "name", "", "optional cluster name (defaults to a UUID)")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		conn, err := dialDaemon(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runClusterInit(ctx, pb.NewClusterClient(conn), clusterName, os.Stdout)
	}
	return c
}

// runClusterInit is the unit-testable body: takes a pb.ClusterClient so
// tests inject a fake without spinning up jacod.
func runClusterInit(ctx context.Context, client pb.ClusterClient, clusterName string, out io.Writer) error {
	resp, err := client.Init(ctx, &pb.ClusterInitRequest{ClusterName: clusterName})
	if err != nil {
		return cliclient.FormatError(err)
	}
	fmt.Fprintln(out, "Cluster initialized.")
	fmt.Fprintln(out, "  cluster_id:    ", resp.GetClusterId())
	fmt.Fprintln(out, "  operator_token:", resp.GetOperatorToken())
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Save the operator token now — it cannot be recovered.")
	return nil
}

func clusterStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the local jacod's cluster status",
	}
	var socket string
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		conn, err := dialDaemon(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return runClusterStatus(ctx, pb.NewClusterClient(conn), os.Stdout)
	}
	return c
}

// runClusterStatus is the unit-testable body.
func runClusterStatus(ctx context.Context, client pb.ClusterClient, out io.Writer) error {
	resp, err := client.Status(ctx, &pb.ClusterStatusRequest{})
	if err != nil {
		return cliclient.FormatError(err)
	}
	if !resp.GetInitialized() {
		fmt.Fprintln(out, "Status:    uninitialized")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Run `jaco cluster init` to start a new cluster,")
		fmt.Fprintln(out, "or `jaco node join` to join an existing one.")
		return nil
	}
	fmt.Fprintln(out, "Status:     initialized")
	fmt.Fprintf(out, "Leader:     %s\n", strDefault(resp.GetLeader(), "(no leader elected)"))
	fmt.Fprintf(out, "Raft index: %d\n", resp.GetRaftIndex())
	fmt.Fprintf(out, "Nodes (%d):\n", len(resp.GetNodes()))
	for _, n := range resp.GetNodes() {
		fmt.Fprintf(out, "  - %s @ %s [%s]\n", n.GetHostname(), n.GetAddress(),
			strings.TrimPrefix(n.GetStatus().String(), "NODE_STATUS_"))
	}
	return nil
}

func strDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func socketDefault() string {
	if v := os.Getenv("JACO_SOCKET"); v != "" {
		return v
	}
	return DefaultDaemonSocket
}

// dialDaemon opens a non-TLS gRPC connection to the local jacod unix
// socket. The unix socket is the trust boundary (mode 0660 owned by the
// jaco group); no token / cert needed.
func dialDaemon(socketPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
}
