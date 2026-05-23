package grpcsrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// joinTokenTTL is how long a freshly issued join token stays valid.
const joinTokenTTL = 24 * time.Hour

// IssueJoinToken mints a single-use 32-byte token, raft-applies a
// JoinTokenIssue command storing only its hash, and returns the cleartext
// token (returned exactly once) plus the cluster CA cert for the joiner to
// pin its TLS dial. Requires operator authentication.
func (c *clusterServer) IssueJoinToken(ctx context.Context, _ *pb.IssueJoinTokenRequest) (*pb.IssueJoinTokenResponse, error) {
	if c.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !c.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "issue join token requires leader")
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, errorStatus(codes.Internal, "rand_failed", err.Error())
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))

	expiresAt := timestamppb.New(time.Now().Add(joinTokenTTL))
	if err := c.applyCommand(&pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_JoinTokenIssue{JoinTokenIssue: &pb.JoinTokenIssue{
			HashedSecret: hash[:],
			ExpiresAt:    expiresAt,
		}},
	}); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}

	meta := c.state.Cluster.Get()
	var caCert []byte
	if meta != nil {
		caCert = meta.GetCaCert()
	}

	leaders := []string{string(c.raft.Leader())}
	for _, n := range c.state.Nodes.List() {
		if n.GetAddress() != "" && n.GetAddress() != leaders[0] {
			leaders = append(leaders, n.GetAddress())
		}
	}

	return &pb.IssueJoinTokenResponse{
		Token:       token,
		CaCert:      caCert,
		LeaderAddrs: leaders,
	}, nil
}

// NodeJoin accepts a CSR + single-use join_token, signs the CSR with the
// cluster CA, raft.AddVoters the joiner, and writes a NodeJoin command into
// the FSM. Unauthenticated (the join_token in the body is the gate; see
// admission.UnauthMethods).
func (c *clusterServer) NodeJoin(_ context.Context, req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
	if c.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !c.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "join requires leader")
	}
	if req.GetName() == "" || req.GetJoinToken() == "" || len(req.GetCsrPem()) == 0 || req.GetAdvertiseAddr() == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "name, join_token, csr_pem, advertise_addr required")
	}

	hash := sha256.Sum256([]byte(req.GetJoinToken()))
	key := hex.EncodeToString(hash[:])
	tok, ok := c.state.JoinTokens.Get(key)
	if !ok {
		return nil, errorStatus(codes.PermissionDenied, "join_token_invalid", "unknown join token")
	}
	if tok.GetConsumedAt() != nil {
		return nil, errorStatus(codes.PermissionDenied, "join_token_consumed", "join token already used")
	}
	if exp := tok.GetExpiresAt(); exp != nil && exp.AsTime().Before(time.Now()) {
		return nil, errorStatus(codes.PermissionDenied, "join_token_expired", "join token expired")
	}

	meta := c.state.Cluster.Get()
	if meta == nil || len(meta.GetCaCert()) == 0 || len(meta.GetCaKey()) == 0 {
		return nil, errorStatus(codes.Internal, "ca_missing", "cluster CA not present in state")
	}
	signedCertPEM, err := ca.SignNodeCSR(req.GetCsrPem(), meta.GetCaCert(), meta.GetCaKey())
	if err != nil {
		return nil, errorStatus(codes.InvalidArgument, "csr_invalid", err.Error())
	}

	// Add the new server to raft. prevIndex=0 disables stale-config check.
	addF := c.raft.Raft.AddVoter(hraft.ServerID(req.GetName()), hraft.ServerAddress(req.GetAdvertiseAddr()), 0, 5*time.Second)
	if err := addF.Error(); err != nil {
		return nil, errorStatus(codes.Internal, "raft_add_voter_failed", err.Error())
	}

	// Mark the token consumed and write the NodeJoin entity in one batch.
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
					Hostname:              req.GetName(),
					Address:               req.GetAdvertiseAddr(),
					ServerCertFingerprint: nil,
					WireguardPubkey:       req.GetWireguardPubkey(),
				}},
			},
		}}},
	}
	if err := c.applyCommand(batch); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}

	peerAddrs := []string{string(c.raft.LocalAddr())}
	for _, n := range c.state.Nodes.List() {
		if a := n.GetAddress(); a != "" && a != peerAddrs[0] && a != req.GetAdvertiseAddr() {
			peerAddrs = append(peerAddrs, a)
		}
	}

	return &pb.NodeJoinResponse{
		ClusterId:  meta.GetClusterId(),
		SignedCert: signedCertPEM,
		CaCert:     meta.GetCaCert(),
		PeerAddrs:  peerAddrs,
	}, nil
}

// NodeRemove evicts hostname from the raft configuration and writes a
// NodeRemove command. Requires operator auth.
//
// force=true (the explicit "rip it out") path: skips drain entirely and
// applies raft.RemoveServer + NodeRemove immediately.
//
// force=false (the default, graceful): runs drain.Plan to compute the
// replica migrations off hostname, applies a ReplicaDesiredUpsert for
// each (which the scheduler will then materialize on remaining nodes),
// waits up to 60s for the new replicas to reach RUNNING in
// state.ReplicasObserved, then applies RemoveServer + NodeRemove. If
// the wait times out, returns FailedPrecondition with drain_timeout so
// the operator can decide whether to retry or use --force.
func (c *clusterServer) NodeRemove(ctx context.Context, req *pb.NodeRemoveRequest) (*pb.NodeRemoveResponse, error) {
	if c.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !c.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "remove requires leader")
	}
	if req.GetHostname() == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "hostname required")
	}

	if !req.GetForce() {
		if err := c.drainHost(ctx, req.GetHostname()); err != nil {
			return nil, err
		}
	}

	rmF := c.raft.Raft.RemoveServer(hraft.ServerID(req.GetHostname()), 0, 5*time.Second)
	if err := rmF.Error(); err != nil {
		return nil, errorStatus(codes.Internal, "raft_remove_failed", err.Error())
	}
	if err := c.applyCommand(&pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_NodeRemove{NodeRemove: &pb.NodeRemove{Hostname: req.GetHostname()}},
	}); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.NodeRemoveResponse{}, nil
}

// applyCommand marshals cmd and submits via raft.Apply on this server's
// raft node. Delegates to applyRaftCommand so tokensServer (and any future
// servers) share the same marshal-then-apply step.
func (c *clusterServer) applyCommand(cmd *pb.Command) error {
	return applyRaftCommand(c.raft, cmd)
}

// applyRaftCommand marshals cmd and submits via raft.Apply on r.
func applyRaftCommand(r *raftnode.Node, cmd *pb.Command) error {
	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	if _, err := r.Apply(data, 5*time.Second); err != nil {
		return err
	}
	return nil
}

func errorStatus(code codes.Code, errCode, msg string) error {
	st := status.New(code, errCode)
	if detailed, err := st.WithDetails(&pb.Error{Code: errCode, Message: msg}); err == nil {
		st = detailed
	}
	return st.Err()
}
