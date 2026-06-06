package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// The methods in this file forward to the controlplane impl that already
// owns the membership / token-mint / backup / restore logic. The daemon
// itself ships Init / Join / Status / NodeJoin (Init+Join are lifecycle
// transitions specific to the two-binary model; Status reads InitGate;
// NodeJoin is the peer-facing handler in membership.go). Everything else
// delegates here so jacod isn't missing operator-facing RPCs.

func (c *clusterServer) delegated(ctx context.Context) (pb.ClusterServer, error) {
	st := c.server.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "state_unavailable")
	}
	r := c.server.Raft()
	if r == nil {
		return nil, status.Error(codes.Unavailable, "raft_unavailable")
	}
	return grpcsrv.NewClusterServer(st, r), nil
}

// NodeList returns the cluster's node entities.
func (c *clusterServer) NodeList(ctx context.Context, req *pb.NodeListRequest) (*pb.NodeListResponse, error) {
	d, err := c.delegated(ctx)
	if err != nil {
		return nil, err
	}
	return d.NodeList(ctx, req)
}

// NodeRemove evicts a node from the raft membership.
func (c *clusterServer) NodeRemove(ctx context.Context, req *pb.NodeRemoveRequest) (*pb.NodeRemoveResponse, error) {
	d, err := c.delegated(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := d.NodeRemove(ctx, req)
	if err == nil {
		// Kick the voter-set reconciler so any post-remove demotion
		// (or backfill promotion of a nonvoter that should now be a
		// voter) lands without waiting for the next tick. The
		// controlplane handler already performs pre-removal demotion
		// where necessary (issue #143); the kick covers the case
		// where the leaver was a nonvoter and a backfill is needed
		// (e.g. 8 → 7 with a nonvoter leaver leaves voters_target=7
		// satisfied by an existing nonvoter promotion).
		if m := c.server.Membership(); m != nil {
			m.Kick()
		}
	}
	return resp, err
}

// IssueJoinToken mints a single-use 24h join token (operator-authenticated).
func (c *clusterServer) IssueJoinToken(ctx context.Context, req *pb.IssueJoinTokenRequest) (*pb.IssueJoinTokenResponse, error) {
	d, err := c.delegated(ctx)
	if err != nil {
		return nil, err
	}
	return d.IssueJoinToken(ctx, req)
}

// Backup streams a snapshot of cluster state.
func (c *clusterServer) Backup(req *pb.BackupRequest, stream pb.Cluster_BackupServer) error {
	d, err := c.delegated(stream.Context())
	if err != nil {
		return err
	}
	return d.Backup(req, stream)
}

// Restore reverses Backup.
func (c *clusterServer) Restore(stream pb.Cluster_RestoreServer) error {
	d, err := c.delegated(stream.Context())
	if err != nil {
		return err
	}
	return d.Restore(stream)
}
