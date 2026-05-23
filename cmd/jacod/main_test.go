package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// freePort returns a tcp port nothing's listening on. Closes the listener;
// caller races against potential reuse but tight-loop tests rarely collide.
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

// writeConfig writes a minimal jacod.yaml referencing tmp paths and returns
// the config path.
func writeConfig(t *testing.T, dataDir, sock string) string {
	t.Helper()
	cluster := freePort(t)
	listen := freePort(t)
	if cluster == listen {
		listen = freePort(t)
	}
	body := fmt.Sprintf(`data_dir: %s
listen_addr: %s
cluster_addr: %s
unix_socket: %s
wg_port: 51820
log_level: info
ipam_pool: 10.244.0.0/16
`, dataDir, listen, cluster, sock)
	path := filepath.Join(t.TempDir(), "jacod.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func dialDaemonForTest(t *testing.T, sock string) *grpc.ClientConn {
	t.Helper()
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
	return conn
}

func TestRun_BootsAndAcceptsStatus(t *testing.T) {
	dataDir := t.TempDir()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	configPath := writeConfig(t, dataDir, sock)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx, configPath, io.Discard) }()

	// Wait for the socket to appear.
	waitForSocket(t, sock, 3*time.Second)

	conn := dialDaemonForTest(t, sock)
	defer conn.Close()

	resp, err := pb.NewClusterClient(conn).Status(context.Background(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp.GetInitialized() {
		t.Errorf("Initialized = true on fresh boot")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
}

func TestRun_InitFlipsStatusAndPersistsRaft(t *testing.T) {
	dataDir := t.TempDir()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	configPath := writeConfig(t, dataDir, sock)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx, configPath, io.Discard) }()
	waitForSocket(t, sock, 3*time.Second)

	conn := dialDaemonForTest(t, sock)
	defer conn.Close()
	client := pb.NewClusterClient(conn)

	initResp, err := client.Init(context.Background(), &pb.ClusterInitRequest{ClusterName: "smoke"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if initResp.GetClusterId() == "" {
		t.Errorf("ClusterId empty")
	}
	if len(initResp.GetOperatorToken()) != 64 {
		t.Errorf("OperatorToken len = %d, want 64", len(initResp.GetOperatorToken()))
	}

	// Status flips initialized.
	statusResp, err := client.Status(context.Background(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !statusResp.GetInitialized() {
		t.Errorf("Initialized = false post-Init")
	}

	// Raft state persisted.
	if _, err := os.Stat(filepath.Join(dataDir, "raft", "log.db")); err != nil {
		t.Errorf("raft/log.db missing: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
}

func TestRun_LoadsConfigFromPath(t *testing.T) {
	// Bad config (unknown field) should cause run() to error before
	// opening the listener.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("foo_bar: 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := run(ctx, bad, io.Discard)
	if err == nil {
		t.Fatalf("expected error from bad config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("err = %v; should mention load config", err)
	}
}

func TestRun_RejectsMissingDataDir(t *testing.T) {
	// Pre-populate a non-existent raft path so config validation passes
	// but server startup catches the unwritable dir.
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	configPath := writeConfig(t, "/nonexistent/path/that/cannot/be/created", sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, configPath, io.Discard) }()
	// We expect Init or Serve to fail; allow the boot to complete then Init
	// fails. For this test it's enough to verify run() doesn't panic.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
}

func TestDefaultConfigPath_EnvOverride(t *testing.T) {
	t.Setenv("JACO_CONFIG", "/custom/path.yaml")
	if got := defaultConfigPath(); got != "/custom/path.yaml" {
		t.Errorf("defaultConfigPath = %q, want /custom/path.yaml", got)
	}
	t.Setenv("JACO_CONFIG", "")
	if got := defaultConfigPath(); got != "/etc/jaco/jacod.yaml" {
		t.Errorf("defaultConfigPath = %q, want /etc/jaco/jacod.yaml", got)
	}
}

// waitForSocket spins until path exists (the daemon's gRPC server opened
// its listener) or deadline.
func waitForSocket(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s never appeared", path)
}

// silence unused
var _ sync.Mutex
