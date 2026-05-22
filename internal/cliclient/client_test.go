package cliclient_test

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/cliclient"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeCluster is a minimal Cluster server for rotation tests. The Status
// handler returns the value supplied at construction; counters expose how
// many times it was called.
type fakeCluster struct {
	pb.UnimplementedClusterServer
	statusErr  error
	statusResp *pb.ClusterStatusResponse
	calls      int64
}

func (f *fakeCluster) Status(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.statusResp, nil
}

func startFakeServer(t *testing.T, statusErr error, statusResp *pb.ClusterStatusResponse) (string, *fakeCluster) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeCluster{statusErr: statusErr, statusResp: statusResp}
	pb.RegisterClusterServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fake
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestClient_InvokeSucceedsOnFirstReachableServer(t *testing.T) {
	dead := freeAddr(t)
	noLeaderErr := newNoLeaderStatus()
	noLeaderAddr, noLeaderFake := startFakeServer(t, noLeaderErr, nil)
	wantResp := &pb.ClusterStatusResponse{Leader: "node-c"}
	goodAddr, goodFake := startFakeServer(t, nil, wantResp)

	c := cliclient.NewInsecure(cliclient.InsecureOptions{
		Addrs: []string{dead, noLeaderAddr, goodAddr},
		Token: "any-token",
	})
	t.Cleanup(func() { _ = c.Close() })

	var got *pb.ClusterStatusResponse
	err := c.Invoke(context.Background(), func(conn *grpc.ClientConn) error {
		resp, e := pb.NewClusterClient(conn).Status(c.AuthContext(context.Background()), &pb.ClusterStatusRequest{})
		got = resp
		return e
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.GetLeader() != "node-c" {
		t.Errorf("response leader = %q, want node-c", got.GetLeader())
	}
	if atomic.LoadInt64(&noLeaderFake.calls) == 0 {
		t.Errorf("no_leader server was never tried")
	}
	if atomic.LoadInt64(&goodFake.calls) == 0 {
		t.Errorf("third server was never tried")
	}
}

func TestClient_InvokeReturnsNonRetryableErrorImmediately(t *testing.T) {
	pdErr := status.Error(codes.PermissionDenied, "denied")
	pdAddr, pdFake := startFakeServer(t, pdErr, nil)
	goodAddr, goodFake := startFakeServer(t, nil, &pb.ClusterStatusResponse{})

	c := cliclient.NewInsecure(cliclient.InsecureOptions{
		Addrs: []string{pdAddr, goodAddr},
		Token: "x",
	})
	t.Cleanup(func() { _ = c.Close() })

	err := c.Invoke(context.Background(), func(conn *grpc.ClientConn) error {
		_, e := pb.NewClusterClient(conn).Status(c.AuthContext(context.Background()), &pb.ClusterStatusRequest{})
		return e
	})
	if err == nil {
		t.Fatalf("expected PermissionDenied to surface")
	}
	if sErr, _ := status.FromError(err); sErr.Code() != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", sErr.Code())
	}
	if atomic.LoadInt64(&pdFake.calls) != 1 {
		t.Errorf("first server should have been hit exactly once; got %d", pdFake.calls)
	}
	if atomic.LoadInt64(&goodFake.calls) != 0 {
		t.Errorf("second server should NOT have been tried for a non-retryable error")
	}
}

func TestClient_InvokeAllEndpointsExhausted(t *testing.T) {
	d1 := freeAddr(t)
	d2 := freeAddr(t)
	c := cliclient.NewInsecure(cliclient.InsecureOptions{Addrs: []string{d1, d2}, Token: "x"})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Invoke(ctx, func(conn *grpc.ClientConn) error {
		_, e := pb.NewClusterClient(conn).Status(c.AuthContext(ctx), &pb.ClusterStatusRequest{})
		return e
	})
	if err == nil {
		t.Fatalf("expected all-endpoints error")
	}
	if !strings.Contains(err.Error(), "all endpoints unreachable") {
		t.Errorf("err = %v; want 'all endpoints unreachable'", err)
	}
}

func TestClient_ConnReusesAfterSuccessfulInvoke(t *testing.T) {
	addr, fake := startFakeServer(t, nil, &pb.ClusterStatusResponse{Leader: "ok"})
	c := cliclient.NewInsecure(cliclient.InsecureOptions{Addrs: []string{addr}, Token: "x"})
	t.Cleanup(func() { _ = c.Close() })

	for i := 0; i < 3; i++ {
		err := c.Invoke(context.Background(), func(conn *grpc.ClientConn) error {
			_, e := pb.NewClusterClient(conn).Status(c.AuthContext(context.Background()), &pb.ClusterStatusRequest{})
			return e
		})
		if err != nil {
			t.Fatalf("Invoke #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&fake.calls); got != 3 {
		t.Errorf("call count = %d, want 3", got)
	}
}

func TestClient_AuthContextAttachesBearer(t *testing.T) {
	c := cliclient.NewInsecure(cliclient.InsecureOptions{Addrs: []string{"x"}, Token: "test-token"})
	t.Cleanup(func() { _ = c.Close() })
	ctx := c.AuthContext(context.Background())
	md, _ := metadata.FromOutgoingContext(ctx)
	if got := md.Get("authorization"); len(got) == 0 || got[0] != "Bearer test-token" {
		t.Errorf("authorization metadata = %v, want [Bearer test-token]", got)
	}
}

func TestClient_NoTokenLeavesContextUnchanged(t *testing.T) {
	c := cliclient.NewInsecure(cliclient.InsecureOptions{Addrs: []string{"x"}, Token: ""})
	t.Cleanup(func() { _ = c.Close() })
	ctx := c.AuthContext(context.Background())
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("authorization")) > 0 {
		t.Errorf("expected no authorization when token is empty; got %v", md.Get("authorization"))
	}
}

// newNoLeaderStatus mirrors what the real server returns: Unavailable code,
// "no_leader" message, and a typed pb.Error detail.
func newNoLeaderStatus() error {
	st := status.New(codes.Unavailable, "no_leader")
	if d, err := st.WithDetails(&pb.Error{Code: "no_leader", Message: "no leader yet"}); err == nil {
		st = d
	}
	return st.Err()
}
