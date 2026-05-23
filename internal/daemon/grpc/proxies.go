package grpc

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// The four delegating proxies below hold a swap-in slot that startSubsystems
// fills with the real grpcsrv-backed handler once OpenRaft populates state
// + raft. Pre-OpenRaft (which only happens for tests that flip the gate
// manually), every method returns Unavailable.
//
// Why proxies and not direct registration in startSubsystems: pb.Register*
// must happen before grpc.Server.Serve starts dispatching. The daemon
// constructs and Serves the gRPC server in New() so the services need to
// be registered there too. The proxies bridge the lifecycle.

type tokensProxy struct {
	pb.UnimplementedTokensServer
	mu     sync.RWMutex
	target pb.TokensServer
}

func (p *tokensProxy) set(t pb.TokensServer) { p.mu.Lock(); p.target = t; p.mu.Unlock() }

func (p *tokensProxy) get() pb.TokensServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

func (p *tokensProxy) Issue(ctx context.Context, req *pb.TokenIssueRequest) (*pb.TokenIssueResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Issue(ctx, req)
}

func (p *tokensProxy) Revoke(ctx context.Context, req *pb.TokenRevokeRequest) (*pb.TokenRevokeResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Revoke(ctx, req)
}

func (p *tokensProxy) List(ctx context.Context, req *pb.TokenListRequest) (*pb.TokenListResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.List(ctx, req)
}

type deployProxy struct {
	pb.UnimplementedDeployServer
	mu     sync.RWMutex
	target pb.DeployServer
}

func (p *deployProxy) set(t pb.DeployServer) { p.mu.Lock(); p.target = t; p.mu.Unlock() }
func (p *deployProxy) get() pb.DeployServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

func (p *deployProxy) Apply(ctx context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Apply(ctx, req)
}
func (p *deployProxy) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Rollback(ctx, req)
}
func (p *deployProxy) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Delete(ctx, req)
}
func (p *deployProxy) Status(ctx context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
	t := p.get()
	if t == nil {
		return nil, errUnavail
	}
	return t.Status(ctx, req)
}
func (p *deployProxy) Logs(req *pb.LogsRequest, stream pb.Deploy_LogsServer) error {
	t := p.get()
	if t == nil {
		return errUnavail
	}
	return t.Logs(req, stream)
}

type auditProxy struct {
	pb.UnimplementedAuditServer
	mu     sync.RWMutex
	target pb.AuditServer
}

func (p *auditProxy) set(t pb.AuditServer) { p.mu.Lock(); p.target = t; p.mu.Unlock() }
func (p *auditProxy) get() pb.AuditServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

func (p *auditProxy) Query(req *pb.AuditQueryRequest, stream pb.Audit_QueryServer) error {
	t := p.get()
	if t == nil {
		return errUnavail
	}
	return t.Query(req, stream)
}

type watchProxy struct {
	pb.UnimplementedWatchServer
	mu     sync.RWMutex
	target pb.WatchServer
}

func (p *watchProxy) set(t pb.WatchServer) { p.mu.Lock(); p.target = t; p.mu.Unlock() }
func (p *watchProxy) get() pb.WatchServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

func (p *watchProxy) Subscribe(req *pb.SubscribeRequest, stream pb.Watch_SubscribeServer) error {
	t := p.get()
	if t == nil {
		return errUnavail
	}
	return t.Subscribe(req, stream)
}

// errUnavail is the placeholder returned by every proxy method while the
// target hasn't been set yet (i.e. before OpenRaft has populated state +
// raft). In normal operation the InitGate catches this earlier and returns
// cluster_uninitialized; the proxies fail-closed defensively.
var errUnavail = status.Error(codes.Unavailable, "state_unavailable: daemon raft state not populated yet")

// wireControlPlane fills the four proxies once state + raft are populated.
// Called from Server.startSubsystems.
func (s *Server) wireControlPlane() {
	if s.tokens != nil {
		s.tokens.set(grpcsrv.NewTokensServer(s.state, s.raft))
	}
	if s.deploy != nil {
		s.deploy.set(grpcsrv.NewDeployServer(s.state, s.raft))
	}
	if s.audit != nil {
		s.audit.set(grpcsrv.NewAuditServer(s.state, s.brokers))
	}
	if s.watch != nil {
		s.watch.set(grpcsrv.NewWatchServer(s.state, s.brokers))
	}
}
