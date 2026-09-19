package grpc_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// --- fake peer Cluster.NodeJoin server ---------------------------------

type fakePeer struct {
	pb.UnimplementedClusterServer
	clusterID     string
	signedCertPEM []byte
	caCertPEM     []byte
	caKeyPEM      []byte
	peerAddrs     []string

	mu      sync.Mutex // protects lastReq
	lastReq *pb.NodeJoinRequest

	rejectWith error // when non-nil, NodeJoin returns this error
}

func (f *fakePeer) NodeJoin(_ context.Context, req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
	if f.rejectWith != nil {
		return nil, f.rejectWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	if f.signedCertPEM == nil {
		var err error
		f.signedCertPEM, err = ca.SignNodeCSR(req.GetCsrPem(), f.caCertPEM, f.caKeyPEM)
		if err != nil {
			return nil, err
		}
	}
	return &pb.NodeJoinResponse{
		ClusterId:  f.clusterID,
		SignedCert: f.signedCertPEM,
		CaCert:     f.caCertPEM,
		PeerAddrs:  f.peerAddrs,
	}, nil
}

func startFakePeer(t *testing.T, peer *fakePeer) string {
	t.Helper()
	caCert, caKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	peer.caCertPEM, peer.caKeyPEM = caCert, caKey
	keyPEM, csrPEM, err := ca.GenerateNodeKeypair("localhost", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignNodeCSR(csrPEM, caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{tlsCert}})))
	pb.RegisterClusterServer(gs, peer)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// --- helpers --------------------------------------------------------

func newDaemon(t *testing.T) (*dgrpc.Server, pb.ClusterClient, string) {
	t.Helper()
	dataDir := t.TempDir()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: sock,
		DataDir:        dataDir,
		Hostname:       "node-b",
		ClusterAddr:    freePort(t),
	})
	if err != nil {
		t.Fatalf("dgrpc.New: %v", err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Stop(ctx)
	})

	conn, err := grpc.NewClient(
		"unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return s, pb.NewClusterClient(conn), dataDir
}

// --- tests -----------------------------------------------------------

func TestJoin_DialsPeerAndPersistsCerts(t *testing.T) {
	peer := &fakePeer{
		clusterID: "cluster-xyz",
		peerAddrs: []string{"127.0.0.1:7001"},
	}
	peerAddr := startFakePeer(t, peer)

	server, c, dataDir := newDaemon(t)
	_, err := c.Join(context.Background(), &pb.ClusterJoinRequest{
		PeerAddr:  peerAddr,
		JoinToken: "fake-token-xyz",
		CaCert:    peer.caCertPEM,
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	if !server.Gate().IsInitialized() {
		t.Errorf("gate not flipped")
	}

	// Files persisted.
	for _, name := range []string{"node-b.key", "node-b.crt", "ca.crt", "join.json"} {
		if _, err := os.Stat(filepath.Join(dataDir, "node", name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	// Cert content matches what the peer returned.
	gotCert, _ := os.ReadFile(filepath.Join(dataDir, "node", "node-b.crt"))
	peer.mu.Lock()
	if string(gotCert) != string(peer.signedCertPEM) {
		t.Errorf("cert content mismatch")
	}
	peer.mu.Unlock()
	gotCA, _ := os.ReadFile(filepath.Join(dataDir, "node", "ca.crt"))
	if string(gotCA) != string(peer.caCertPEM) {
		t.Errorf("ca content mismatch")
	}
	// join.json carries cluster_id + peers.
	metaBytes, _ := os.ReadFile(filepath.Join(dataDir, "node", "join.json"))
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("join.json: %v", err)
	}
	if meta["cluster_id"] != "cluster-xyz" {
		t.Errorf("meta.cluster_id = %v", meta["cluster_id"])
	}

	// The peer received our CSR + name + join_token.
	peer.mu.Lock()
	got := peer.lastReq
	peer.mu.Unlock()
	if got == nil {
		t.Fatalf("peer never saw NodeJoin")
	}
	if got.GetName() != "node-b" {
		t.Errorf("name = %q, want node-b", got.GetName())
	}
	if got.GetJoinToken() != "fake-token-xyz" {
		t.Errorf("token = %q", got.GetJoinToken())
	}
	if len(got.GetCsrPem()) == 0 {
		t.Errorf("csr_pem empty")
	}
}

func TestJoin_RefusesWhenAlreadyInitialized(t *testing.T) {
	// Post-init, Join goes through the admission interceptor (iter 14).
	// With no real Init having run, state.Tokens is empty and the lazy
	// admission returns Unavailable (state_unavailable). The daemon's
	// own "cluster_already_initialized" refusal lives behind the admin
	// layer; that path is exercised by TestInit_RefusesWhenAlreadyInitialized
	// which uses a real operator token.
	server, c, _ := newDaemon(t)
	server.Gate().MarkInitialized()
	_, err := c.Join(context.Background(), &pb.ClusterJoinRequest{
		PeerAddr:  "127.0.0.1:9999",
		JoinToken: "x",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

func TestJoin_RejectsEmptyArgs(t *testing.T) {
	_, c, _ := newDaemon(t)
	for _, req := range []*pb.ClusterJoinRequest{
		{PeerAddr: "", JoinToken: "x"},
		{PeerAddr: "127.0.0.1:7000", JoinToken: ""},
	} {
		_, err := c.Join(context.Background(), req)
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("req=%+v code = %v, want InvalidArgument", req, st.Code())
		}
	}
}

func TestJoin_SurfacesPeerError(t *testing.T) {
	peer := &fakePeer{rejectWith: status.Error(codes.PermissionDenied, "join_token_invalid")}
	peerAddr := startFakePeer(t, peer)

	server, c, _ := newDaemon(t)
	_, err := c.Join(context.Background(), &pb.ClusterJoinRequest{
		PeerAddr:  peerAddr,
		JoinToken: "bad-token",
		CaCert:    peer.caCertPEM,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "join_token_invalid") {
		t.Errorf("err should mention join_token_invalid; got %v", err)
	}
	if server.Gate().IsInitialized() {
		t.Errorf("gate flipped despite peer error")
	}
}
