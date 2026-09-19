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
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
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
		server   string
		token    string
		caCert   string
		socket   string
		showCA   bool
		nodeName string
		sans     []string
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCert, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().BoolVar(&showCA, "show-ca", false, "append the cluster CA certificate to the output")
	c.Flags().StringVar(&nodeName, "node-name", "", "exact hostname of the joining daemon; required")
	c.Flags().StringArrayVar(&sans, "san", nil, "additional approved DNS name or IP address; repeat for each advertised/private identity")
	_ = c.MarkFlagRequired("node-name")

	c.RunE = func(cmd *cobra.Command, _ []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: token, caCert: caCert, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = withAuth(ctx)
		resp, err := pb.NewClusterClient(conn).IssueJoinToken(ctx, &pb.IssueJoinTokenRequest{
			NodeName: nodeName, AllowedSans: sans,
		})
		if err != nil {
			return cliclient.FormatError(err)
		}
		// The join instructions reference the peer address joining nodes will
		// dial. Over the socket we have no --server to echo, so fall back to a
		// placeholder the operator must fill in with this node's reachable
		// address.
		peerAddr := server
		if peerAddr == "" {
			peerAddr = "<this-node-host:port>"
		}
		fmt.Fprint(cmd.OutOrStdout(), formatIssueJoinToken(peerAddr, resp.GetToken(), 24*time.Hour, string(resp.GetCaCert()), showCA))
		return nil
	}
	return c
}

// formatIssueJoinToken returns the human-readable output for issue-join-token.
// It is a pure function so it can be unit-tested without a live gRPC server.
// When showCA is true the cluster CA PEM is appended after the main output.
func formatIssueJoinToken(server, token string, expires time.Duration, ca string, showCA bool) string {
	// Format the expiry as a compact human string: whole hours → "24h",
	// otherwise fall back to the standard Duration.String() representation.
	expiryStr := expires.String()
	if h := expires.Hours(); h == float64(int(h)) && h > 0 {
		expiryStr = fmt.Sprintf("%dh", int(h))
	}
	out := fmt.Sprintf("Join token issued. Provision the cluster CA independently on the joining node, then run:\n\n  sudo jaco node join --peer=%s --token=%s --ca-cert=/path/to/cluster-ca.crt\n\nToken expires in %s (single-use).\n",
		server, token, expiryStr)
	if showCA && ca != "" {
		out += fmt.Sprintf("\nCluster CA (transfer using an independently authenticated channel):\n%s", ca)
	}
	return out
}

// --- jaco node join ----------------------------------------------------------

func nodeJoinCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "join --peer <host:port> --token <single-use> --ca-cert <trusted-ca.pem>",
		Short: "Join this node to an existing cluster (RPCs the local jacod)",
		Long: `Join this node to an existing cluster.

This RPCs the local jacod over its unix socket; the daemon does all the
work (generates a CSR, verifies the peer using the independently provisioned
cluster CA before sending the join token, validates the returned certificate,
persists credentials under $JACO_DATA_DIR/node/, and opens its raft node).
The join token must approve this daemon's hostname and advertised DNS/IP names.
There is no token-only or trust-on-first-use enrollment mode.`,
	}
	var (
		socket    string
		peer      string
		joinToken string
		caCert    string

		noSystemdEnable bool
	)
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket")
	c.Flags().StringVar(&peer, "peer", "", "leader gRPC address (host:port); required")
	c.Flags().StringVar(&joinToken, "token", "", "single-use join token (or JACO_JOIN_TOKEN env)")
	c.Flags().StringVar(&caCert, "ca-cert", defaultCACertPath(), "path to independently provisioned cluster CA PEM (or JACO_CA_CERT)")
	c.Flags().BoolVar(&noSystemdEnable, "no-systemd-enable", false, "do not run `systemctl enable jaco` after join (this node won't auto-start after a reboot)")
	_ = c.MarkFlagRequired("peer")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if joinToken == "" {
			joinToken = os.Getenv("JACO_JOIN_TOKEN")
		}
		if joinToken == "" {
			return fmt.Errorf("--token or JACO_JOIN_TOKEN env is required")
		}
		caPEM, err := readCACert(caCert)
		if err != nil {
			return err
		}
		conn, err := dialDaemon(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runNodeJoin(ctx, pb.NewClusterClient(conn), peer, joinToken, caPEM, !noSystemdEnable, os.Stdout)
	}
	return c
}

