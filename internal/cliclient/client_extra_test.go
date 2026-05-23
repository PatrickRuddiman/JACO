package cliclient_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
)

// TestNewClient_RejectsEmptyAddrs — defensive guard.
func TestNewClient_RejectsEmptyAddrs(t *testing.T) {
	if _, err := cliclient.NewClient(&cliclient.Context{}); err == nil {
		t.Errorf("NewClient with no ServerAddrs returned nil err")
	}
}

// TestNewClient_RejectsMissingCACert — when CACertPath points
// nowhere, NewClient surfaces an open error.
func TestNewClient_RejectsMissingCACert(t *testing.T) {
	ctx := &cliclient.Context{
		ServerAddrs: []string{"127.0.0.1:1234"},
		CACertPath:  filepath.Join(t.TempDir(), "no-such-file.crt"),
		Token:       "x",
	}
	if _, err := cliclient.NewClient(ctx); err == nil {
		t.Errorf("NewClient with missing CA file returned nil err")
	}
}

// TestNewClient_RejectsCorruptCAPEM — file exists but isn't valid PEM.
func TestNewClient_RejectsCorruptCAPEM(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, []byte("not-pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &cliclient.Context{
		ServerAddrs: []string{"127.0.0.1:1234"},
		CACertPath:  caPath,
		Token:       "x",
	}
	_, err := cliclient.NewClient(ctx)
	if err == nil {
		t.Errorf("NewClient with corrupt CA returned nil err")
	}
	if !strings.Contains(err.Error(), "did not parse") {
		t.Errorf("err = %v, want \"did not parse\" substring", err)
	}
}

// TestNewClient_AcceptsValidCA — synthetic self-signed CA passes
// AppendCertsFromPEM.
func TestNewClient_AcceptsValidCA(t *testing.T) {
	caPEM := mintTestCA(t)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &cliclient.Context{
		ServerAddrs: []string{"127.0.0.1:1234"},
		CACertPath:  caPath,
		Token:       "x",
	}
	cli, err := cliclient.NewClient(ctx)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
}

// TestConn_LazilyDialsFirstAddr — Conn() should produce a *grpc.ClientConn
// (lazy; doesn't actually connect because grpc.NewClient is lazy).
func TestConn_LazilyDialsFirstAddr(t *testing.T) {
	cli := cliclient.NewInsecure(cliclient.InsecureOptions{
		Addrs: []string{"127.0.0.1:0"},
		Token: "tok",
	})
	conn, err := cli.Conn()
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	if conn == nil {
		t.Errorf("Conn returned nil *grpc.ClientConn")
	}
	defer cli.Close()
	// Second Conn should reuse the same connection.
	conn2, _ := cli.Conn()
	if conn2 != conn {
		t.Errorf("Conn did not reuse the cached connection")
	}
}

// TestDefaultClustersPath_HonorsXDG — XDG_CONFIG_HOME wins when set.
func TestDefaultClustersPath_HonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	if got := cliclient.DefaultClustersPath(); got != "/tmp/xdg-test/jaco/clusters.yaml" {
		t.Errorf("XDG path = %q", got)
	}
}

// TestDefaultClustersPath_FallsBackToHomeWhenXDGUnset — falls back to
// $HOME/.config/jaco/clusters.yaml.
func TestDefaultClustersPath_FallsBackToHomeWhenXDGUnset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/home-test")
	if got := cliclient.DefaultClustersPath(); got != "/tmp/home-test/.config/jaco/clusters.yaml" {
		t.Errorf("HOME-fallback path = %q", got)
	}
}

// --- helpers ----------------------------------------------------------------

func mintTestCA(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		BasicConstraintsValid: true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
