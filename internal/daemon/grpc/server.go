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
	"google.golang.org/protobuf/proto"

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
	gs         *grpc.Server
	listener   net.Listener
	gate       *admission.InitGate
	socketPath string

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
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(gate.UnaryInterceptor(nil)),
		grpc.StreamInterceptor(gate.StreamInterceptor(nil)),
	)

	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	server := &Server{
		gs:         gs,
		listener:   lis,
		gate:       gate,
		socketPath: opts.UnixSocketPath,
		logger:     logger,
		docker:     opts.Docker,
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

// Serve blocks until Stop is called or the listener errors.
func (s *Server) Serve() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("server already started")
	}
	s.started = true
	s.mu.Unlock()

	err := s.gs.Serve(s.listener)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
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
