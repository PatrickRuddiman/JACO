package grpc

import (
	"context"
	"sync"

	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// The four delegating proxies below hold a swap-in slot that startSubsystems
// fills with the real grpcsrv-backed handler once OpenRaft populates state
// + raft. The InitGate admission interceptor blocks every RPC routed through
// the proxies until MarkInitialized fires, and MarkInitialized only fires
// after wireControlPlane has filled every target. The proxy methods can
// therefore assume target != nil — if it isn't, that's a programming error
// and a nil deref is the appropriate loud failure.
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
	return p.get().Issue(ctx, req)
}

func (p *tokensProxy) Revoke(ctx context.Context, req *pb.TokenRevokeRequest) (*pb.TokenRevokeResponse, error) {
	return p.get().Revoke(ctx, req)
}

func (p *tokensProxy) List(ctx context.Context, req *pb.TokenListRequest) (*pb.TokenListResponse, error) {
	return p.get().List(ctx, req)
}

type deployProxy struct {
	pb.UnimplementedDeployServer
	mu     sync.RWMutex
	target pb.DeployServer
	server *Server // back-reference so Logs can call streamLocalLogs
}

func (p *deployProxy) set(t pb.DeployServer) { p.mu.Lock(); p.target = t; p.mu.Unlock() }
func (p *deployProxy) get() pb.DeployServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

func (p *deployProxy) Apply(ctx context.Context, req *pb.ApplyRequest) (*pb.ApplyResponse, error) {
	return p.get().Apply(ctx, req)
}
func (p *deployProxy) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	return p.get().Rollback(ctx, req)
}
func (p *deployProxy) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	return p.get().Delete(ctx, req)
}
func (p *deployProxy) Status(ctx context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
	return p.get().Status(ctx, req)
}

// Logs is implemented locally on the daemon — the controlplane stub
// returns Unimplemented because it needs a dockerx handle + hostname,
// which only the daemon has. The handler streams local replicas
// directly and dials Internal.Logs on each peer hosting remote replicas
// of the same deployment/service, fanning everything into the operator
// stream.
func (p *deployProxy) Logs(req *pb.LogsRequest, stream pb.Deploy_LogsServer) error {
	return p.server.streamDeploymentLogs(req, stream)
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
	return p.get().Query(req, stream)
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
	return p.get().Subscribe(req, stream)
}

// wireControlPlane fills the four proxies once state + raft are populated.
// Called from Server.startSubsystems before MarkInitialized flips the
// InitGate open.
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
