package ca_test

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

func TestGenerateAndSignNodeCert_ChainValidates(t *testing.T) {
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}

	keyPEM, csrPEM, err := ca.GenerateNodeKeypair("node-a")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	if len(keyPEM) == 0 || len(csrPEM) == 0 {
		t.Fatalf("empty key or CSR PEM")
	}

	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	// Build trust pool from CA, verify the node cert chains to it.
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	nodeBlock, _ := pem.Decode(nodeCertPEM)
	if nodeBlock == nil || nodeBlock.Type != "CERTIFICATE" {
		t.Fatalf("node cert PEM: bad block")
	}
	nodeCert, err := x509.ParseCertificate(nodeBlock.Bytes)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "node-a",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := nodeCert.Verify(opts); err != nil {
		t.Fatalf("node cert verify failed: %v", err)
	}

	// Subject CN and DNS SAN both carry the hostname.
	if nodeCert.Subject.CommonName != "node-a" {
		t.Errorf("CN = %q, want node-a", nodeCert.Subject.CommonName)
	}
	foundSAN := false
	for _, d := range nodeCert.DNSNames {
		if d == "node-a" {
			foundSAN = true
			break
		}
	}
	if !foundSAN {
		t.Errorf("DNS SAN missing node-a: got %v", nodeCert.DNSNames)
	}
}

func TestSignNodeCSRRejectsTamperedCSR(t *testing.T) {
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	if _, err := ca.SignNodeCSR([]byte("not a csr"), caCertPEM, caKeyPEM); err == nil {
		t.Errorf("expected error on garbage CSR")
	}
}

func TestGenerateNodeKeypair_IPHostname(t *testing.T) {
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	_, csrPEM, err := ca.GenerateNodeKeypair("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	block, _ := pem.Decode(nodeCertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}
	if len(cert.IPAddresses) == 0 {
		t.Errorf("expected IPAddresses populated for IP hostname; got %v", cert.IPAddresses)
	}
}

func TestGenerateNodeKeypair_BothDNSAndIPSANs(t *testing.T) {
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}

	targetIP := net.ParseIP("100.96.111.6")
	_, csrPEM, err := ca.GenerateNodeKeypair("jaco-1", targetIP)
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}

	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	block, _ := pem.Decode(nodeCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("node cert PEM: bad block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}

	// DNS SAN must carry the hostname.
	foundDNS := false
	for _, d := range cert.DNSNames {
		if d == "jaco-1" {
			foundDNS = true
			break
		}
	}
	if !foundDNS {
		t.Errorf("DNS SAN missing jaco-1: got %v", cert.DNSNames)
	}

	// IP SAN must carry the advertise IP.
	foundIP := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(targetIP) {
			foundIP = true
			break
		}
	}
	if !foundIP {
		t.Errorf("IP SAN missing 100.96.111.6: got %v", cert.IPAddresses)
	}

	// Cert must chain to the CA when verified by DNS name.
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "jaco-1",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		t.Errorf("cert verify (DNS) failed: %v", err)
	}
}

func TestGenerateNodeKeypair_VerifyHostnameByIP(t *testing.T) {
	// Asserts the end-to-end TLS-validation property the IP SAN exists for:
	// a client dialing the node by IP literal must pass cert verification.
	// crypto/tls drives this via Certificate.VerifyHostname(host), which
	// matches against IPAddresses when host parses as an IP literal.
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}

	targetIP := net.ParseIP("100.96.111.6")
	_, csrPEM, err := ca.GenerateNodeKeypair("jaco-1", targetIP)
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	block, _ := pem.Decode(nodeCertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse node cert: %v", err)
	}

	// Cert must validate when the dialer's target is the IP literal that
	// went into the SAN.
	if err := cert.VerifyHostname(targetIP.String()); err != nil {
		t.Errorf("VerifyHostname(%q) failed: %v", targetIP.String(), err)
	}

	// And must validate against the DNS SAN.
	if err := cert.VerifyHostname("jaco-1"); err != nil {
		t.Errorf("VerifyHostname(%q) failed: %v", "jaco-1", err)
	}

	// Negative: a different IP not in the SAN must be rejected.
	if err := cert.VerifyHostname("10.0.0.99"); err == nil {
		t.Errorf("VerifyHostname(10.0.0.99) accepted; expected rejection (SAN was %v)", cert.IPAddresses)
	}
}

func TestGenerateNodeKeypair_IPDeduplication(t *testing.T) {
	// Passing the same IP twice must not produce duplicate IP SANs.
	ip := net.ParseIP("10.0.0.1")
	_, csrPEM, err := ca.GenerateNodeKeypair("node-x", ip, ip)
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("expected CSR block, got %v", block)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}

	count := 0
	for _, csrIP := range csr.IPAddresses {
		if csrIP.Equal(ip) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one IP SAN for 10.0.0.1, got %d (IPs: %v)", count, csr.IPAddresses)
	}
}
