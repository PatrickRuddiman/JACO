package grpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
)

func writeNodeCredentials(t *testing.T, dataDir, name string, pair tls.Certificate, caPEM []byte) {
	t.Helper()
	dir := filepath.Join(dataDir, "node")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for filename, data := range map[string][]byte{
		name + ".crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}),
		name + ".key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"ca.crt":      caPEM,
	} {
		if err := os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRaftRequiresVerifiedNodeTLS(t *testing.T) {
	trustedCA, trustedKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	wrongCA, wrongKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing-cert", "missing-ca", "malformed-key", "wrong-ca", "wrong-identity", "wrong-san", "valid"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pair := peerCertificate(t, trustedCA, trustedKey, "joining-node", net.ParseIP("127.0.0.1"))
			caPEM := trustedCA
			switch name {
			case "missing-ca":
				caPEM = nil
			case "wrong-ca":
				pair = peerCertificate(t, wrongCA, wrongKey, "joining-node", net.ParseIP("127.0.0.1"))
			case "wrong-identity":
				pair = peerCertificate(t, trustedCA, trustedKey, "another-node", net.ParseIP("127.0.0.1"))
			case "wrong-san":
				pair = peerCertificate(t, trustedCA, trustedKey, "joining-node")
			}
			if name != "missing-cert" {
				writeNodeCredentials(t, dir, "joining-node", pair, caPEM)
			}
			if name == "malformed-key" {
				if err := os.WriteFile(filepath.Join(dir, "node", "joining-node.key"), []byte("not PEM"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, err := New(Options{
				UnixSocketPath: filepath.Join(dir, "rpc.sock"), UnixListener: bufconn.Listen(1024 * 1024),
				DataDir: dir, Hostname: "joining-node", ListenAddr: "127.0.0.1:0",
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				s.Stop(ctx)
				_ = s.listener.Close()
				_ = s.tcpListener.Close()
			})
			err = s.OpenRaft("joining-node", "127.0.0.1:0", "")
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				served, err := s.tlsDyn.GetCertificate(nil)
				if err != nil || !bytes.Equal(served.Certificate[0], pair.Certificate[0]) {
					t.Fatalf("verified initialized certificate was not loaded: %v", err)
				}
				return
			}
			if err == nil {
				t.Error("invalid node credentials silently initialized the peer")
			}
			if s.Raft() != nil {
				t.Error("invalid TLS credentials started Raft")
			}
			if _, err := os.Stat(filepath.Join(dir, "raft")); !os.IsNotExist(err) {
				t.Errorf("invalid TLS credentials wrote Raft state: %v", err)
			}
		})
	}
}
