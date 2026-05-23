package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeClusterClient implements pb.ClusterClient. Only Init / Status / Join
// are exercised by the cluster + node-join CLI tests.
type fakeClusterClient struct {
	pb.ClusterClient
	initFn   func(ctx context.Context, req *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error)
	statusFn func(ctx context.Context, req *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error)
	joinFn   func(ctx context.Context, req *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error)
}

func (f *fakeClusterClient) Init(ctx context.Context, req *pb.ClusterInitRequest, _ ...grpc.CallOption) (*pb.ClusterInitResponse, error) {
	return f.initFn(ctx, req)
}
func (f *fakeClusterClient) Status(ctx context.Context, req *pb.ClusterStatusRequest, _ ...grpc.CallOption) (*pb.ClusterStatusResponse, error) {
	return f.statusFn(ctx, req)
}
func (f *fakeClusterClient) Join(ctx context.Context, req *pb.ClusterJoinRequest, _ ...grpc.CallOption) (*pb.ClusterJoinResponse, error) {
	return f.joinFn(ctx, req)
}

func TestRunClusterInit_PrintsTokenAndID(t *testing.T) {
	var out bytes.Buffer
	client := &fakeClusterClient{
		initFn: func(_ context.Context, req *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
			if req.GetClusterName() != "my-cluster" {
				t.Errorf("cluster_name = %q, want my-cluster", req.GetClusterName())
			}
			return &pb.ClusterInitResponse{
				ClusterId:     "cid-xyz",
				OperatorToken: "deadbeef1234",
			}, nil
		},
	}
	if err := runClusterInit(context.Background(), client, "my-cluster", &out); err != nil {
		t.Fatalf("runClusterInit: %v", err)
	}
	got := out.String()
	for _, want := range []string{"cid-xyz", "deadbeef1234", "Save the operator token"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunClusterInit_SurfacesServerError(t *testing.T) {
	client := &fakeClusterClient{
		initFn: func(context.Context, *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
			return nil, errors.New("cluster_already_initialized")
		},
	}
	err := runClusterInit(context.Background(), client, "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cluster_already_initialized") {
		t.Errorf("err = %v", err)
	}
}

func TestRunClusterStatus_UninitializedPath(t *testing.T) {
	var out bytes.Buffer
	client := &fakeClusterClient{
		statusFn: func(context.Context, *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
			return &pb.ClusterStatusResponse{Initialized: false}, nil
		},
	}
	if err := runClusterStatus(context.Background(), client, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "uninitialized") {
		t.Errorf("missing 'uninitialized' in %q", got)
	}
	if !strings.Contains(got, "jaco cluster init") {
		t.Errorf("missing 'jaco cluster init' hint in %q", got)
	}
}

func TestRunClusterStatus_InitializedReportsLeaderAndNodes(t *testing.T) {
	var out bytes.Buffer
	client := &fakeClusterClient{
		statusFn: func(context.Context, *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
			return &pb.ClusterStatusResponse{
				Initialized: true,
				Leader:      "node-a",
				RaftIndex:   42,
				Nodes: []*pb.Node{
					{Hostname: "node-a", Address: "10.0.0.1:7001", Status: pb.NodeStatus_NODE_STATUS_READY},
					{Hostname: "node-b", Address: "10.0.0.2:7001", Status: pb.NodeStatus_NODE_STATUS_JOINING},
				},
			}, nil
		},
	}
	if err := runClusterStatus(context.Background(), client, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"initialized", "node-a", "10.0.0.1:7001", "READY",
		"node-b", "JOINING", "Raft index: 42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunClusterStatus_NoLeaderShowsPlaceholder(t *testing.T) {
	var out bytes.Buffer
	client := &fakeClusterClient{
		statusFn: func(context.Context, *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
			return &pb.ClusterStatusResponse{Initialized: true, Leader: "", RaftIndex: 0}, nil
		},
	}
	_ = runClusterStatus(context.Background(), client, &out)
	if !strings.Contains(out.String(), "(no leader elected)") {
		t.Errorf("expected '(no leader elected)' placeholder; got:\n%s", out.String())
	}
}

func TestRunNodeJoin_SuccessPrintsConfirmation(t *testing.T) {
	var out bytes.Buffer
	var captured *pb.ClusterJoinRequest
	client := &fakeClusterClient{
		joinFn: func(_ context.Context, req *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
			captured = req
			return &pb.ClusterJoinResponse{}, nil
		},
	}
	if err := runNodeJoin(context.Background(), client, "10.0.0.1:7000", "tok-123", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Joined cluster") {
		t.Errorf("missing confirmation: %s", out.String())
	}
	if captured.GetPeerAddr() != "10.0.0.1:7000" || captured.GetJoinToken() != "tok-123" {
		t.Errorf("request = %+v", captured)
	}
}

func TestRunNodeJoin_SurfacesServerError(t *testing.T) {
	client := &fakeClusterClient{
		joinFn: func(context.Context, *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
			return nil, errors.New("join_token_invalid")
		},
	}
	err := runNodeJoin(context.Background(), client, "10.0.0.1:7000", "tok-bad", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "join_token_invalid") {
		t.Errorf("err = %v", err)
	}
}

func TestSocketDefault_HonorsEnvOverride(t *testing.T) {
	t.Setenv("JACO_SOCKET", "/tmp/custom.sock")
	if got := socketDefault(); got != "/tmp/custom.sock" {
		t.Errorf("socketDefault = %q, want /tmp/custom.sock", got)
	}
	t.Setenv("JACO_SOCKET", "")
	if got := socketDefault(); got != DefaultDaemonSocket {
		t.Errorf("socketDefault unset = %q, want %s", got, DefaultDaemonSocket)
	}
}
