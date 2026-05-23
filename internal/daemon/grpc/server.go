// Package grpc builds the gRPC server jacod listens on. v1 opens a unix
// socket listener; TLS-over-TCP for cross-host control lands once the
// daemon transitions through Init/Join and has a real cluster CA cert
// (later iters of task 38).
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cpadmission "github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	schedhealth "github.com/PatrickRuddiman/jaco/internal/scheduler/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Server bundles the daemon-side gRPC server with its unix socket listener
// + the InitGate that governs which RPCs accept while uninitialized.
type Server struct {
	gs           *grpc.Server
	listener     net.Listener // unix socket — local control
	tcpListener  net.Listener // cross-host control; nil when ListenAddr unset
	gate         *admission.InitGate
	socketPath   string
	tcpAddr      string // resolved listener address (the port may be 0 → ephemeral)

	cluster *clusterServer

	// Populated by OpenRaft after Cluster.Init or Cluster.Join lands. The
	// pre-OpenRaft state is "raft handle nil; RPCs that need raft return
	// Unavailable + cluster_uninitialized via the gate".
	raftMu  sync.RWMutex
	raft    *raftnode.Node
	state   *state.State
	brokers *watch.Registry
	fsm     *fsm.FSM

	// subsystemsCancel cancels every steady-state goroutine spawned by
	// OpenRaft (scheduler.Run, restarter.Run, etc). Reset to nil after Stop
	// drains subsystemsWG.
	subsystemsCancel context.CancelFunc
	subsystemsWG     sync.WaitGroup

	// logger receives subsystem errors so they surface in jacod's stderr
	// instead of disappearing into goroutine panics. nil → log.Default().
	logger *log.Logger

	// docker is the optional runtime engine handle. nil → no runtime
	// reconciler is spawned in startSubsystems.
	docker dockerx.Docker

	mu      sync.Mutex
	started bool
}

// Options configures Server.
type Options struct {
	// UnixSocketPath is the path to the local-control socket. Parent dir
	// is created if missing; existing socket file is removed.
	UnixSocketPath string

	// SocketMode is the permission mask applied to the socket file
	// (default 0o660 — owner+group rw).
	SocketMode os.FileMode

	// DataDir is the daemon's $JACO_DATA_DIR. Cluster.Init writes raft
	// state under $DataDir/raft and certs under $DataDir/node.
	DataDir string

	// ListenAddr is the cross-host control-plane listener (TCP). Peers
	// dial this for Cluster.{Status,Join} during cluster formation and
	// for ongoing operator RPCs. Empty → no cross-host listener (single
	// node only).
	//
	// v0 ships plaintext TCP — Tailscale / WireGuard is expected to wrap
	// the connection. TLS-with-cluster-CA is a follow-up iter.
	ListenAddr string

	// ClusterAddr is the raft TCP transport listen address. Used by
	// Cluster.Init to bind raft so peers can dial.
	ClusterAddr string

	// Hostname overrides os.Hostname() at handler time. Tests use this;
	// production leaves it empty.
	Hostname string

	// Logger receives subsystem errors. nil → log.Default(). Tests pass a
	// log.Logger writing to an io.Discard to suppress noise.
	Logger *log.Logger

	// Docker is the runtime engine handle. nil → skip runtime wiring (the
	// daemon still runs the control plane + scheduler, but doesn't create
	// containers). cmd/jacod wires dockerx.New; tests usually pass nil or
	// an in-memory fake.
	Docker dockerx.Docker
}

