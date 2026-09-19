package grpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func joiningServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{
		UnixSocketPath:      filepath.Join(t.TempDir(), "rpc.sock"),
		UnixListener:        bufconn.Listen(1024 * 1024),
		DataDir:             t.TempDir(),
		Hostname:            "joining-node",
		ClusterAddr:         "127.0.0.1:0",
		ListenAdvertiseAddr: "127.0.0.1:7000",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.Stop(ctx)
		_ = s.listener.Close()
	})
	return s
}

func TestJoinRejectsInvalidCredentialsBeforePersistence(t *testing.T) {
	trustedCA, trustedKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	otherCA, otherKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"replaced-ca", "appended-ca", "private-block", "opaque-prefix", "opaque-gap",
		"opaque-tail", "malformed-prefix", "pem-headers", "wrong-chain", "wrong-key",
		"wrong-identity", "missing-advertised-san", "expired", "missing-client-eku",
		"missing-server-eku", "ca-leaf",
	} {
		t.Run(name, func(t *testing.T) {
			serveCert := peerCertificate(t, trustedCA, trustedKey, "peer", net.ParseIP("127.0.0.1"))
			addr, _ := startRecordingPeer(t, serveCert, func(req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
				signCA, signKey, csr := trustedCA, trustedKey, req.GetCsrPem()
				if name == "wrong-chain" {
					signCA, signKey = otherCA, otherKey
				}
				if name == "wrong-key" {
					_, replacementCSR, keyErr := ca.GenerateNodeKeypair(req.GetName(), net.ParseIP("127.0.0.1"))
					if keyErr != nil {
						return nil, keyErr
					}
					csr = replacementCSR
				}
				certPEM, err := ca.SignNodeCSR(csr, signCA, signKey)
				if err != nil {
					return nil, err
				}
				block, _ := pem.Decode(certPEM)
				leaf, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, err
				}
				switch name {
				case "wrong-identity":
					leaf.Subject.CommonName = "another-node"
					leaf.RawSubject = nil
				case "missing-advertised-san":
					leaf.IPAddresses = nil
				case "expired":
					leaf.NotBefore = time.Now().Add(-2 * time.Hour)
					leaf.NotAfter = time.Now().Add(-time.Hour)
				case "missing-client-eku":
					leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
				case "missing-server-eku":
					leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				case "ca-leaf":
					leaf.IsCA, leaf.BasicConstraintsValid = true, true
					leaf.KeyUsage |= x509.KeyUsageCertSign
				}
				issuer, key, err := ca.ParseCA(signCA, signKey)
				if err != nil {
					return nil, err
				}
				der, err := x509.CreateCertificate(rand.Reader, leaf, issuer, leaf.PublicKey, key)
				if err != nil {
					return nil, err
				}
				returnedCA := trustedCA
				switch name {
				case "replaced-ca":
					returnedCA = otherCA
				case "appended-ca":
					returnedCA = bytes.Join([][]byte{trustedCA, otherCA}, []byte("\n"))
				case "private-block":
					returnedCA = bytes.Join([][]byte{trustedCA, trustedKey}, []byte("\n"))
				case "opaque-prefix":
					returnedCA = bytes.Join([][]byte{[]byte("opaque data"), trustedCA}, []byte("\n"))
				case "opaque-gap":
					returnedCA = bytes.Join([][]byte{trustedCA, []byte("opaque data"), trustedCA}, []byte("\n"))
				case "opaque-tail":
					returnedCA = bytes.Join([][]byte{trustedCA, []byte("opaque data")}, []byte("\n"))
				case "malformed-prefix":
					returnedCA = append([]byte("-----BEGIN CERTIFICATE-----\ninvalid certificate\n"), trustedCA...)
				case "pem-headers":
					block, _ := pem.Decode(trustedCA)
					block.Headers = map[string]string{"Comment": "not certificate data"}
					returnedCA = pem.EncodeToMemory(block)
				}
				return &pb.NodeJoinResponse{
					ClusterId: "test-cluster", CaCert: returnedCA,
					SignedCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
					PeerAddrs:  []string{"127.0.0.1:7001"},
				}, nil
			})
			s := joiningServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := s.cluster.Join(ctx, &pb.ClusterJoinRequest{
				PeerAddr: addr, JoinToken: "synthetic-join-token", CaCert: trustedCA,
			})
			if err == nil {
				t.Error("invalid enrollment credentials accepted")
			}
			if _, err := os.Stat(filepath.Join(s.dataDir, "node")); !os.IsNotExist(err) {
				t.Errorf("invalid enrollment credentials were persisted: %v", err)
			}
			if s.gate.IsInitialized() || s.Raft() != nil {
				t.Error("invalid enrollment started cluster membership")
			}
		})
	}
}
