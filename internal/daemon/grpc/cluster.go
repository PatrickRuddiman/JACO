package grpc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	"github.com/PatrickRuddiman/jaco/internal/daemon/netdetect"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// clusterServer is the daemon-side Cluster service. Init wires the real
// bootstrap; Join (iter 5) and the steady-state operational RPCs (later
// iters) will fill in once raft + the other services are wired through
// the daemon entry.
type clusterServer struct {
	pb.UnimplementedClusterServer
	gate     *admission.InitGate
	dataDir  string
	hostname string // override for tests; defaults to os.Hostname() at handler time
	bindAddr string // raft TCP transport bind; cfg.ClusterAddr in production
	// advertiseAddr is the host:port peers should dial for this node's raft
	// transport. Equals bindAddr when the operator pinned an explicit IP;
	// when bindAddr is 0.0.0.0:N, cmd/jacod sets this to the auto-detected
	// interface IP + N. Empty → fall back to bindAddr (the legacy single-IP
	// path used by tests with 127.0.0.1:port).
	advertiseAddr string
	server        *Server // back-reference so handlers can call OpenRaft / read raft handle

	// mu guards the single-flight Init / Join. Concurrent Init calls would
	// race on raft store creation; serialize them at the handler layer.
	mu sync.Mutex
}

// effectiveHostname returns the test override when set, else os.Hostname().
func (c *clusterServer) effectiveHostname() (string, error) {
	if c.hostname != "" {
		return c.hostname, nil
	}
	return os.Hostname()
}

// effectiveBindAddr returns the configured raft bind, defaulting to
// 127.0.0.1:0 (ephemeral) when unset — same as bootstrap.Run's own default.
func (c *clusterServer) effectiveBindAddr() string {
	if c.bindAddr != "" {
		return c.bindAddr
	}
	return "127.0.0.1:0"
}

// effectiveAdvertiseAddr returns the host:port peers should be told to
// dial for raft. Falls back to effectiveBindAddr when no explicit advertise
// was configured — appropriate for tests and any deployment where bind is
// already a routable IP.
func (c *clusterServer) effectiveAdvertiseAddr() string {
	if c.advertiseAddr != "" {
		return c.advertiseAddr
	}
	return c.effectiveBindAddr()
}

// Init creates a brand-new single-node cluster on this daemon. Refuses when
// raft state already exists on disk (idempotency — the operator should run
// `jaco node join` on a node that already booted into a cluster).
func (c *clusterServer) Init(_ context.Context, req *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gate.IsInitialized() {
		return nil, status.Error(codes.FailedPrecondition,
			"cluster_already_initialized: this daemon already has raft state")
	}
	if raftExists(c.dataDir) {
		// In-memory gate said no, but disk says yes — race between two
		// fresh-boot Inits, or a previous Init crashed mid-write. Refuse
		// rather than corrupt state.
		return nil, status.Error(codes.FailedPrecondition,
			"cluster_already_initialized: raft state on disk; restart jacod to pick it up")
	}

	hostname, err := c.effectiveHostname()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hostname: %v", err)
	}
	bindAddr := c.effectiveBindAddr()
	advertiseAddr := c.effectiveAdvertiseAddr()

	result, err := bootstrap.Run(bootstrap.Options{
		DataDir:             c.dataDir,
		Name:                hostname,
		BindAddr:            bindAddr,
		AdvertiseAddr:       advertiseAddr,
		ListenAdvertiseAddr: c.server.TCPAdvertiseAddr(),
	})
	if err != nil {
		if errors.Is(err, errRaftExists) || isRaftExistsErr(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "cluster_already_initialized: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "bootstrap: %v", err)
	}

	// Re-open raft for steady-state operation. bootstrap.Run shut down its
	// own raft handle to release the bolt file lock.
	if err := c.server.OpenRaft(hostname, bindAddr, advertiseAddr); err != nil {
		return nil, status.Errorf(codes.Internal, "open raft post-bootstrap: %v", err)
	}

	c.gate.MarkInitialized()

	return &pb.ClusterInitResponse{
		ClusterId:     result.ClusterID,
		OperatorToken: result.OperatorToken,
	}, nil
}