// New builds a Server. Doesn't start anything yet — call Serve.
func New(opts Options) (*Server, error) {
	if opts.UnixSocketPath == "" {
		return nil, errors.New("UnixSocketPath is required")
	}
	if opts.SocketMode == 0 {
		opts.SocketMode = 0o660
	}

	if err := os.MkdirAll(filepath.Dir(opts.UnixSocketPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket parent: %w", err)
	}
	// Remove any stale socket file from a previous run.
	_ = os.Remove(opts.UnixSocketPath)

	lis, err := net.Listen("unix", opts.UnixSocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", opts.UnixSocketPath, err)
	}
	if err := os.Chmod(opts.UnixSocketPath, opts.SocketMode); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	gate := admission.New()
	// stateAccessor returns the current state.State once OpenRaft has
	// populated it. Captured by lazyUnary / lazyStream below so the
	// admission interceptor is constructed (and reads the live token
	// store) on every post-init request.
	server := &Server{} // forward-declare so the closures capture it
	stateAccessor := func() *state.State {
		server.raftMu.RLock()
		defer server.raftMu.RUnlock()
		return server.state
	}
	lazyUnary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// UnauthMethods (Status, NodeJoin) bypass the bearer check
		// regardless of whether state is populated — Status doesn't read
		// state.Tokens and NodeJoin authenticates via the body's
		// join_token. This keeps Cluster.Status callable even in tests
		// that flip the gate manually without driving OpenRaft.
		if cpadmission.UnauthMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		st := stateAccessor()
		if st == nil {
			// Defensive: post-init handler ran before state hookup. Should
			// not happen because OpenRaft populates state before flipping
			// the gate, but fail closed rather than skipping admission.
			return nil, errStateUnavailable
		}
		return cpadmission.UnaryInterceptor(st)(ctx, req, info, handler)
	}
	lazyStream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if cpadmission.UnauthMethods[info.FullMethod] {
			return handler(srv, ss)
		}
		st := stateAccessor()
		if st == nil {
			return errStateUnavailable
		}
		return cpadmission.StreamInterceptor(st)(srv, ss, info, handler)
	}
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(gate.UnaryInterceptor(lazyUnary)),
		grpc.StreamInterceptor(gate.StreamInterceptor(lazyStream)),
	)

	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Optional cross-host TCP listener. Empty ListenAddr → single-node
	// daemon (unix socket only). We open this NOW so a failure surfaces
	// before Init/Join rather than mid-flight.
	var tcpLis net.Listener
	var tcpAddr string
	if opts.ListenAddr != "" {
		tl, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			_ = lis.Close()
			return nil, fmt.Errorf("listen tcp %s: %w", opts.ListenAddr, err)
		}
		tcpLis = tl
		tcpAddr = tl.Addr().String()
	}

	*server = Server{
		gs:          gs,
		listener:    lis,
		tcpListener: tcpLis,
		gate:        gate,
		socketPath:  opts.UnixSocketPath,
		tcpAddr:     tcpAddr,
		logger:      logger,
		docker:      opts.Docker,
	}
	cluster := &clusterServer{
		gate:     gate,
		dataDir:  opts.DataDir,
		bindAddr: opts.ClusterAddr,
		hostname: opts.Hostname,
		server:   server,
	}
	server.cluster = cluster
	pb.RegisterClusterServer(gs, cluster)
	pb.RegisterInternalServer(gs, &internalServer{server: server})

	return server, nil
}

// OpenRaft opens the persisted raft state and populates the Server's
// raft/state/brokers/fsm handles. Called from Cluster.Init (after
// bootstrap.Run) and from Cluster.Join (after persistJoin). Idempotent —
// returns nil + leaves existing handles alone if already opened.
func (s *Server) OpenRaft(hostname, bindAddr string) error {
	s.raftMu.Lock()
	defer s.raftMu.Unlock()
	if s.raft != nil {
		return nil
	}
	if hostname == "" {
		return fmt.Errorf("OpenRaft: hostname is required")
	}
	if bindAddr == "" {
		return fmt.Errorf("OpenRaft: bindAddr is required")
	}
	if s.cluster == nil || s.cluster.dataDir == "" {
		return fmt.Errorf("OpenRaft: dataDir is required")
	}

	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)

	node, err := raftnode.New(raftnode.Config{
		DataDir:   s.cluster.dataDir,
		BindAddr:  bindAddr,
		LocalID:   hostname,
		Bootstrap: false, // raft state already on disk
		FSM:       f,
	})
	if err != nil {
		return fmt.Errorf("raftnode.New: %w", err)
	}

	s.raft = node
	s.state = st
	s.brokers = brokers
	s.fsm = f

	s.startSubsystems(node, st, brokers, hostname)
	return nil
}

