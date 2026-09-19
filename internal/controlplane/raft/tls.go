package raftnode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hraft "github.com/hashicorp/raft"
)

const raftALPN = "jaco-raft-v1"
const transportTimeout = 10 * time.Second

type tlsTransport struct {
	*hraft.NetworkTransport
	heartbeatMu sync.Mutex
}

func (t *tlsTransport) SetHeartbeatHandler(handle func(hraft.RPC)) {
	if handle == nil {
		t.NetworkTransport.SetHeartbeatHandler(nil)
		return
	}
	t.NetworkTransport.SetHeartbeatHandler(func(rpc hraft.RPC) {
		t.heartbeatMu.Lock()
		defer t.heartbeatMu.Unlock()
		if t.IsShutdown() {
			rpc.Respond(nil, hraft.ErrTransportShutdown)
			return
		}
		handle(rpc)
	})
}

func (t *tlsTransport) Close() error {
	// Fast-path heartbeats run outside Raft's shutdown wait group.
	t.heartbeatMu.Lock()
	defer t.heartbeatMu.Unlock()
	return t.NetworkTransport.Close()
}

func loadNodeTLS(dataDir, localID string) (*tls.Config, error) {
	if localID == "." || localID == ".." || strings.ContainsAny(localID, `/\`) {
		return nil, fmt.Errorf("invalid node identity %q", localID)
	}
	dir := filepath.Join(dataDir, "node")
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, localID+".crt"), filepath.Join(dir, localID+".key"))
	if err != nil {
		return nil, fmt.Errorf("load node certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("load cluster CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid cluster CA")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse node certificate: %w", err)
	}
	if leaf.IsCA || leaf.Subject.CommonName != localID {
		return nil, fmt.Errorf("certificate is not a node identity for %q", localID)
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("parse intermediate: %w", err)
		}
		intermediates.AddCert(c)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, DNSName: localID,
			KeyUsages: []x509.ExtKeyUsage{usage},
		}); err != nil {
			return nil, fmt.Errorf("verify node certificate: %w", err)
		}
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots, ClientCAs: roots,
		ClientAuth: tls.RequireAndVerifyClientCert,
		NextProtos: []string{raftALPN},
	}, nil
}

type tlsStream struct {
	net.Listener
	advertise net.Addr
	dataDir   string
	localID   string
	raft      atomic.Pointer[hraft.Raft]
	mu        sync.Mutex
	closed    bool
	conns     map[*tlsRaftConn]struct{}
}

func newTLSStream(cfg Config, advertise net.Addr) (*tlsStream, error) {
	lis, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		return nil, err
	}
	if advertise == nil {
		advertise = lis.Addr()
	}
	addr, ok := advertise.(*net.TCPAddr)
	if !ok || addr.IP == nil || addr.IP.IsUnspecified() {
		_ = lis.Close()
		return nil, fmt.Errorf("local bind address is not advertisable")
	}
	return &tlsStream{
		Listener: lis, advertise: advertise, dataDir: cfg.DataDir, localID: cfg.LocalID,
		conns: make(map[*tlsRaftConn]struct{}),
	}, nil
}

func (s *tlsStream) Addr() net.Addr { return s.advertise }

func (s *tlsStream) peerID(address hraft.ServerAddress) (string, error) {
	r := s.raft.Load()
	if r == nil {
		return "", fmt.Errorf("raft TLS: configuration unavailable")
	}
	var id string
	for _, peer := range r.GetConfiguration().Configuration().Servers {
		if peer.Address == address {
			if id != "" {
				return "", fmt.Errorf("raft TLS: ambiguous identity for %q", address)
			}
			id = string(peer.ID)
		}
	}
	if id == "" {
		return "", fmt.Errorf("raft TLS: unknown peer address %q", address)
	}
	return id, nil
}

// The node CA is the enrollment authority. An offline follower's Raft
// configuration may not yet contain a legitimately elected new leader.
func (s *tlsStream) authorize(cs tls.ConnectionState, expectedID string) error {
	if cs.NegotiatedProtocol != raftALPN || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("raft TLS: authenticated Raft protocol required")
	}
	cert := cs.PeerCertificates[0]
	id := cert.Subject.CommonName
	if cert.IsCA || id == "" || (expectedID != "" && id != expectedID) {
		return fmt.Errorf("raft TLS: unexpected peer identity %q", id)
	}
	if err := cert.VerifyHostname(id); err != nil {
		return fmt.Errorf("raft TLS: peer identity SAN: %w", err)
	}
	now := time.Now()
	for _, chain := range cs.VerifiedChains {
		valid := len(chain) > 0
		for _, cert := range chain {
			if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				valid = false
				break
			}
		}
		if valid {
			return nil
		}
	}
	return fmt.Errorf("raft TLS: verified certificate chain expired or not yet valid")
}

func (s *tlsStream) config(expectedID string) (*tls.Config, error) {
	cfg, err := loadNodeTLS(s.dataDir, s.localID)
	if err != nil {
		return nil, fmt.Errorf("raft TLS: %w", err)
	}
	cfg.ServerName = expectedID
	cfg.VerifyConnection = func(cs tls.ConnectionState) error { return s.authorize(cs, expectedID) }
	return cfg, nil
}

func (s *tlsStream) Accept() (net.Conn, error) {
	conn, err := s.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// Handshake in the per-connection Raft reader, not the accept loop:
	// a silent unauthenticated socket must not block healthy members.
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return s.config("")
		},
	}
	return s.track(tls.Server(conn, cfg), "", "")
}

func (s *tlsStream) Dial(address hraft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	id, err := s.peerID(address)
	if err != nil {
		return nil, err
	}
	cfg, err := s.config(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", string(address))
	if err != nil {
		return nil, err
	}
	conn, err := s.track(tls.Client(raw, cfg), id, address)
	if err != nil {
		return nil, err
	}
	if err := conn.handshake(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (s *tlsStream) track(conn *tls.Conn, id string, target hraft.ServerAddress) (*tlsRaftConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	c := &tlsRaftConn{Conn: conn, stream: s, expectedID: id, target: target}
	s.conns[c] = struct{}{}
	return c, nil
}

func (s *tlsStream) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = make(map[*tlsRaftConn]struct{})
	s.mu.Unlock()
	err := s.Listener.Close()
	for conn := range conns {
		_ = conn.Close()
	}
	return err
}

type tlsRaftConn struct {
	*tls.Conn
	stream     *tlsStream
	expectedID string
	target     hraft.ServerAddress
	once       sync.Once
	err        error
}

func (c *tlsRaftConn) handshake(ctx context.Context) error {
	c.once.Do(func() { c.err = c.Conn.HandshakeContext(ctx) })
	return c.err
}

func (c *tlsRaftConn) check() error {
	ctx, cancel := context.WithTimeout(context.Background(), transportTimeout)
	defer cancel()
	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return err
	}
	if err := c.authorize(); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

func (c *tlsRaftConn) authorize() error {
	// Hashicorp pools by address; a reassigned address must not inherit a
	// connection authenticated as the previous server ID.
	if c.target != "" {
		id, err := c.stream.peerID(c.target)
		if err != nil {
			return err
		}
		if id != c.expectedID {
			return fmt.Errorf("raft TLS: peer identity changed for %q", c.target)
		}
	}
	return c.stream.authorize(c.ConnectionState(), c.expectedID)
}

func (c *tlsRaftConn) Read(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		if authErr := c.authorize(); authErr != nil {
			_ = c.Close()
			return 0, authErr
		}
	}
	return n, err
}

func (c *tlsRaftConn) Write(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *tlsRaftConn) Close() error {
	c.stream.mu.Lock()
	delete(c.stream.conns, c)
	c.stream.mu.Unlock()
	return c.Conn.Close()
}
