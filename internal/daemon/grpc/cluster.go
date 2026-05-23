package grpc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
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
	bindAddr string // raft TCP transport addr; cfg.ClusterAddr in production

	// mu guards the single-flight Init / Join. Concurrent Init calls would
	// race on raft store creation; serialize them at the handler layer.
	mu sync.Mutex
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

	hostname := c.hostname
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "hostname: %v", err)
		}
	}

	bindAddr := c.bindAddr
	if bindAddr == "" {
		// Single-node bootstrap with no peers can listen on loopback —
		// the recorded address only matters once peers join.
		bindAddr = "127.0.0.1:0"
	}

	result, err := bootstrap.Run(bootstrap.Options{
		DataDir:  c.dataDir,
		Name:     hostname,
		BindAddr: bindAddr,
	})
	if err != nil {
		if errors.Is(err, errRaftExists) || isRaftExistsErr(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "cluster_already_initialized: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "bootstrap: %v", err)
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

	hostname := c.hostname
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "hostname: %v", err)
		}
	}

	keyPEM, csrPEM, err := ca.GenerateNodeKeypair(hostname)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate keypair: %v", err)
	}

	// Dial peer with TLS skip-verify — the join_token is the trust anchor.
	// Once we have the CA in hand from the response, future RPCs validate
	// against it.
	peerCreds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	conn, err := grpc.NewClient(req.GetPeerAddr(), grpc.WithTransportCredentials(peerCreds))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dial peer: %v", err)
	}
	defer conn.Close()

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	advertise := c.bindAddr
	resp, err := pb.NewClusterClient(conn).NodeJoin(dialCtx, &pb.NodeJoinRequest{
		Name:          hostname,
		JoinToken:     req.GetJoinToken(),
		CsrPem:        csrPEM,
		AdvertiseAddr: advertise,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "node join rpc: %v", err)
	}

	if err := persistJoin(c.dataDir, hostname, advertise, keyPEM, resp); err != nil {
		return nil, status.Errorf(codes.Internal, "persist: %v", err)
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
// init — the InitGate's AllowedPreInit list lets it through.
func (c *clusterServer) Status(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	return &pb.ClusterStatusResponse{
		Initialized: c.gate.IsInitialized(),
	}, nil
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
