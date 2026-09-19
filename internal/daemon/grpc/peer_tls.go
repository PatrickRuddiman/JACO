package grpc

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func (s *Server) peerClientCertificate() (tls.Certificate, error) {
	if s.cluster == nil || s.dataDir == "" {
		return tls.Certificate{}, fmt.Errorf("peer TLS: local node credentials unavailable")
	}
	hostname, err := s.cluster.effectiveHostname()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("peer TLS hostname: %w", err)
	}
	return clusterNodeCert(s.dataDir, hostname)
}

// dialPeer reloads credentials on each new connection so credential rotation
// takes effect without caching an expired keypair or an obsolete CA.
func (s *Server) dialPeer(addr string) (*grpc.ClientConn, error) {
	cert, err := s.peerClientCertificate()
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(s.dataDir, "node", "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("peer TLS CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("peer TLS: invalid cluster CA")
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
	})))
}
