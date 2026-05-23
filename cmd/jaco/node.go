package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func init() {
	node := &cobra.Command{Use: "node", Short: "Cluster membership management"}
	node.AddCommand(nodeJoinCmd())
	node.AddCommand(nodeRemoveCmd())
	node.AddCommand(nodeListCmd())
	node.AddCommand(nodeIssueJoinTokenCmd())
	rootCmd.AddCommand(node)
}

// --- jaco node issue-join-token ----------------------------------------------

func nodeIssueJoinTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "issue-join-token",
		Short: "Issue a single-use join token (operator-authenticated)",
	}
	var (
		server string
		token  string
		caCert string
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN); required")
	c.Flags().StringVar(&caCert, "ca-cert", "", "path to cluster CA cert PEM; required")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if token == "" {
			token = os.Getenv("JACO_TOKEN")
		}
		if token == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		conn, err := dialServer(server, mustReadFile(caCert))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		resp, err := pb.NewClusterClient(conn).IssueJoinToken(ctx, &pb.IssueJoinTokenRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("Join token: %s\n", resp.GetToken())
		fmt.Printf("Expires in: 24h\n")
		fmt.Printf("Cluster CA (write to a file on the joining node):\n%s", resp.GetCaCert())
		return nil
	}
	return c
}

// --- jaco node join ----------------------------------------------------------

func nodeJoinCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "join --peer <host:port> --token <single-use>",
		Short: "Join this node to an existing cluster (RPCs the local jacod)",
		Long: `Join this node to an existing cluster.

This RPCs the local jacod over its unix socket; the daemon does all the
work (generates a CSR, dials the peer, exchanges via Cluster.NodeJoin for
a signed cert + cluster CA, persists everything under $JACO_DATA_DIR/node/,
opens its raft node, and connects to the existing cluster).`,
	}
	var (
		socket    string
		peer      string
		joinToken string
	)
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket")
	c.Flags().StringVar(&peer, "peer", "", "leader / any-cluster-member gRPC address (host:port); required")
	c.Flags().StringVar(&joinToken, "token", "", "single-use join token (or JACO_JOIN_TOKEN env)")
	_ = c.MarkFlagRequired("peer")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if joinToken == "" {
			joinToken = os.Getenv("JACO_JOIN_TOKEN")
		}
		if joinToken == "" {
			return fmt.Errorf("--token or JACO_JOIN_TOKEN env is required")
		}
		conn, err := dialDaemon(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runNodeJoin(ctx, pb.NewClusterClient(conn), peer, joinToken, os.Stdout)
	}
	return c
}

// runNodeJoin is the unit-testable body: takes a pb.ClusterClient so tests
// inject a fake without spinning up jacod.
func runNodeJoin(ctx context.Context, client pb.ClusterClient, peer, token string, out io.Writer) error {
	if _, err := client.Join(ctx, &pb.ClusterJoinRequest{
		PeerAddr:  peer,
		JoinToken: token,
	}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Joined cluster.")
	return nil
}

// --- jaco node remove --------------------------------------------------------

func nodeRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "remove <hostname>",
		Short: "Remove a node from the cluster",
		Args:  cobra.ExactArgs(1),
	}
	var (
		server, token, caCertPath string
		force                     bool
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	c.Flags().BoolVar(&force, "force", false, "skip drain enforcement")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, args []string) error {
		if token == "" {
			token = os.Getenv("JACO_TOKEN")
		}
		if token == "" {
			return fmt.Errorf("--token or JACO_TOKEN env is required")
		}
		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		if _, err := pb.NewClusterClient(conn).NodeRemove(ctx, &pb.NodeRemoveRequest{
			Hostname: args[0], Force: force,
		}); err != nil {
			return err
		}
		fmt.Printf("Removed node %s\n", args[0])
		return nil
	}
	return c
}

// --- jaco node list ----------------------------------------------------------

func nodeListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List cluster members",
	}
	var server, token, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM; required")
	_ = c.MarkFlagRequired("server")
	// ca-cert no longer required: v0 uses plaintext TCP

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if token == "" {
			token = os.Getenv("JACO_TOKEN")
		}
		conn, err := dialServer(server, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		resp, err := pb.NewClusterClient(conn).NodeList(ctx, &pb.NodeListRequest{})
		if err != nil {
			return err
		}
		for _, n := range resp.GetNodes() {
			fmt.Printf("%s\t%s\t%s\n", n.GetHostname(), n.GetAddress(), n.GetStatus())
		}
		return nil
	}
	return c
}

// --- shared dial helper ---------------------------------------------------

// dialServer dials the JACO control plane. The cross-host listener is
// always TLS (bootstrap self-signed pre-Init, cluster-CA-signed post-
// Init — task 41). When caCertPEM is non-empty the dial pins the
// cluster CA; otherwise it falls back to InsecureSkipVerify with a
// single one-line warning so v0 muscle-memory keeps working.
func dialServer(addr string, caCertPEM []byte) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("--server is required (host:port of any cluster node)")
	}
	if len(caCertPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCertPEM) {
			return nil, fmt.Errorf("--ca-cert did not parse as PEM")
		}
		return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool})))
	}
	fmt.Fprintln(os.Stderr, "warning: dialing without --ca-cert; server identity is not verified")
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
}

// silence unused if --ca-cert is the only path callers exercise.
var _ = insecure.NewCredentials

func mustReadFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		// Cobra will surface the runE error; this helper is only called when
		// the flag is required, so the file path must exist or we error out
		// during RunE. Use a placeholder that triggers a clean error.
		return []byte("__MISSING__:" + err.Error())
	}
	return b
}
