package grpcsrv

import (
	"context"

	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// clusterServer is the Cluster service implementation. Methods that depend on
// later tasks (Bootstrap, IssueJoinToken, NodeJoin, NodeRemove, Backup,
// Restore) fall through to UnimplementedClusterServer and return
// codes.Unimplemented. Status is wired here because the admission integration
// test (task 06) needs a real RPC to exercise.
type clusterServer struct {
	pb.UnimplementedClusterServer
	state *state.State
	raft  *raftnode.Node
}

// Status returns a snapshot of the cluster: known Node entities, the current
// raft leader address, and the local last-applied index when available.
func (c *clusterServer) Status(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	resp := &pb.ClusterStatusResponse{
		Nodes: c.state.Nodes.List(),
	}
	if c.raft != nil {
		resp.Leader = string(c.raft.Leader())
	}
	return resp, nil
}

// NodeList returns every known Node entity. Implemented here for parity with
// the CLI's `jaco node list` (task 07); the test in task 06 doesn't call it.
func (c *clusterServer) NodeList(_ context.Context, _ *pb.NodeListRequest) (*pb.NodeListResponse, error) {
	return &pb.NodeListResponse{Nodes: c.state.Nodes.List()}, nil
}
