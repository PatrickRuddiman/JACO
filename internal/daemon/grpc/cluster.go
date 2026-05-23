package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// clusterServer is the daemon-side Cluster service. Only Init/Join/Status
// are wired in this iter; the rest will return Unimplemented for now and
// land in later iters when raft is up and steady-state goroutines run.
type clusterServer struct {
	pb.UnimplementedClusterServer
	gate *admission.InitGate
}

// Init is a placeholder — iter 4 wires the real bootstrap.
func (c *clusterServer) Init(_ context.Context, _ *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"cluster_init_unimplemented: this jacod build doesn't yet implement Cluster.Init (task 38 iter 4)")
}

// Join is a placeholder — iter 5 wires the real raft join.
func (c *clusterServer) Join(_ context.Context, _ *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"cluster_join_unimplemented: this jacod build doesn't yet implement Cluster.Join (task 38 iter 5)")
}

// Status reports the daemon's initialized flag. Always callable, even pre-
// init — that's the whole point of the AllowedPreInit allow-list.
func (c *clusterServer) Status(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	return &pb.ClusterStatusResponse{
		Initialized: c.gate.IsInitialized(),
	}, nil
}
