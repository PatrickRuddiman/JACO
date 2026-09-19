package grpcsrv

import (
	"google.golang.org/grpc/codes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ValidateJoinIdentity binds both enrollment handlers to the operator-approved
// token scope before certificate issuance or Raft membership changes.
func ValidateJoinIdentity(st *state.State, tok *pb.JoinToken, req *pb.NodeJoinRequest) error {
	if tok.GetNodeName() == "" {
		return errorStatus(codes.PermissionDenied, "join_token_unscoped", "reissue this legacy token with an approved node name and SANs")
	}
	if tok.GetNodeName() != req.GetName() {
		return errorStatus(codes.PermissionDenied, "join_identity_unapproved", "join token belongs to a different node")
	}
	if _, ok := st.Nodes.Get(req.GetName()); ok {
		return errorStatus(codes.AlreadyExists, "node_exists", "node is already a member; reconnect with its existing credentials")
	}
	if err := ca.ValidateNodeIdentity(tok.GetNodeName(), tok.GetAllowedSans(), req.GetAdvertiseAddr(), req.GetGrpcAddress()); err != nil {
		return errorStatus(codes.PermissionDenied, "join_identity_unapproved", err.Error())
	}
	return nil
}
