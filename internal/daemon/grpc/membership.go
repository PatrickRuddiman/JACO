package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// NodeJoin is the peer-facing RPC `jaco node join` (on jacod-2) calls to ask
// this jacod (jacod-1) to sign its CSR and add it to the raft membership.
// Unauthenticated — the single-use join_token in the body is the trust
// anchor (gated by InitGate.AllowedPreInit so peers can reach it pre-init
// too, even though it only succeeds once the leader has raft state).
func (c *clusterServer) NodeJoin(_ context.Context, req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
	r := c.server.Raft()
	if r == nil {
		return nil, status.Error(codes.Unavailable, "raft_unavailable: daemon has no raft state — run `jaco cluster init` first")
	}
	if !r.IsLeader() {
		return nil, status.Error(codes.Unavailable, "no_leader: NodeJoin must be sent to the leader")
	}
	if req.GetName() == "" || req.GetJoinToken() == "" || len(req.GetCsrPem()) == 0 || req.GetAdvertiseAddr() == "" {
		return nil, status.Error(codes.InvalidArgument, "validation_failed: name, join_token, csr_pem, advertise_addr required")
	}

	st := c.server.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "state_unavailable")
	}

	hash := sha256.Sum256([]byte(req.GetJoinToken()))
	key := hex.EncodeToString(hash[:])
	tok, ok := st.JoinTokens.Get(key)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "join_token_invalid")
	}
	if tok.GetConsumedAt() != nil {
		return nil, status.Error(codes.PermissionDenied, "join_token_consumed")
	}
	if exp := tok.GetExpiresAt(); exp != nil && exp.AsTime().Before(time.Now()) {
		return nil, status.Error(codes.PermissionDenied, "join_token_expired")
	}

	meta := st.Cluster.Get()
	if meta == nil || len(meta.GetCaCert()) == 0 || len(meta.GetCaKey()) == 0 {
		return nil, status.Error(codes.Internal, "ca_missing: cluster CA not in state")
	}
	signedCertPEM, err := ca.SignNodeCSR(req.GetCsrPem(), meta.GetCaCert(), meta.GetCaKey())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr_invalid: %v", err)
	}

	// Add the joining node as a voter. prevIndex=0 disables stale-config
	// check, matching the controlplane/grpcsrv implementation.
	addF := r.Raft.AddVoter(hraft.ServerID(req.GetName()), hraft.ServerAddress(req.GetAdvertiseAddr()), 0, 5*time.Second)
	if err := addF.Error(); err != nil {
		return nil, status.Errorf(codes.Internal, "raft_add_voter_failed: %v", err)
	}

	// Mark the token consumed, write the NodeJoin entity, and immediately
	// promote the new node from JOINING → READY so the scheduler will
	// place workloads on it. All three records land atomically in one
	// raft.Apply.
	//
	// The JOINING → READY auto-transition is the v0 behavior — once the
	// join token is consumed and AddVoter succeeded, the leader trusts
	// the joiner enough to schedule on. Drain-based gating (where the
	// new node has to prove health first) is a follow-up iter.
	now := timestamppb.Now()
	batch := &pb.Command{
		Identity: "join_token:" + key[:8],
		Ts:       now,
		Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
			{
				Identity: "join_token:" + key[:8],
				Ts:       now,
				Payload: &pb.Command_JoinTokenConsume{JoinTokenConsume: &pb.JoinTokenConsume{
					HashedSecret: hash[:],
				}},
			},
			{
				Identity: "join_token:" + key[:8],
				Ts:       now,
				Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{
					Hostname:        req.GetName(),
					Address:         req.GetAdvertiseAddr(),
					WireguardPubkey: req.GetWireguardPubkey(),
					GrpcAddress:     req.GetGrpcAddress(),
				}},
			},
			{
				Identity: "join_token:" + key[:8],
				Ts:       now,
				Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
					Hostname: req.GetName(),
					Status:   pb.NodeStatus_NODE_STATUS_READY,
				}},
			},
		}}},
	}
	data, err := proto.Marshal(batch)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal batch: %v", err)
	}
	if _, err := r.Apply(data, 5*time.Second); err != nil {
		return nil, status.Errorf(codes.Internal, "raft_apply_failed: %v", err)
	}

	// Build the peer list returned to the joiner: leader (self) + every
	// other known node, excluding the joiner itself.
	peerAddrs := []string{string(r.LocalAddr())}
	for _, n := range st.Nodes.List() {
		a := n.GetAddress()
		if a == "" || a == peerAddrs[0] || a == req.GetAdvertiseAddr() {
			continue
		}
		peerAddrs = append(peerAddrs, a)
	}

	return &pb.NodeJoinResponse{
		ClusterId:  meta.GetClusterId(),
		SignedCert: signedCertPEM,
		CaCert:     meta.GetCaCert(),
		PeerAddrs:  peerAddrs,
	}, nil
}

