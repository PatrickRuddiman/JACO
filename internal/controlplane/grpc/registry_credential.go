package grpcsrv

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// registryCredentialsServer implements jaco.v1.RegistryCredentials. Add and
// Remove gate on the raft leader so the operator command lands on a single
// authoritative path (followers forward at the CLI layer via --server).
// List reads local state — the credential is fully replicated, so any
// node returns the same set.
//
// SECURITY: the secret is held in raft and in-memory only. It is NEVER
// returned on the wire (List uses RegistryCredentialSummary) and NEVER
// appears in audit events (the FSM emits username + registry only).
type registryCredentialsServer struct {
	pb.UnimplementedRegistryCredentialsServer
	state *state.State
	raft  *raftnode.Node
}

// Add upserts the credential for the canonicalized registry host. Replaces
// any existing entry for the same host (rotation). The secret is required —
// callers wanting to delete should call Remove.
func (r *registryCredentialsServer) Add(ctx context.Context, req *pb.RegistryCredentialAddRequest) (*pb.RegistryCredentialAddResponse, error) {
	if r.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !r.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "add registry credential requires leader")
	}
	host := canonicalRegistryHost(req.GetRegistry())
	if host == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "registry is required")
	}
	if req.GetUsername() == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "username is required")
	}
	if len(req.GetSecret()) == 0 {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "secret is required")
	}

	now := timestamppb.Now()
	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       now,
		Payload: &pb.Command_RegistryCredentialUpsert{
			RegistryCredentialUpsert: &pb.RegistryCredentialUpsert{
				Credential: &pb.RegistryCredential{
					Registry:  host,
					Username:  req.GetUsername(),
					Secret:    req.GetSecret(),
					UpdatedAt: now,
				},
			},
		},
	}
	if err := applyRaftCommand(r.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.RegistryCredentialAddResponse{Registry: host, UpdatedAt: now}, nil
}

// Remove deletes the credential for the canonicalized registry host.
// Idempotent — removing an unknown host is not an error (the cluster does
// not need to leak which hosts are configured).
func (r *registryCredentialsServer) Remove(ctx context.Context, req *pb.RegistryCredentialRemoveRequest) (*pb.RegistryCredentialRemoveResponse, error) {
	if r.raft == nil {
		return nil, errorStatus(codes.Unavailable, "raft_unavailable", "raft not wired")
	}
	if !r.raft.IsLeader() {
		return nil, errorStatus(codes.Unavailable, "no_leader", "remove registry credential requires leader")
	}
	host := canonicalRegistryHost(req.GetRegistry())
	if host == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "registry is required")
	}
	cmd := &pb.Command{
		Identity: admission.IdentityFromContext(ctx),
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_RegistryCredentialRemove{
			RegistryCredentialRemove: &pb.RegistryCredentialRemove{Registry: host},
		},
	}
	if err := applyRaftCommand(r.raft, cmd); err != nil {
		return nil, errorStatus(codes.Internal, "raft_apply_failed", err.Error())
	}
	return &pb.RegistryCredentialRemoveResponse{}, nil
}

// List returns every credential's host + username + updated_at, with the
// secret stripped. The secret never crosses this wire — operators rotate by
// Add'ing a fresh secret rather than reading the existing one.
func (r *registryCredentialsServer) List(_ context.Context, _ *pb.RegistryCredentialListRequest) (*pb.RegistryCredentialListResponse, error) {
	all := r.state.RegistryCredentials.List()
	out := make([]*pb.RegistryCredentialSummary, 0, len(all))
	for _, c := range all {
		out = append(out, &pb.RegistryCredentialSummary{
			Registry:  c.GetRegistry(),
			Username:  c.GetUsername(),
			UpdatedAt: c.GetUpdatedAt(),
		})
	}
	return &pb.RegistryCredentialListResponse{Credentials: out}, nil
}

// canonicalRegistryHost mirrors the FSM helper of the same name so the gRPC
// handler validates / canonicalizes against the same key space the FSM
// stores under. Kept duplicated rather than exported because both call sites
// are internal and a future change to the canonicalization rules should land
// in lock-step.
func canonicalRegistryHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return ""
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "index.docker.io", "registry-1.docker.io", "registry.docker.io":
		return "docker.io"
	}
	return h
}
