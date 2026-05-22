package ca_test

import (
	"crypto/x509"
	"encoding/pem"
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