// runNodeJoin is the unit-testable body: takes a pb.ClusterClient so tests
// inject a fake without spinning up jacod. When enableSystemd is true it
// enables jaco.service after a successful join so the freshly-committed node
// survives a reboot (issue #151).
func runNodeJoin(ctx context.Context, client pb.ClusterClient, peer, token string, caPEM []byte, enableSystemd bool, out io.Writer) error {
	if len(caPEM) == 0 {
		return fmt.Errorf("--ca-cert must contain independently provisioned cluster trust")
	}
	if _, err := client.Join(ctx, &pb.ClusterJoinRequest{
		PeerAddr:  peer,
		JoinToken: token,
		CaCert:    caPEM,
	}); err != nil {
		return cliclient.FormatError(err)
	}
	fmt.Fprintln(out, "Joined cluster.")
	if enableSystemd {
		systemdEnabler(out)
	}
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
		server, token, caCertPath, socket string
		force                             bool
	)
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); off-node only — omit to use the local socket")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN); required with --server")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket (used when --server is omitted)")
	c.Flags().BoolVar(&force, "force", false, "skip drain enforcement")

	c.RunE = func(_ *cobra.Command, args []string) error {
		conn, withAuth, err := dialOperator(operatorAuth{server: server, token: token, caCert: caCertPath, socket: socket})
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = withAuth(ctx)
		if _, err := pb.NewClusterClient(conn).NodeRemove(ctx, &pb.NodeRemoveRequest{
			Hostname: args[0], Force: force,
		}); err != nil {
			return cliclient.FormatError(err)
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
		// Honors --output; renders -o json / -o yaml via renderNodeList.
		Annotations: map[string]string{annotationHonorsOutput: "true"},
	}
	var server, token, caCertPath string
	c.Flags().StringVar(&server, "server", "", "leader address (host:port); required")
	c.Flags().StringVar(&token, "token", "", "operator bearer token (or JACO_TOKEN)")
	c.Flags().StringVar(&caCertPath, "ca-cert", defaultCACertPath(), "path to cluster CA cert PEM")
	_ = c.MarkFlagRequired("server")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		if token == "" {
			token = os.Getenv("JACO_TOKEN")
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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		resp, err := pb.NewClusterClient(conn).NodeList(ctx, &pb.NodeListRequest{})
		if err != nil {
			return cliclient.FormatError(err)
		}
		return renderNodeList(os.Stdout, resp)
	}
	return c
}

// renderNodeList dispatches the node list on --output. The table path keeps
// the tab-separated `hostname address status` rows; json/yaml emit a
// {nodes:[...]} object with lowercase snake_case status values.
func renderNodeList(out io.Writer, resp *pb.NodeListResponse) error {
	return renderOutput(out, nodeListToView(resp), func() error {
		for _, n := range resp.GetNodes() {
			fmt.Fprintf(out, "%s\t%s\t%s\n", n.GetHostname(), n.GetAddress(), n.GetStatus())
		}
		return nil
	})
}

// nodeListView is the JSON/YAML shape of `jaco node list`. Wrapped in an
// object (not a bare array) so top-level fields can be added without breaking
// consumers.
type nodeListView struct {
	Nodes []nodeView `json:"nodes" yaml:"nodes"`
}

type nodeView struct {
	Hostname string `json:"hostname" yaml:"hostname"`
	Address  string `json:"address" yaml:"address"`
	Status   string `json:"status" yaml:"status"`
}

func nodeListToView(resp *pb.NodeListResponse) nodeListView {
	v := nodeListView{Nodes: make([]nodeView, 0, len(resp.GetNodes()))}
	for _, n := range resp.GetNodes() {
		v.Nodes = append(v.Nodes, nodeView{
			Hostname: n.GetHostname(),
			Address:  n.GetAddress(),
			Status:   enumString(n.GetStatus().String(), "NODE_STATUS_"),
		})
	}
	return v
}

// --- shared dial helper ---------------------------------------------------

// dialServer authenticates the control plane using explicitly provisioned CA
// trust and the DNS/IP identity in addr. Missing trust is never an opt-out.
func dialServer(addr string, caCertPEM []byte) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("--server is required (host:port of any cluster node)")
	}
	if len(caCertPEM) == 0 {
		return nil, fmt.Errorf("--ca-cert or JACO_CA_CERT is required; server authentication cannot be disabled")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("--ca-cert did not parse as PEM")
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS12,
	})))
}

// readCACert reads the PEM-encoded CA certificate at path. When the file does
// not exist it returns the documented user-facing error so operators know how
// to fix it. An empty path cannot disable server authentication.
func readCACert(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("--ca-cert or JACO_CA_CERT is required; server authentication cannot be disabled")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ca cert not found at %s — pass --ca-cert or set JACO_CA_CERT", path)
		}
		return nil, fmt.Errorf("read CA cert %s: %w", path, err)
	}
	return b, nil
}
