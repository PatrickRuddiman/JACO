// Package grpc builds the gRPC server jacod listens on. v1 opens a unix
// socket listener; TLS-over-TCP for cross-host control lands once the
// daemon transitions through Init/Join and has a real cluster CA cert
// (later iters of task 38).
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"

	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Server bundles the daemon-side gRPC server with its unix socket listener
// + the InitGate that governs which RPCs accept while uninitialized.
type Server struct {
	gs        *grpc.Server
	listener  net.Listener
	gate      *admission.InitGate
	socketPath string

	cluster *clusterServer

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

	cluster := &clusterServer{
		gate:     gate,
		dataDir:  opts.DataDir,
		bindAddr: opts.ClusterAddr,
		hostname: opts.Hostname,
	}
	pb.RegisterClusterServer(gs, cluster)

	return &Server{
		gs:         gs,
		listener:   lis,
		gate:       gate,
		socketPath: opts.UnixSocketPath,
		cluster:    cluster,
	}, nil
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
}

// Gate returns the InitGate so callers can flip MarkInitialized after a
// successful Init / Join.
func (s *Server) Gate() *admission.InitGate { return s.gate }

// SocketPath returns the path the daemon is listening on.
func (s *Server) SocketPath() string { return s.socketPath }
