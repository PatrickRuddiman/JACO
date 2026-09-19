package grpc

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// subnetPoolSize is the number of /24s in the /16 IPAM pool — used for the
// utilization warning thresholds.
const subnetPoolSize = 256

// internalServer is the authenticated node-to-node service. Its command
// allowlist is deliberately narrower than the local leader's FSM interface.
type internalServer struct {
	pb.UnimplementedInternalServer
	server *Server
}

// Submit applies command_bytes to the local raft log. Returns the assigned
// log index. Refuses when raft isn't open (pre-Init) or this node isn't
// the leader — the caller is expected to retry against the actual leader.
func (i *internalServer) Submit(ctx context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
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
	cmd := &pb.Command{}
	if err := (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(req.GetCommandBytes(), cmd); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid_command")
	}
	if cmd.Payload == nil || len(cmd.ProtoReflect().GetUnknown()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid_command")
	}
	if err := i.server.authorizeInternalCommand(ctx, cmd); err != nil {
		logging.FromContext(ctx, i.server.srvLog).Info("internal command denied",
			"principal", admission.IdentityFromContext(ctx), logging.KeyReason, status.Convert(err).Message())
		return nil, err
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return nil, status.Error(codes.Internal, "marshal_command_failed")
	}
	idx, err := r.Apply(data, 0)
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
	if err := i.server.authorizeInternalLogs(stream.Context()); err != nil {
		return err
	}
	var mu sync.Mutex
	return i.server.streamLocalLogs(req, mutexSender{ctx: stream.Context(), send: func(line *pb.LogLine) error {
		mu.Lock()
		defer mu.Unlock()
		if err := i.server.authorizeInternalLogs(stream.Context()); err != nil {
			return err
		}
		return stream.Send(line)
	}})
}

// EnsureSubnet idempotently allocates the per-host /24 for
// (deployment, network, host) and returns its CIDR. Leader-only: followers
// get Unavailable/no_leader and are expected to retry against the leader.
// The leader is the single allocator, so the CIDR it computes lands in the
// SubnetAllocate command (the FSM just stores it) — keeping Apply
// deterministic across nodes.
func (i *internalServer) EnsureSubnet(ctx context.Context, req *pb.EnsureSubnetRequest) (*pb.EnsureSubnetResponse, error) {
	r := i.server.Raft()
	if r == nil {
		return nil, status.Error(codes.Unavailable, "raft_unavailable: daemon has no raft state")
	}
	if !r.IsLeader() {
		return nil, status.Error(codes.Unavailable, "no_leader: forward to the current leader")
	}
	if req.GetDeployment() == "" || req.GetNetwork() == "" || req.GetHost() == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment, network and host are required")
	}
	if err := i.server.authorizeInternalSubnet(ctx, req); err != nil {
		return nil, err
	}

	allocator := i.server.IPAMAllocator()
	if allocator == nil {
		return nil, status.Error(codes.Unavailable, "ipam_unavailable: allocator not initialized")
	}
	st := i.server.State()

	before := st.Subnets.Len()
	sn, err := allocator.Allocate(req.GetDeployment(), req.GetNetwork(), req.GetHost())
	if err != nil {
		if ipam.IsExhausted(err) {
			return nil, status.Errorf(codes.ResourceExhausted,
				"subnet_pool_exhausted: %s/%s on %s: %v",
				req.GetDeployment(), req.GetNetwork(), req.GetHost(), err)
		}
		return nil, status.Errorf(codes.Internal, "subnet allocate: %v", err)
	}

	// Log utilization only when a new subnet was actually written (not on an
	// idempotent hit), so the line fires exactly when the pool grows.
	if used := st.Subnets.Len(); used > before {
		i.logSubnetUtilization(used, req)
	}
	return &pb.EnsureSubnetResponse{Cidr: sn.GetCidr()}, nil
}

// logSubnetUtilization emits a WARN at >=75% and ERROR at >=90% pool usage,
// naming the tuple that crossed it. Below 75% it stays quiet.
func (i *internalServer) logSubnetUtilization(used int, req *pb.EnsureSubnetRequest) {
	pct := used * 100 / subnetPoolSize
	switch {
	case pct >= 90:
		i.server.srvLog.Error("subnet_pool_utilization critical",
			"pct", pct, "used", used, "size", subnetPoolSize,
			logging.KeyDeployment, req.GetDeployment(), "network", req.GetNetwork(), "host", req.GetHost())
	case pct >= 75:
		i.server.srvLog.Warn("subnet_pool_utilization high",
			"pct", pct, "used", used, "size", subnetPoolSize,
			logging.KeyDeployment, req.GetDeployment(), "network", req.GetNetwork(), "host", req.GetHost())
	}
}
