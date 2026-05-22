package ca_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

func TestGenerateClusterCA_ValidSelfSignedTenYears(t *testing.T) {
	certPEM, keyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("cert PEM: bad block %+v", block)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	if !cert.IsCA {
		t.Errorf("IsCA = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("KeyUsage missing CertSign")
	}

	want := 10 * 365 * 24 * time.Hour
	got := cert.NotAfter.Sub(cert.NotBefore)
	if got < want-2*time.Hour || got > want+2*time.Hour {
		t.Errorf("validity duration = %v, want ~%v", got, want)
	}

	// Verify self-signed by checking the signature against itself.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("self-signed signature: %v", err)
	}

	// Verify key parses as Ed25519 and matches the cert's public key.
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("key PEM: bad block %+v", keyBlock)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	priv, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("key type = %T, want ed25519.PrivateKey", keyAny)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert pubkey type = %T, want ed25519.PublicKey", cert.PublicKey)
	}
	if !pub.Equal(priv.Public()) {
		t.Errorf("private key does not match cert public key")
	}
}

func TestParseCARoundTrip(t *testing.T) {
	certPEM, keyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	cert, key, err := ca.ParseCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ParseCA: %v", err)
	}
	if cert.Subject.CommonName != "JACO Cluster CA" {
		t.Errorf("CN = %q, want JACO Cluster CA", cert.Subject.CommonName)
	}
	if len(key) == 0 {
		t.Errorf("ParseCA returned empty key")
	}
}

func TestParseCARejectsBadPEM(t *testing.T) {
	if _, _, err := ca.ParseCA([]byte("not pem"), []byte("not pem")); err == nil {
		t.Errorf("expected error on bad PEM")
	}
}
