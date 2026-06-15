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
	"google.golang.org/grpc/metadata"

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
	var noSystemdEnable bool
	c.Flags().StringVar(&socket, "socket", socketDefault(), "local jacod unix socket")
	c.Flags().StringVar(&clusterName, "name", "", "optional cluster name (defaults to a UUID)")
	c.Flags().BoolVar(&noSystemdEnable, "no-systemd-enable", false, "do not run `systemctl enable jaco` after init (this node won't auto-start after a reboot)")

	c.RunE = func(_ *cobra.Command, _ []string) error {
		conn, err := dialDaemon(socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runClusterInit(ctx, pb.NewClusterClient(conn), clusterName, !noSystemdEnable, os.Stdout)
	}
	return c
}

// runClusterInit is the unit-testable body: takes a pb.ClusterClient so
// tests inject a fake without spinning up jacod. When enableSystemd is true it
// enables jaco.service after a successful init so the freshly-committed node
// survives a reboot (issue #151).
func runClusterInit(ctx context.Context, client pb.ClusterClient, clusterName string, enableSystemd bool, out io.Writer) error {
	resp, err := client.Init(ctx, &pb.ClusterInitRequest{ClusterName: clusterName})
	if err != nil {
		return cliclient.FormatError(err)
	}
	fmt.Fprintln(out, "Cluster initialized.")
	fmt.Fprintln(out, "  cluster_id:    ", resp.GetClusterId())
	fmt.Fprintln(out, "  operator_token:", resp.GetOperatorToken())
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Save the operator token now — it cannot be recovered.")
	if enableSystemd {
		systemdEnabler(out)
	}
	return nil
}

func clusterStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the local jacod's cluster status",
		// Honors --output; renders -o json / -o yaml in runClusterStatus.
		Annotations: map[string]string{annotationHonorsOutput: "true"},
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
	return renderOutput(out, clusterStatusToView(resp), func() error {
		return renderClusterStatus(out, resp)
	})
}

// renderClusterStatus writes the human-readable cluster status view.
func renderClusterStatus(out io.Writer, resp *pb.ClusterStatusResponse) error {
	if !resp.GetInitialized() {
		fmt.Fprintln(out, "Status:    uninitialized")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Run `jaco cluster init` to start a new cluster,")
		fmt.Fprintln(out, "or `jaco node join` to join an existing one.")
		return nil
	}
	fmt.Fprintln(out, "Status:     initialized")
	leader := resp.GetLeader()
	if leader == "" {
		leader = "(no leader elected)"
	}
	fmt.Fprintf(out, "Leader:     %s\n", leader)
	fmt.Fprintf(out, "Raft index: %d\n", resp.GetRaftIndex())
	// Build a hostname -> suffrage lookup so the render below joins
	// in a single pass. suffrages is empty when this jacod isn't the
	// leader; we render "?" for the suffrage cell in that case so
	// operators see they're looking at follower-derived data rather
	// than misread an absent column.
	suffrage := suffrageByHostname(resp)
	fmt.Fprintf(out, "Nodes (%d):\n", len(resp.GetNodes()))
	for _, n := range resp.GetNodes() {
		status := strings.TrimPrefix(n.GetStatus().String(), "NODE_STATUS_")
		s, ok := suffrage[n.GetHostname()]
		if !ok {
			// Either the daemon isn't the leader (so suffrages is
			// empty) or this node hasn't reached the raft config
			// yet. Either way, don't claim a suffrage we can't see.
			s = "?"
		}
		fmt.Fprintf(out, "  - %s @ %s [%s, %s]\n", n.GetHostname(), n.GetAddress(), status, s)
	}
	return nil
}

// suffrageByHostname maps each node's hostname to its raft suffrage as an
// UPPERCASE table token ("VOTER"/"NONVOTER"); hostnames absent from the map
// have no observable suffrage (follower-derived or pre-config).
func suffrageByHostname(resp *pb.ClusterStatusResponse) map[string]string {
	suffrage := make(map[string]string, len(resp.GetSuffrages()))
	for _, s := range resp.GetSuffrages() {
		switch s.GetKind() {
		case pb.NodeSuffrage_KIND_VOTER:
			suffrage[s.GetHostname()] = "VOTER"
		case pb.NodeSuffrage_KIND_NONVOTER:
			suffrage[s.GetHostname()] = "NONVOTER"
		}
	}
	return suffrage
}

