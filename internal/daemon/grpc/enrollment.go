package grpc

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func validateJoinResponse(resp *pb.NodeJoinResponse, keyPEM, trustedCA []byte, hostname string, addresses ...string) error {
	roots, trusted, err := clusterTrust(trustedCA)
	if err != nil {
		return err
	}
	_, returned, err := clusterTrust(resp.GetCaCert())
	if err != nil {
		return fmt.Errorf("returned CA: %w", err)
	}
	for _, cert := range returned {
		matches := false
		for _, root := range trusted {
			if cert.Equal(root) {
				matches = true
				break
			}
		}
		if !matches {
			return fmt.Errorf("returned CA does not match independently provisioned trust")
		}
	}
	pair, err := tls.X509KeyPair(resp.GetSignedCert(), keyPEM)
	if err != nil {
		return fmt.Errorf("returned node keypair: %w", err)
	}
	return verifyNodeCertificate(pair, roots, hostname, addresses...)
}

func verifyNodeCertificate(pair tls.Certificate, roots *x509.CertPool, hostname string, addresses ...string) error {
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("node certificate: %w", err)
	}
	if leaf.IsCA || leaf.Subject.CommonName != hostname {
		return fmt.Errorf("certificate does not identify node %q", hostname)
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("intermediate certificate: %w", err)
		}
		intermediates.AddCert(cert)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, DNSName: hostname,
			KeyUsages: []x509.ExtKeyUsage{usage},
		}); err != nil {
			return fmt.Errorf("verify node certificate: %w", err)
		}
	}
	for _, addr := range addresses {
		if addr == "" {
			continue
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid advertised address %q: %w", addr, err)
		}
		if err := leaf.VerifyHostname(host); err != nil {
			return fmt.Errorf("certificate does not cover advertised address %q: %w", addr, err)
		}
	}
	return nil
}
