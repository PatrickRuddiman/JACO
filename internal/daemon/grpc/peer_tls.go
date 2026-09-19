package grpc

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func (s *Server) dialPeer(addr string) (*grpc.ClientConn, error) {
	caPEM, err := os.ReadFile(filepath.Join(s.dataDir, "node", "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read peer CA: %w", err)
	}
	return dialVerifiedPeer(addr, caPEM)
}

func dialVerifiedPeer(addr string, caPEM []byte) (*grpc.ClientConn, error) {
	roots, _, err := clusterTrust(caPEM)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	})))
}

func clusterTrust(caPEM []byte) (*x509.CertPool, []*x509.Certificate, error) {
	roots := x509.NewCertPool()
	var certs []*x509.Certificate
	for rest := bytes.TrimSpace(caPEM); len(rest) > 0; rest = bytes.TrimSpace(rest) {
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("invalid cluster CA PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("parse cluster CA: %w", err)
		}
		if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, nil, fmt.Errorf("cluster trust must contain CA certificates")
		}
		roots.AddCert(cert)
		certs = append(certs, cert)
		rest = tail
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("independently provisioned cluster CA PEM is required")
	}
	return roots, certs, nil
}
