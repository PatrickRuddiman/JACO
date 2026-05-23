package grpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
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

// Join is a placeholder — iter 5 wires the real raft join.
func (c *clusterServer) Join(_ context.Context, _ *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"cluster_join_unimplemented: this jacod build doesn't yet implement Cluster.Join (task 38 iter 5)")
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
