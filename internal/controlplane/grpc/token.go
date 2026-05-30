package grpcsrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// tokensServer implements the jaco.v1.Tokens gRPC service. All methods require
// operator authentication (the admission interceptor handles that gate; this
// type just consumes IdentityFromContext when it needs to audit).
//
// SECURITY: Issue returns the cleartext token to the caller exactly once and
// it is never logged anywhere. Only the SHA-256 hash is persisted in
// state.Tokens via the FSM. hashed_secret is stripped from every List
// response.
type tokensServer struct {
	pb.UnimplementedTokensServer
	state *state.State
	raft  *raftnode.Node
}

// Issue mints a new operator token under identity. Returns the cleartext token
// (hex of 32 random bytes) so the caller can save it; the FSM stores only its
// SHA-256 hash.
func (t *tokensServer) Issue(ctx context.Context, req *pb.TokenIssueRequest) (*pb.TokenIssueResponse, error) {
	if t.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !t.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "issue requires leader")
	}
	if req.GetIdentity() == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "identity is required")
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, errorStatus(codes.Internal, "rand_failed", err.Error())
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	issuedAt := timestamppb.Now()

	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       issuedAt,
		Payload: &pb.Command_TokenIssue{TokenIssue: &pb.TokenIssue{
			Identity:         req.GetIdentity(),
			HashedSecret:     hash[:],
			AllowsPrivileged: req.GetAllowsPrivileged(),
		}},
	}
	if err := applyRaftCommand(t.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}

	return &pb.TokenIssueResponse{
		Identity: req.GetIdentity(),
		Token:    token,
		IssuedAt: issuedAt,
	}, nil
}

// Revoke marks a token revoked. Idempotent — revoking a non-existent identity
// is not an error (the cluster doesn't need to leak which identities exist).
func (t *tokensServer) Revoke(ctx context.Context, req *pb.TokenRevokeRequest) (*pb.TokenRevokeResponse, error) {
	if t.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !t.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "revoke requires leader")
	}
	if req.GetIdentity() == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "identity is required")
	}

	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_TokenRevoke{TokenRevoke: &pb.TokenRevoke{
			Identity: req.GetIdentity(),
		}},
	}
	if err := applyRaftCommand(t.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.TokenRevokeResponse{}, nil
}

// List returns every known token's identity, issued_at, revoked_at. The
// hashed_secret is intentionally stripped before responding.
func (t *tokensServer) List(_ context.Context, _ *pb.TokenListRequest) (*pb.TokenListResponse, error) {
	all := t.state.Tokens.List()
	out := make([]*pb.TokenInfo, 0, len(all))
	for _, tok := range all {
		out = append(out, &pb.TokenInfo{
			Identity:         tok.GetIdentity(),
			IssuedAt:         tok.GetIssuedAt(),
			RevokedAt:        tok.GetRevokedAt(),
			AllowsPrivileged: tok.GetAllowsPrivileged(),
		})
	}
	return &pb.TokenListResponse{Tokens: out}, nil
}