// --- structured (-o json / -o yaml) view --------------------------------------

// clusterStatusView is the JSON/YAML shape of `jaco cluster status`. Enum
// fields (node status, suffrage) are lowercase snake_case; suffrage is
// "voter" | "nonvoter" | "unknown" (the last when this jacod can't observe a
// node's raft suffrage).
type clusterStatusView struct {
	Initialized bool              `json:"initialized" yaml:"initialized"`
	Leader      string            `json:"leader" yaml:"leader"`
	RaftIndex   uint64            `json:"raft_index" yaml:"raft_index"`
	Nodes       []clusterNodeView `json:"nodes" yaml:"nodes"`
}

type clusterNodeView struct {
	Hostname string `json:"hostname" yaml:"hostname"`
	Address  string `json:"address" yaml:"address"`
	Status   string `json:"status" yaml:"status"`
	Suffrage string `json:"suffrage" yaml:"suffrage"`
}

// clusterStatusToView builds the structured cluster status. When uninitialized
// it returns {initialized:false} with an empty node list so scripts get a
// stable shape instead of the prose the table path prints.
func clusterStatusToView(resp *pb.ClusterStatusResponse) clusterStatusView {
	v := clusterStatusView{
		Initialized: resp.GetInitialized(),
		Leader:      resp.GetLeader(),
		RaftIndex:   resp.GetRaftIndex(),
		Nodes:       make([]clusterNodeView, 0, len(resp.GetNodes())),
	}
	suffrage := suffrageByHostname(resp)
	for _, n := range resp.GetNodes() {
		s, ok := suffrage[n.GetHostname()]
		if !ok {
			s = "unknown"
		}
		v.Nodes = append(v.Nodes, clusterNodeView{
			Hostname: n.GetHostname(),
			Address:  n.GetAddress(),
			Status:   enumString(n.GetStatus().String(), "NODE_STATUS_"),
			Suffrage: strings.ToLower(s),
		})
	}
	return v
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

// operatorAuth carries the flag values shared by every operator command that
// can dial either the local unix socket or a remote leader over TCP.
type operatorAuth struct {
	server string // --server (host:port); when set, dial TCP + bearer
	token  string // --token / JACO_TOKEN; only consulted on the TCP path
	caCert string // --ca-cert path; only consulted on the TCP path
	socket string // --socket / JACO_SOCKET; the on-node unix socket
}

// dialOperator resolves the transport for an operator command, honoring the
// locked design: if --server is set, dial TCP and require a bearer token
// (unchanged from v0); otherwise, if the local unix socket exists, dial it
// with NO token (the socket perms are the auth boundary); otherwise return a
// clear error telling the operator how to proceed.
//
// It returns the connection and a function that decorates an outgoing context
// with the bearer token when (and only when) the TCP path is in use. The
// caller is responsible for closing the returned conn.
func dialOperator(a operatorAuth) (*grpc.ClientConn, func(context.Context) context.Context, error) {
	if a.server != "" {
		token := a.token
		if token == "" {
			token = os.Getenv("JACO_TOKEN")
		}
		if token == "" {
			return nil, nil, fmt.Errorf("--token or JACO_TOKEN env is required when --server is set")
		}
		caCertPEM, err := readCACert(a.caCert)
		if err != nil {
			return nil, nil, err
		}
		// Never log the bearer token; only the selected transport + target.
		Logger().Debug("dialing remote leader over TCP", "server", a.server)
		conn, err := dialServer(a.server, caCertPEM)
		if err != nil {
			return nil, nil, err
		}
		withAuth := func(ctx context.Context) context.Context {
			return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		return conn, withAuth, nil
	}

	socket := a.socket
	if socket == "" {
		socket = socketDefault()
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, nil, fmt.Errorf(
			"no --server given and local socket %s is not available: pass --server (host:port) with --token, or run this command on a cluster node with the jaco daemon socket",
			socket)
	}
	Logger().Debug("dialing local daemon over unix socket", "socket", socket)
	conn, err := dialDaemon(socket)
	if err != nil {
		return nil, nil, err
	}
	// Over the unix socket the daemon trusts the peer by socket perms; no
	// bearer token is attached.
	return conn, func(ctx context.Context) context.Context { return ctx }, nil
}
