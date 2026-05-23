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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// withOperatorAuth returns a context carrying the Bearer-token metadata the
// daemon's admission interceptor expects post-init. Used in tests that
// drive a full Init → second-call flow.
func withOperatorAuth(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// waitForOperatorToken polls until state.Tokens has at least one entry —
// raft applies are async after OpenRaft, so the operator token from
// bootstrap takes a moment to replay into the daemon's state.
func waitForOperatorToken(t *testing.T, s *dgrpc.Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.State() != nil && s.State().Tokens.Len() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operator token never appeared in state.Tokens")
}

// freePort returns a tcp port nothing's listening on. Closes the listener
// but the port may be momentarily reused by something else; for tests
// running in tight loops this is rare enough not to matter.
func freePort(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// startServerWithDataDir spins up a jacod gRPC server with a working
// DataDir + Hostname override + a free-port ClusterAddr so post-Init the
// raft re-open lands on the same port bootstrap recorded.
func startServerWithDataDir(t *testing.T, dataDir string) (*grpc.ClientConn, *dgrpc.Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: sock,
		DataDir:        dataDir,
		Hostname:       "test-host",
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
	conn, s := startServerWithDataDir(t, dataDir)
	c := pb.NewClusterClient(conn)

	resp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitForOperatorToken(t, s)
	// Second call must refuse — attach the operator token returned by
	// the first Init so admission lets the call reach the handler.
	authCtx := withOperatorAuth(context.Background(), resp.GetOperatorToken())
	_, err = c.Init(authCtx, &pb.ClusterInitRequest{})
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
	// Post-Init with the operator token: Bootstrap falls through to the
	// embedded UnimplementedClusterServer and returns Unimplemented.
	conn, s := startServerWithDataDir(t, t.TempDir())
	c := pb.NewClusterClient(conn)

	_, err := c.Bootstrap(context.Background(), &pb.BootstrapRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("pre-Init Bootstrap code = %v, want Unavailable", st.Code())
	}

	initResp, err := c.Init(context.Background(), &pb.ClusterInitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the post-OpenRaft FSM replay to land the operator token in
	// state.Tokens before exercising the admission interceptor.
	waitForOperatorToken(t, s)

	authCtx := withOperatorAuth(context.Background(), initResp.GetOperatorToken())
	_, err = c.Bootstrap(authCtx, &pb.BootstrapRequest{})
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented {
		t.Errorf("post-Init Bootstrap code = %v, want Unimplemented", st.Code())
	}
}

func TestInit_OpensRaftAndStatusReportsLeader(t *testing.T) {
	// After Init, Cluster.Status should report a non-zero raft_index +
	// the leader (which is this node since we're a single-node cluster).
	conn, _ := startServerWithDataDir(t, t.TempDir())
	c := pb.NewClusterClient(conn)

	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}

	// Wait briefly for raft to elect itself leader.
	var resp *pb.ClusterStatusResponse
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		resp, err = c.Status(context.Background(), &pb.ClusterStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetLeader() != "" && resp.GetRaftIndex() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resp.GetLeader() == "" {
		t.Errorf("Status.Leader empty post-Init (raft hasn't elected yet?)")
	}
	if resp.GetRaftIndex() == 0 {
		t.Errorf("Status.RaftIndex = 0; bootstrap should have applied ClusterInit + ≥1 raft log entries")
	}
	if !resp.GetInitialized() {
		t.Errorf("Status.Initialized = false post-Init")
	}
}
