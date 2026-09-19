package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func writePeerIdentity(t *testing.T, dir, hostname string, cert tls.Certificate, caPEM []byte) {
	t.Helper()
	nodeDir := filepath.Join(dir, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"ca.crt":          caPEM,
		hostname + ".crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		hostname + ".key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(filepath.Join(nodeDir, name), value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeerDialPresentsCertificate(t *testing.T) {
	target, _ := internalRPCLeader(t)
	meta := target.state.Cluster.Get()
	target.tlsDyn.swap(nodeRPCCertificate(t, "leader", meta.GetCaCert(), meta.GetCaKey()))
	source := &Server{dataDir: t.TempDir(), cluster: &clusterServer{hostname: "worker"}}
	writePeerIdentity(t, source.dataDir, "worker",
		nodeRPCCertificate(t, "worker", meta.GetCaCert(), meta.GetCaKey()), meta.GetCaCert())
	conn, err := source.dialPeer(target.TCPAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := submitRPCCommand(t, conn, &pb.Command{Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
		Hostname: "worker", IncludePressure: true, CpuPressure: 0.25,
	}}}); err != nil {
		t.Fatalf("certificate-authenticated forwarding: %v", err)
	}
	node, _ := target.state.Nodes.Get("worker")
	if node.GetCpuPressure() != 0.25 {
		t.Error("forwarded observation was not applied")
	}
	// The same TLS channel still supports a public method without a bearer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pb.NewClusterClient(conn).Status(ctx, &pb.ClusterStatusRequest{}); err != nil {
		t.Fatalf("public status: %v", err)
	}
}

func TestPeerDialFailsClosedOnMissingOrInvalidCredentials(t *testing.T) {
	target, _ := internalRPCLeader(t)
	meta := target.state.Cluster.Get()
	cert := nodeRPCCertificate(t, "worker", meta.GetCaCert(), meta.GetCaKey())
	for _, file := range []string{"worker.crt", "worker.key", "ca.crt"} {
		for _, missing := range []bool{true, false} {
			name := file + "/malformed"
			if missing {
				name = file + "/missing"
			}
			t.Run(name, func(t *testing.T) {
				source := &Server{dataDir: t.TempDir(), cluster: &clusterServer{hostname: "worker"}}
				writePeerIdentity(t, source.dataDir, "worker", cert, meta.GetCaCert())
				path := filepath.Join(source.dataDir, "node", file)
				var err error
				if missing {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte("not a credential"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				conn, err := source.dialPeer(target.TCPAddr())
				if conn != nil {
					_ = conn.Close()
				}
				if err == nil {
					t.Fatal("peer dial fell back without valid node credentials")
				}
			})
		}
	}
}

func TestPeerDialVerifiesServerAndReloadsTrust(t *testing.T) {
	target, _ := internalRPCLeader(t)
	meta := target.state.Cluster.Get()
	serverCert := nodeRPCCertificate(t, "leader", meta.GetCaCert(), meta.GetCaKey())
	source := &Server{dataDir: t.TempDir(), cluster: &clusterServer{hostname: "worker"}}
	writePeerIdentity(t, source.dataDir, "worker",
		nodeRPCCertificate(t, "worker", meta.GetCaCert(), meta.GetCaKey()), meta.GetCaCert())
	callStatus := func() error {
		t.Helper()
		conn, err := source.dialPeer(target.TCPAddr())
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = pb.NewClusterClient(conn).Status(ctx, &pb.ClusterStatusRequest{})
		return err
	}
	// The daemon is still presenting its untrusted bootstrap certificate.
	if err := callStatus(); status.Code(err) != codes.Unavailable {
		t.Fatalf("untrusted server accepted: %v", err)
	}
	wrongSAN := reissueRPCCertificate(t, serverCert, meta.GetCaCert(), meta.GetCaKey(), func(c *x509.Certificate) {
		c.IPAddresses = []net.IP{net.ParseIP("127.0.0.2")}
	})
	target.tlsDyn.swap(wrongSAN)
	if err := callStatus(); status.Code(err) != codes.Unavailable {
		t.Fatalf("wrong destination SAN accepted: %v", err)
	}
	target.tlsDyn.swap(serverCert)
	if err := callStatus(); err != nil {
		t.Fatalf("valid peer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source.dataDir, "node", "ca.crt"), []byte("invalid replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := callStatus(); err == nil {
		t.Fatal("peer dial reused stale trust after CA-file replacement")
	}
}