// Join asks the daemon to add this node to an existing cluster. Generates
// a local keypair + CSR, dials the peer over TLS (skip-verify; the
// join_token is the trust anchor), exchanges via Cluster.NodeJoin for the
// signed cert + CA + raft peer list, persists everything under
// $DataDir/node/, and flips the InitGate. The local raft node + steady-
// state goroutines come up in iter 6 once a real daemon entry exists.
func (c *clusterServer) Join(ctx context.Context, req *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gate.IsInitialized() {
		return nil, status.Error(codes.FailedPrecondition,
			"cluster_already_initialized: this daemon already has raft state")
	}
	if raftExists(c.dataDir) {
		return nil, status.Error(codes.FailedPrecondition,
			"cluster_already_initialized: raft state on disk; restart jacod to pick it up")
	}
	if req.GetPeerAddr() == "" {
		return nil, status.Error(codes.InvalidArgument, "peer_addr is required")
	}
	if req.GetJoinToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "join_token is required")
	}

	hostname, err := c.effectiveHostname()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hostname: %v", err)
	}

	// Collect IP SANs for the joiner's CSR. The signed cert is presented
	// by this node's gRPC TLS listener, so the gRPC listen advertise IP is
	// the load-bearing one — clients dial the gRPC listener by that IP.
	// The raft transport advertise IP is included too when distinct; dedupe
	// in ca.GenerateNodeKeypair collapses identical values into one SAN.
	// Every up, non-loopback local interface IP is added as well so an
	// operator reaching this node by any other interface (second NIC, the
	// VNet address when advertise picked Tailscale, etc.) doesn't hit a TLS
	// SAN mismatch.
	advertise := c.effectiveAdvertiseAddr()
	grpcAdvertise := c.server.TCPAdvertiseAddr()
	var clusterIP, listenIP net.IP
	if advertise != "" {
		if host, _, splitErr := net.SplitHostPort(advertise); splitErr == nil {
			clusterIP = net.ParseIP(host) // nil when host is a DNS name
		}
	}
	if grpcAdvertise != "" {
		if host, _, splitErr := net.SplitHostPort(grpcAdvertise); splitErr == nil {
			listenIP = net.ParseIP(host)
		}
	}
	sanIPs := append(netdetect.LocalIPs(), listenIP, clusterIP)
	keyPEM, csrPEM, err := ca.GenerateNodeKeypair(hostname, sanIPs...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate keypair: %v", err)
	}

	// Dial peer with TLS skip-verify — pre-Init this joiner can't yet
	// verify the peer's bootstrap cert, but the join_token in the body is
	// the trust anchor. Once Cluster.NodeJoin returns the cluster CA,
	// subsequent operator RPCs validate against that pin.
	creds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	conn, err := grpc.NewClient(req.GetPeerAddr(), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dial peer: %v", err)
	}
	defer conn.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// grpcAdvertise (computed above for the cert's IP SAN) is the joiner's
	// own cross-host gRPC listener address, used by the leader (or any
	// other node) to dial back via Internal.Submit. Resolved at startup by
	// cmd/jacod via netdetect when listen_addr is unspecified; falls back
	// to the bound address when explicit.
	resp, err := pb.NewClusterClient(conn).NodeJoin(dialCtx, &pb.NodeJoinRequest{
		Name:          hostname,
		JoinToken:     req.GetJoinToken(),
		CsrPem:        csrPEM,
		AdvertiseAddr: advertise,
		GrpcAddress:   grpcAdvertise,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "node join rpc: %v", err)
	}

	if err := persistJoin(c.dataDir, hostname, advertise, keyPEM, resp); err != nil {
		return nil, status.Errorf(codes.Internal, "persist: %v", err)
	}

	// NB: opening raft post-Join requires the cluster's raft to already
	// know about us (raft.AddVoter on the leader fires inside the peer's
	// Cluster.NodeJoin handler). We open our local raft node here against
	// the existing peer set; raft will catch up via snapshot+log.
	if err := c.server.OpenRaft(hostname, c.effectiveBindAddr(), advertise); err != nil {
		return nil, status.Errorf(codes.Internal, "open raft post-join: %v", err)
	}

	c.gate.MarkInitialized()
	return &pb.ClusterJoinResponse{}, nil
}