// startSubsystems spins up every per-host goroutine that depends on raft +
// state being open. Called from OpenRaft under s.raftMu (so the goroutines
// see fully-populated handles). All goroutines self-cancel when
// s.subsystemsCancel fires from Stop.
//
// v0 wires scheduler.Run (desired-state reconciler, leader-only) and
// scheduler/health.Restarter.Run (restart policy, leader-only). Runtime,
// discovery, and ingress wiring lands in subsequent iters.
func (s *Server) startSubsystems(node *raftnode.Node, st *state.State, brokers *watch.Registry, hostname string) {
	ctx, cancel := context.WithCancel(context.Background())
	s.subsystemsCancel = cancel

	apply := func(cmd []byte) error {
		_, err := node.Apply(cmd, 0)
		return err
	}

	sched := scheduler.New(st, brokers, node, apply)
	s.subsystemsWG.Add(1)
	go func() {
		defer s.subsystemsWG.Done()
		if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("scheduler.Run exited: %v", err)
		}
	}()

	restarter := schedhealth.New(st, brokers, node, apply)
	s.subsystemsWG.Add(1)
	go func() {
		defer s.subsystemsWG.Done()
		if err := restarter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("scheduler/health.Restarter.Run exited: %v", err)
		}
	}()

	// Runtime reconciler: skipped when no Docker handle was injected (the
	// daemon still serves the control plane + scheduler in that mode). On
	// hosts where docker is unreachable, opts.Docker should already be
	// nil — cmd/jacod logs a warning + continues.
	if s.docker != nil {
		// SubmitFn writes ReplicaObserved back through raft.Apply directly
		// (leader path). Follower-side Internal.Submit forwarding lands in
		// a later iter — for now the runtime works on whichever node is
		// also the leader.
		submit := func(ctx context.Context, obs *pb.ReplicaObserved) error {
			cmd := &pb.Command{
				Identity: "runtime",
				Payload:  &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: obs}},
			}
			data, err := proto.Marshal(cmd)
			if err != nil {
				return fmt.Errorf("marshal: %w", err)
			}
			_, err = node.Apply(data, 0)
			return err
		}
		rec := reconciler.New(s.docker, st, brokers, hostname, health.SubmitFn(submit), s.logger)
		s.subsystemsWG.Add(1)
		go func() {
			defer s.subsystemsWG.Done()
			if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("runtime.Reconciler.Run exited: %v", err)
			}
		}()
	}
}

// Raft returns the daemon's raft handle. nil pre-OpenRaft.
func (s *Server) Raft() *raftnode.Node {
	s.raftMu.RLock()
	defer s.raftMu.RUnlock()
	return s.raft
}

// State returns the daemon's state.State. nil pre-OpenRaft.
func (s *Server) State() *state.State {
	s.raftMu.RLock()
	defer s.raftMu.RUnlock()
	return s.state
}

// Serve blocks until Stop is called or one of the listeners errors. When a
// cross-host TCP listener is configured, it runs alongside the unix socket
// on the same grpc.Server (so Cluster RPCs are visible identically on both
// transports).
func (s *Server) Serve() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("server already started")
	}
	s.started = true
	s.mu.Unlock()

	errs := make(chan error, 2)
	go func() {
		err := s.gs.Serve(s.listener)
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
		errs <- err
	}()
	if s.tcpListener != nil {
		go func() {
			err := s.gs.Serve(s.tcpListener)
			if errors.Is(err, grpc.ErrServerStopped) {
				err = nil
			}
			errs <- err
		}()
	}
	// Return the first non-nil error (or nil if both shut down cleanly).
	first := <-errs
	if s.tcpListener != nil {
		// Drain the other one so its goroutine doesn't leak.
		select {
		case <-errs:
		default:
		}
	}
	return first
}

// Stop performs a graceful shutdown.
func (s *Server) Stop(ctx context.Context) {
	stopped := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		s.gs.Stop()
	}
	_ = os.Remove(s.socketPath)

	// Cancel steady-state goroutines BEFORE shutting raft so they don't
	// race against a nil raft handle. WaitGroup drains with a 5s budget;
	// after that we proceed regardless to avoid hanging the daemon.
	s.raftMu.Lock()
	cancel := s.subsystemsCancel
	s.subsystemsCancel = nil
	s.raftMu.Unlock()
	if cancel != nil {
		cancel()
		done := make(chan struct{})
		go func() { s.subsystemsWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.logger.Printf("subsystems shutdown timed out after 5s")
		}
	}

	// Close raft + release the bolt-store file lock so a follow-on jacod
	// boot (or test) can re-open the same data dir.
	s.raftMu.Lock()
	if s.raft != nil {
		_ = s.raft.Shutdown()
		s.raft = nil
	}
	s.raftMu.Unlock()
}

// Gate returns the InitGate so callers can flip MarkInitialized after a
// successful Init / Join.
func (s *Server) Gate() *admission.InitGate { return s.gate }

// SocketPath returns the path the daemon is listening on.
func (s *Server) SocketPath() string { return s.socketPath }

// TCPAddr returns the resolved cross-host listener address (after net.Listen
// substituted any :0 port). Empty when ListenAddr was unset.
func (s *Server) TCPAddr() string { return s.tcpAddr }

// errStateUnavailable is returned by the lazy admission interceptor when it
// fires before OpenRaft has populated state. Should be unreachable in
// practice because the gate doesn't dispatch post-init handlers until
// MarkInitialized fires (which OpenRaft does after assigning state).
var errStateUnavailable = status.Error(codes.Unavailable, "state_unavailable: daemon raft state not populated yet")
