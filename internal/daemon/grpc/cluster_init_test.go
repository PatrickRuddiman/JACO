package grpc_test

import (
	"context"
	"net"
	"os"
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

// startServerWithDataDir spins up a jacod gRPC server with a working
// DataDir + Hostname override (so Init can actually run bootstrap.Run).
func startServerWithDataDir(t *testing.T, dataDir string) (*grpc.ClientConn, *dgrpc.Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: sock,
		DataDir:        dataDir,
		Hostname:       "test-host",
		// BindAddr left empty → bootstrap.Run defaults to 127.0.0.1:0.
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
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, s
}

func TestInit_FreshDataDirSucceeds(t *testing.T) {
	dataDir := t.TempDir()
	conn, s := startServerWithDataDir(t, dataDir)
	c := pb.NewClusterClient(conn)

	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if resp.GetClusterId() == "" {
		t.Errorf("ClusterId empty")
	}
	if got := resp.GetOperatorToken(); len(got) != 64 {
		t.Errorf("OperatorToken len = %d, want 64 hex chars", len(got))
	}
	if !s.Gate().IsInitialized() {
		t.Errorf("InitGate not flipped to initialized")
	}
	// Raft state persisted to disk.
	if _, err := os.Stat(filepath.Join(dataDir, "raft", "log.db")); err != nil {
		t.Errorf("raft/log.db missing: %v", err)
	}
	// Node certs persisted.
	if _, err := os.Stat(filepath.Join(dataDir, "node", "test-host.crt")); err != nil {
		t.Errorf("node cert missing: %v", err)
	}
}

func TestInit_StatusFlipsToInitialized(t *testing.T) {
	dataDir := t.TempDir()
	conn, _ := startServerWithDataDir(t, dataDir)
	c := pb.NewClusterClient(conn)

	before, _ := c.Status(context.Background(), &pb.ClusterStatusRequest{})
	if before.GetInitialized() {
		t.Errorf("Initialized=true before Init")
	}
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	after, _ := c.Status(context.Background(), &pb.ClusterStatusRequest{})
	if !after.GetInitialized() {
		t.Errorf("Initialized=false after Init")
	}
}

func TestInit_RefusesWhenAlreadyInitialized(t *testing.T) {
	dataDir := t.TempDir()
	conn, _ := startServerWithDataDir(t, dataDir)
	c := pb.NewClusterClient(conn)

	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	// Second call must refuse.
	_, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err == nil {
		t.Fatal("second Init succeeded; want FailedPrecondition")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
	if !strings.Contains(st.Message(), "cluster_already_initialized") {
		t.Errorf("message = %q, want cluster_already_initialized", st.Message())
	}
}

func TestInit_RefusesWhenRaftStateOnDiskButGateOpen(t *testing.T) {
	// Pre-create $dataDir/raft/log.db; daemon should refuse Init even
	// though the in-memory gate is closed (could happen if a previous
	// Init crashed mid-flight or the operator manually placed state).
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "raft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "raft", "log.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, _ := startServerWithDataDir(t, dataDir)
	c := pb.NewClusterClient(conn)

	_, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err == nil {
		t.Fatal("Init with pre-existing raft state succeeded")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

func TestInit_GatedMethodsUnblockedAfterInit(t *testing.T) {
	// Pre-Init: Bootstrap returns cluster_uninitialized (gate closed).
	// Post-Init: Bootstrap falls through to the embedded
	// UnimplementedClusterServer and returns Unimplemented.
	conn, _ := startServerWithDataDir(t, t.TempDir())
	c := pb.NewClusterClient(conn)

	_, err := c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("pre-Init Bootstrap code = %v, want Unavailable", st.Code())
	}

	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}

	_, err = c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented {
		t.Errorf("post-Init Bootstrap code = %v, want Unimplemented", st.Code())
	}
}