// persistJoin writes the joining node's certs + cluster CA + join metadata
// under $dataDir/node/.
func persistJoin(dataDir, hostname, advertise string, keyPEM []byte, resp *pb.NodeJoinResponse) error {
	nodeDir := filepath.Join(dataDir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return fmt.Errorf("create node dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, hostname+".key"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, hostname+".crt"), resp.GetSignedCert(), 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "ca.crt"), resp.GetCaCert(), 0o644); err != nil {
		return fmt.Errorf("write ca: %w", err)
	}
	meta := map[string]any{
		"cluster_id": resp.GetClusterId(),
		"peer_addrs": resp.GetPeerAddrs(),
		"hostname":   hostname,
		"advertise":  advertise,
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(nodeDir, "join.json"), metaBytes, 0o644); err != nil {
		return fmt.Errorf("write join meta: %w", err)
	}
	return nil
}

// Status reports the daemon's initialized flag. Always callable, even pre-
// init — the InitGate's AllowedPreInit list lets it through. Post-Init the
// response carries the raft leader + last index + the cluster's node list,
// plus the per-node raft suffrage (voter / nonvoter) when this jacod is
// the leader (issue #143). On followers the suffrages field is empty —
// raft.GetConfiguration can still be called, but the values would be
// stale across an election, so we refuse to mislead operators.
func (c *clusterServer) Status(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	resp := &pb.ClusterStatusResponse{
		Initialized: c.gate.IsInitialized(),
	}
	raftNode := c.server.Raft()
	if raftNode != nil {
		resp.Leader = string(raftNode.Raft.Leader())
		resp.RaftIndex = raftNode.Raft.LastIndex()
	}
	if st := c.server.State(); st != nil {
		resp.Nodes = st.Nodes.List()
	}
	if raftNode != nil && raftNode.IsLeader() {
		f := raftNode.GetConfiguration()
		if f.Error() == nil {
			servers := f.Configuration().Servers
			resp.Suffrages = make([]*pb.NodeSuffrage, 0, len(servers))
			for _, s := range servers {
				resp.Suffrages = append(resp.Suffrages, &pb.NodeSuffrage{
					Hostname: string(s.ID),
					Kind:     suffrageKind(s.Suffrage),
				})
			}
		}
	}
	return resp, nil
}

// suffrageKind maps the raft library's ServerSuffrage onto our wire
// enum. Staging (legacy) collapses to NONVOTER — it's "nonvoter that
// can be promoted" in the lib, but the lib itself recommends using
// Nonvoter instead, and operator-facing output should treat them the
// same.
func suffrageKind(s hraft.ServerSuffrage) pb.NodeSuffrage_Kind {
	switch s {
	case hraft.Voter:
		return pb.NodeSuffrage_KIND_VOTER
	case hraft.Nonvoter, hraft.Staging:
		return pb.NodeSuffrage_KIND_NONVOTER
	default:
		return pb.NodeSuffrage_KIND_UNSPECIFIED
	}
}

// raftExists reports whether $dataDir/raft/log.db exists. bootstrap.Run does
// its own check too; we mirror it here so the handler returns the right
// status code (FailedPrecondition vs Internal).
func raftExists(dataDir string) bool {
	if dataDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dataDir, "raft", "log.db"))
	return err == nil
}

// isRaftExistsErr matches the literal error text bootstrap.Run produces on
// pre-existing state. (bootstrap.Run uses fmt.Errorf; no sentinel.)
func isRaftExistsErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "raft state already exists")
}

// errRaftExists is a placeholder sentinel; bootstrap.Run doesn't actually
// export one. Kept for forward-compat if it gains one.
var errRaftExists = fmt.Errorf("raft state already exists")

var _ = filepath.Join // silence unused-import when stat path changes
var _ = errors.Is     // silence; kept for the errRaftExists sentinel
