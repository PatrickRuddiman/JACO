package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
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
	_ = c.MarkFlagRequired("ca-cert")

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
		Use:   "join",
		Short: "Join this node to an existing cluster (unauthenticated; gated by join token)",
	}
	var (
		address       string
		joinToken     string
		hostname      string
		caCertPath    string
		advertiseAddr string
	)
	c.Flags().StringVar(&address, "address", "", "leader gRPC address (host:port); required")
	c.Flags().StringVar(&joinToken, "join-token", "", "single-use join token (or JACO_JOIN_TOKEN); required")
	c.Flags().StringVar(&hostname, "name", "", "this node's hostname / raft local-id; required")
	c.Flags().StringVar(&caCertPath, "ca-cert", "", "path to cluster CA cert PEM (TLS pin); required")
	c.Flags().StringVar(&advertiseAddr, "advertise", "", "this node's raft transport address (host:port); required")
	_ = c.MarkFlagRequired("address")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("ca-cert")
	_ = c.MarkFlagRequired("advertise")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if joinToken == "" {
			joinToken = os.Getenv("JACO_JOIN_TOKEN")
		}
		if joinToken == "" {
			return fmt.Errorf("--join-token or JACO_JOIN_TOKEN env is required")
		}
		dataDir := os.Getenv("JACO_DATA_DIR")
		if dataDir == "" {
			dataDir = "/var/lib/jaco"
		}
		if err := os.MkdirAll(filepath.Join(dataDir, "node"), 0o700); err != nil {
			return fmt.Errorf("create node dir: %w", err)
		}

		keyPEM, csrPEM, err := ca.GenerateNodeKeypair(hostname)
		if err != nil {
			return fmt.Errorf("generate keypair: %w", err)
		}

		conn, err := dialServer(address, mustReadFile(caCertPath))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := pb.NewClusterClient(conn).NodeJoin(ctx, &pb.NodeJoinRequest{
			Name:          hostname,
			JoinToken:     joinToken,
			CsrPem:        csrPEM,
			AdvertiseAddr: advertiseAddr,
		})
		if err != nil {
			return err
		}

		// Persist the signed cert + CA + cluster_id + peers for the eventual
		// `jaco serve` startup (task 17).
		if err := os.WriteFile(filepath.Join(dataDir, "node", hostname+".key"), keyPEM, 0o600); err != nil {
			return fmt.Errorf("write key: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "node", hostname+".crt"), resp.GetSignedCert(), 0o644); err != nil {
			return fmt.Errorf("write cert: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "node", "ca.crt"), resp.GetCaCert(), 0o644); err != nil {
			return fmt.Errorf("write ca: %w", err)
		}
		joinMeta := map[string]any{
			"cluster_id": resp.GetClusterId(),
			"peer_addrs": resp.GetPeerAddrs(),
			"hostname":   hostname,
			"advertise":  advertiseAddr,
		}
		metaBytes, _ := json.MarshalIndent(joinMeta, "", "  ")
		if err := os.WriteFile(filepath.Join(dataDir, "node", "join.json"), metaBytes, 0o644); err != nil {
			return fmt.Errorf("write join meta: %w", err)
		}
		fmt.Printf("Joined cluster %s; run `jaco serve` to start the daemon.\n", resp.GetClusterId())
		return nil
	}
	return c
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
	_ = c.MarkFlagRequired("ca-cert")

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
	_ = c.MarkFlagRequired("ca-cert")

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

// --- shared dial helper (task 11 will replace with cliclient) ---------------

func dialServer(addr string, caCertPEM []byte) (*grpc.ClientConn, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("CA cert PEM did not parse")
	}
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

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
