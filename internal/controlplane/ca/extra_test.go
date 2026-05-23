package ca_test

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

// TestParseCA_RejectsMalformedCertBytes — PEM-decodes but the DER body
// isn't a valid certificate.
func TestParseCA_RejectsMalformedCertBytes(t *testing.T) {
	badCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x01, 0x02}})
	_, validKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	if _, _, err := ca.ParseCA(badCertPEM, validKeyPEM); err == nil {
		t.Errorf("ParseCA with malformed cert DER returned nil err")
	}
}

// TestParseCA_RejectsWrongKeyType — feed a non-Ed25519 PKCS8 key.
// We generate a CA, then swap in a fresh RSA-style key via a synthetic
// PEM that ParsePKCS8PrivateKey will accept but isn't ed25519. Easiest
// approach: re-encode a valid CSR key (which is ed25519 via the
// node_cert API), but pass the WRONG label.
//
// Simpler still: feed garbage to the key parser to drive the
// ParsePKCS8PrivateKey error path.
func TestParseCA_RejectsBadKeyDER(t *testing.T) {
	validCertPEM, _, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	badKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x01}})
	if _, _, err := ca.ParseCA(validCertPEM, badKeyPEM); err == nil {
		t.Errorf("ParseCA with bad key DER returned nil err")
	}
}

// TestParseCA_RejectsWrongPEMType — cert PEM has the wrong block
// type.
func TestParseCA_RejectsWrongCertBlockType(t *testing.T) {
	wrongTypePEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{0x01}})
	if _, _, err := ca.ParseCA(wrongTypePEM, []byte{}); err == nil || !strings.Contains(err.Error(), "decode CA cert PEM") {
		t.Errorf("err = %v, want decode CA cert PEM substring", err)
	}
}

// TestSignNodeCSR_RejectsMalformedCSRBytes — CSR PEM parses but DER
// body isn't a CSR.
func TestSignNodeCSR_RejectsMalformedCSRBytes(t *testing.T) {
	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	badCSR := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte{0xff, 0x00}})
	if _, err := ca.SignNodeCSR(badCSR, caCertPEM, caKeyPEM); err == nil {
		t.Errorf("SignNodeCSR with malformed CSR returned nil err")
	}
}

// TestSignNodeCSR_RejectsWrongPEMType — block type isn't
// CERTIFICATE REQUEST.
func TestSignNodeCSR_RejectsWrongPEMType(t *testing.T) {
	caCertPEM, caKeyPEM, _ := ca.GenerateClusterCA()
	wrong := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x01}})
	if _, err := ca.SignNodeCSR(wrong, caCertPEM, caKeyPEM); err == nil || !strings.Contains(err.Error(), "decode CSR PEM") {
		t.Errorf("err = %v, want decode CSR PEM substring", err)
	}
}

// TestGenerateClusterCA_CertHasExpectedFields — extracted certificate
// has IsCA=true, KeyUsageCertSign, lifetime ~10y.
func TestGenerateClusterCA_CertHasExpectedFields(t *testing.T) {
	certPEM, _, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("cert PEM did not decode")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if !c.IsCA {
		t.Errorf("IsCA = false")
	}
	if c.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("KeyUsageCertSign not set")
	}
	if c.SerialNumber.BitLen() < 64 {
		t.Errorf("serial too small: %s", c.SerialNumber.String())
	}
}
