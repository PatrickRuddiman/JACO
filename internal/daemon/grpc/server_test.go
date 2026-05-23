package grpc_test

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// startServer spins up a jacod gRPC server bound to a temp unix socket and
// returns a grpc.ClientConn dialing it + a teardown.
func startServer(t *testing.T) (*grpc.ClientConn, *dgrpc.Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{UnixSocketPath: sock})
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
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, s
}

func TestServer_StatusReturnsUninitializedBeforeMarkInitialized(t *testing.T) {
	conn, _ := startServer(t)
	c := pb.NewClusterClient(conn)
	resp, err := c.Status(context.Background(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp.GetInitialized() {
		t.Errorf("Initialized = true on fresh server")
	}
}

func TestServer_GatedMethodsReturnClusterUninitialized(t *testing.T) {
	conn, _ := startServer(t)
	c := pb.NewClusterClient(conn)

	// Bootstrap is not in the AllowedPreInit set — should be gated.
	_, err := c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if err == nil {
		t.Fatalf("Bootstrap should be gated pre-init")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
	if !strings.Contains(st.Message(), "cluster_uninitialized") {
		t.Errorf("message = %q, want cluster_uninitialized", st.Message())
	}
}

func TestServer_StatusReflectsMarkInitialized(t *testing.T) {
	conn, s := startServer(t)
	c := pb.NewClusterClient(conn)
	s.Gate().MarkInitialized()
	resp, err := c.Status(context.Background(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetInitialized() {
		t.Errorf("Initialized = false after MarkInitialized")
	}
}

func TestServer_AfterInitializedGatedMethodsHitAdmission(t *testing.T) {
	// Once the gate is open, non-allow-listed RPCs go through the
	// admission interceptor wired in iter 14. Without a Bearer token
	// they're rejected as Unauthenticated rather than reaching the
	// handler. Tests that need to reach the handler must capture the
	// operator token from Init and attach it via metadata (see e.g.
	// TestInit_GatedMethodsUnblockedAfterInit).
	conn, s := startServer(t)
	s.Gate().MarkInitialized()
	c := pb.NewClusterClient(conn)

	// Bootstrap is not in UnauthMethods, so post-init it requires a
	// Bearer token. With state nil (manual gate flip in this test), the
	// lazy admission returns state_unavailable.
	_, err := c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if err == nil {
		t.Fatalf("post-Init Bootstrap should be rejected (no auth)")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable (state_unavailable)", st.Code())
	}
}

func TestServer_NewRejectsMissingSocketPath(t *testing.T) {
	if _, err := dgrpc.New(dgrpc.Options{}); err == nil {
		t.Errorf("expected error on empty UnixSocketPath")
	}
}

func TestServer_TCPListenerServesClusterStatus(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	listenAddr := freePort(t)
	s, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: sock,
		ListenAddr:     listenAddr,
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

	if got := s.TCPAddr(); got == "" {
		t.Fatalf("TCPAddr empty after configured listener")
	}

	// Dial the TCP listener — pure plain-text, no TLS at this point.
	conn, err := grpc.NewClient(s.TCPAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer conn.Close()

	resp, err := pb.NewClusterClient(conn).Status(context.Background(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatalf("Status over TCP: %v", err)
	}
	if resp.GetInitialized() {
		t.Errorf("Initialized = true on fresh server")
	}
}

func TestServer_StopRemovesSocketFile(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{UnixSocketPath: sock})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Stop(ctx)

	// Socket file should be removed.
	if _, err := net.Dial("unix", sock); err == nil {
		t.Errorf("socket still reachable after Stop")
	}
}
