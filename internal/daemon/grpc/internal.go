package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// internalServer implements pb.InternalServer — the peer-to-peer service
// follower nodes use to forward raft.Apply work to the leader. Today it
// ships Submit (used by the runtime to forward ReplicaObserved updates).
// SignNodeCert + Logs land in later iters.
//
// Authentication: the peer mTLS scheme described in the slice isn't wired
// yet (v0 uses plaintext TCP, expecting Tailscale / WireGuard to wrap the
// wire), so Submit is in admission.UnauthMethods. The body's command_bytes
// is itself unstructured raft data — a malicious sender can apply arbitrary
// FSM commands. This is fine on a trusted overlay network and gets locked
// down once peer mTLS lands.
type internalServer struct {
	pb.UnimplementedInternalServer
	server *Server
}

// Submit applies command_bytes to the local raft log. Returns the assigned
// log index. Refuses when raft isn't open (pre-Init) or this node isn't
// the leader — the caller is expected to retry against the actual leader.
func (i *internalServer) Submit(_ context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	r := i.server.Raft()
	if r == nil {
		return nil, status.Error(codes.Unavailable, "raft_unavailable: daemon has no raft state")
	}
	if !r.IsLeader() {
		return nil, status.Error(codes.Unavailable, "no_leader: forward to the current leader")
	}
	if len(req.GetCommandBytes()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command_bytes is required")
	}
	idx, err := r.Apply(req.GetCommandBytes(), 0)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "raft_apply: %v", err)
	}
	return &pb.SubmitResponse{RaftIndex: idx}, nil
}

// Logs is the peer-facing fanout endpoint. A leader's Deploy.Logs handler
// dials this on every node hosting a relevant replica so cross-host log
// streams reach the operator. pb.Internal_LogsServer and
// pb.Deploy_LogsServer are both aliases for grpc.ServerStreamingServer
// [LogLine], so streamLocalLogs takes either.
func (i *internalServer) Logs(req *pb.LogsRequest, stream pb.Internal_LogsServer) error {
	return i.server.streamLocalLogs(req, stream)
}
