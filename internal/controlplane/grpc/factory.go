package grpcsrv

import (
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// NewClusterServer returns a pb.ClusterServer backed by the controlplane
// implementation. The daemon uses this for the membership / token RPCs
// (NodeList, NodeRemove, IssueJoinToken, Backup, Restore) that aren't
// reimplemented in the daemon-side clusterServer.
func NewClusterServer(st *state.State, r *raftnode.Node) pb.ClusterServer {
	return &clusterServer{state: st, raft: r}
}

// NewTokensServer returns a pb.TokensServer backed by the given state + raft.
// Used by the daemon (internal/daemon/grpc.Server) to register the same
// operator-token CRUD surface controlplane/grpcsrv ships, without having to
// import the unexported server struct.
func NewTokensServer(st *state.State, r *raftnode.Node) pb.TokensServer {
	return &tokensServer{state: st, raft: r}
}

// NewDeployServer returns a pb.DeployServer.
func NewDeployServer(st *state.State, r *raftnode.Node) pb.DeployServer {
	return &deployServer{state: st, raft: r}
}

// NewAuditServer returns a pb.AuditServer.
func NewAuditServer(st *state.State, br *watch.Registry) pb.AuditServer {
	return &auditServer{state: st, brokers: br}
}

// NewWatchServer returns a pb.WatchServer.
func NewWatchServer(st *state.State, br *watch.Registry) pb.WatchServer {
	return &watchServer{state: st, brokers: br}
}

// NewRegistryCredentialsServer returns a pb.RegistryCredentialsServer backed
// by the given state + raft. Symmetric with NewTokensServer — Add/Remove gate
// on the leader; List reads local state.
func NewRegistryCredentialsServer(st *state.State, r *raftnode.Node) pb.RegistryCredentialsServer {
	return &registryCredentialsServer{state: st, raft: r}
}
