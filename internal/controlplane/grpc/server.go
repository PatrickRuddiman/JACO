// Package grpcsrv wires the JACO control-plane gRPC server: TLS terminated by
// the per-node CA-signed cert, the admission interceptor on every RPC, and
// service registrations (Cluster + others added as their tasks land).
//
// The package name is grpcsrv so it can be imported alongside
// google.golang.org/grpc without an alias.
package grpcsrv

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Options carries everything NewServer needs.
type Options struct {
	BindAddr string         // e.g. "0.0.0.0:7000"; "127.0.0.1:0" picks a free port (tests)
	NodeCert []byte         // PEM
	NodeKey  []byte         // PEM
	CACert   []byte         // PEM (cluster CA — used to verify client certs in future)
	State    *state.State
	Brokers  *watch.Registry // needed for follow-mode Audit.Query streams
	Raft     *raftnode.Node  // optional; nil for tests that bypass raft
}

// Server wraps the listening gRPC server.
type Server struct {
	grpc     *grpc.Server
	listener net.Listener
	opts     Options
}

// NewServer constructs and binds the listener. Call Serve() to start serving
// (it blocks until Stop()).
func NewServer(opts Options) (*Server, error) {
	if opts.State == nil {
		return nil, fmt.Errorf("Options.State is required")
	}
	if len(opts.NodeCert) == 0 || len(opts.NodeKey) == 0 {
		return nil, fmt.Errorf("Options.NodeCert and NodeKey are required")
	}
	if opts.BindAddr == "" {
		return nil, fmt.Errorf("Options.BindAddr is required")
	}

	cert, err := tls.X509KeyPair(opts.NodeCert, opts.NodeKey)
	if err != nil {
		return nil, fmt.Errorf("load node TLS keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if len(opts.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CACert) {
			return nil, fmt.Errorf("parse CA cert PEM")
		}
		// CA pool is held for future client-cert verification; we don't gate
		// requests on client certs in v1 (bearer token is the gate).
		tlsCfg.ClientCAs = pool
	}

	lis, err := net.Listen("tcp", opts.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", opts.BindAddr, err)
	}

	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(admission.UnaryInterceptor(opts.State)),
		grpc.StreamInterceptor(admission.StreamInterceptor(opts.State)),
	)

	pb.RegisterClusterServer(gs, &clusterServer{state: opts.State, raft: opts.Raft})
	pb.RegisterTokensServer(gs, &tokensServer{state: opts.State, raft: opts.Raft})
	pb.RegisterAuditServer(gs, &auditServer{state: opts.State, brokers: opts.Brokers})
	pb.RegisterDeployServer(gs, &deployServer{state: opts.State, raft: opts.Raft})

	return &Server{grpc: gs, listener: lis, opts: opts}, nil
}

// Serve blocks until Stop is called or a fatal error occurs.
func (s *Server) Serve() error { return s.grpc.Serve(s.listener) }

// Stop gracefully shuts down the server.
func (s *Server) Stop() { s.grpc.GracefulStop() }

// Addr returns the actual bound address (useful when BindAddr used port 0).
func (s *Server) Addr() net.Addr { return s.listener.Addr() }
