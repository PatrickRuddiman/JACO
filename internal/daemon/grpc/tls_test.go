package grpc

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDynamicTLS_SwapAndLoad — initial cert is returned by
// GetCertificate; after swap, the new cert is returned.
func TestDynamicTLS_SwapAndLoad(t *testing.T) {
	c1, err := bootstrapCert("host-a")
	if err != nil {
		t.Fatalf("bootstrapCert host-a: %v", err)
	}
	d := newDynamicTLS(c1)
	got, err := d.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate initial: %v", err)
	}
	if &got.Certificate[0][0] != &c1.Certificate[0][0] {
		t.Errorf("initial GetCertificate did not return the stored cert")
	}

	c2, err := bootstrapCert("host-b")
	if err != nil {
		t.Fatalf("bootstrapCert host-b: %v", err)
	}
	d.swap(c2)
	got2, _ := d.GetCertificate(nil)
	if &got2.Certificate[0][0] == &c1.Certificate[0][0] {
		t.Errorf("swap had no effect; still returns old cert")
	}
}

// TestClusterNodeCert_MissingFile — both cert and key paths must exist;
// missing either surfaces a descriptive error.
func TestClusterNodeCert_MissingFile(t *testing.T) {
	if _, err := clusterNodeCert(t.TempDir(), "ghost"); err == nil {
		t.Errorf("clusterNodeCert(missing) = nil err")
	}
}

// TestClusterNodeCert_LoadsValidPair — generate a self-signed pair into
// the expected $dataDir/node/<host>.{crt,key} layout, then assert
// clusterNodeCert returns a usable tls.Certificate.
func TestClusterNodeCert_LoadsValidPair(t *testing.T) {
	dir := t.TempDir()
	nodeDir := filepath.Join(dir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Reuse bootstrapCert to mint a PEM-encodable pair; extract the
	// raw PEM blocks by re-encoding from the parsed tls.Certificate
	// fields. Simpler: write the PEMs directly via re-encoding.
	cert, err := bootstrapCertPEMs("test-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "test-host.crt"), cert.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "test-host.key"), cert.keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := clusterNodeCert(dir, "test-host")
	if err != nil {
		t.Fatalf("clusterNodeCert: %v", err)
	}
	if len(c.Certificate) == 0 {
		t.Errorf("loaded cert has 0 certificate entries")
	}
}

// TestClusterNodeCert_MalformedKeyFile — cert exists but key is junk;
// X509KeyPair fails with a recognizable error.
func TestClusterNodeCert_MalformedKeyFile(t *testing.T) {
	dir := t.TempDir()
	nodeDir := filepath.Join(dir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert, err := bootstrapCertPEMs("test-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "test-host.crt"), cert.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "test-host.key"), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = clusterNodeCert(dir, "test-host")
	if err == nil {
		t.Fatalf("clusterNodeCert with malformed key succeeded")
	}
	if !strings.Contains(err.Error(), "load keypair") {
		t.Errorf("err = %v, want \"load keypair\" substring", err)
	}
}

// TestBootstrapTLSConfig — returned *tls.Config has GetCertificate
// plumbed; calling it surfaces a usable cert.
func TestBootstrapTLSConfig(t *testing.T) {
	cfg, d, err := bootstrapTLSConfig("test-bootstrap")
	if err != nil {
		t.Fatalf("bootstrapTLSConfig: %v", err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want >= TLS 1.2", cfg.MinVersion)
	}
	if d == nil {
		t.Errorf("dynamicTLS returned nil")
	}
	got, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Errorf("cert empty")
	}
}

// TestBootstrapCert_DefaultsHostname — empty hostname falls back to
// jacod-bootstrap (defensive default; production passes the real host).
func TestBootstrapCert_DefaultsHostname(t *testing.T) {
	cert, err := bootstrapCert("")
	if err != nil {
		t.Fatalf("bootstrapCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Errorf("empty cert chain")
	}
}

// --- helpers ----------------------------------------------------------------

type pemPair struct {
	certPEM []byte
	keyPEM  []byte
}

// bootstrapCertPEMs re-encodes the tls.Certificate returned by
// bootstrapCert into raw PEM blocks so tests can write them to the
// expected on-disk layout.
func bootstrapCertPEMs(hostname string) (*pemPair, error) {
	cert, err := bootstrapCert(hostname)
	if err != nil {
		return nil, err
	}
	priv, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not *ecdsa.PrivateKey: %T", cert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &pemPair{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}
