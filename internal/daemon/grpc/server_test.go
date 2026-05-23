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

func TestServer_InitReturnsUnimplemented(t *testing.T) {
	conn, _ := startServer(t)
	c := pb.NewClusterClient(conn)
	_, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err == nil {
		t.Fatalf("Init should be Unimplemented in this iter")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
	if !strings.Contains(st.Message(), "cluster_init_unimplemented") {
		t.Errorf("message = %q", st.Message())
	}
}

func TestServer_JoinReturnsUnimplemented(t *testing.T) {
	conn, _ := startServer(t)
	c := pb.NewClusterClient(conn)
	_, err := c.Join(context.Background(), &pb.ClusterJoinRequest{})
	if err == nil {
		t.Fatalf("Join should be Unimplemented in this iter")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
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

func TestServer_AfterInitializedGatedMethodsStillSurfaceUnimplemented(t *testing.T) {
	// Once the gate is open, non-allow-listed RPCs no longer return
	// cluster_uninitialized — they fall through to their actual handlers.
	// Since this iter ships Cluster.{Init,Join} as Unimplemented stubs,
	// they should now return Unimplemented INSTEAD of cluster_uninitialized.
	// (Bootstrap will hit the embedded UnimplementedClusterServer.)
	conn, s := startServer(t)
	s.Gate().MarkInitialized()
	c := pb.NewClusterClient(conn)

	_, err := c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if err == nil {
		t.Fatalf("Bootstrap unimplemented in this iter")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented (not Unavailable)", st.Code())
	}
}

func TestServer_NewRejectsMissingSocketPath(t *testing.T) {
	if _, err := dgrpc.New(dgrpc.Options{}); err == nil {
		t.Errorf("expected error on empty UnixSocketPath")
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
