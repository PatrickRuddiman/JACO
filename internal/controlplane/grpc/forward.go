package grpcsrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// LeaderForwarder dials the current raft leader's Internal service so write
// RPCs handled on a follower can transparently submit through the leader.
// Construct one per server; Internal() returns a client every time.
type LeaderForwarder struct {
	raft   *raftnode.Node
	caPEM  []byte
	dialer func(addr string) (*grpc.ClientConn, error)
}

// NewLeaderForwarder builds a forwarder. caPEM is the cluster CA used to
// verify the peer's TLS cert when dialing.
func NewLeaderForwarder(r *raftnode.Node, caPEM []byte) *LeaderForwarder {
	f := &LeaderForwarder{raft: r, caPEM: caPEM}
	f.dialer = f.defaultDial
	return f
}

// EnsureLeader returns (client, isLocal, err) where:
//   - isLocal=true,  client=nil       — the local node is the raft leader; the
//     caller should handle the RPC directly.
//   - isLocal=false, client=non-nil   — a connection to the remote leader is
//     ready; the caller forwards through it.
//   - err non-nil                     — leader is unknown (no_leader) or dial
//     failed.
func (f *LeaderForwarder) EnsureLeader(_ context.Context) (pb.InternalClient, bool, error) {
	if f.raft == nil {
		return nil, false, statusErr(codes.Unavailable, "no_leader", "raft not wired")
	}
	if f.raft.IsLeader() {
		return nil, true, nil
	}
	leader := string(f.raft.Leader())
	if leader == "" {
		return nil, false, statusErr(codes.Unavailable, "no_leader", "leader address unknown")
	}
	conn, err := f.dialer(leader)
	if err != nil {
		return nil, false, statusErr(codes.Unavailable, "no_leader", fmt.Sprintf("dial leader %s: %v", leader, err))
	}
	return pb.NewInternalClient(conn), false, nil
}

func (f *LeaderForwarder) defaultDial(addr string) (*grpc.ClientConn, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

func statusErr(code codes.Code, errCode, msg string) error {
	st := status.New(code, errCode)
	if detailed, err := st.WithDetails(&pb.Error{Code: errCode, Message: msg}); err == nil {
		st = detailed
	}
	return st.Err()
}
