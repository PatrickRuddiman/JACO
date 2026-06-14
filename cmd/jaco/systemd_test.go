package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestRunClusterInit_EnablesServiceWhenRequested(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		initFn: func(context.Context, *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
			return &pb.ClusterInitResponse{ClusterId: "c", OperatorToken: "t"}, nil
		},
	}
	if err := runClusterInit(context.Background(), client, "", true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("systemdEnabler called %d times, want 1", calls)
	}
}

func TestRunClusterInit_SkipsServiceWhenDisabled(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		initFn: func(context.Context, *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
			return &pb.ClusterInitResponse{ClusterId: "c", OperatorToken: "t"}, nil
		},
	}
	if err := runClusterInit(context.Background(), client, "", false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("systemdEnabler called %d times, want 0", calls)
	}
}

func TestRunClusterInit_DoesNotEnableOnError(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		initFn: func(context.Context, *pb.ClusterInitRequest) (*pb.ClusterInitResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}
	if err := runClusterInit(context.Background(), client, "", true, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
	if calls != 0 {
		t.Errorf("systemdEnabler called %d times on failed init, want 0", calls)
	}
}

func TestRunNodeJoin_EnablesServiceWhenRequested(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		joinFn: func(context.Context, *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
			return &pb.ClusterJoinResponse{}, nil
		},
	}
	if err := runNodeJoin(context.Background(), client, "10.0.0.1:7000", "tok", true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("systemdEnabler called %d times, want 1", calls)
	}
}

func TestRunNodeJoin_SkipsServiceWhenDisabled(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		joinFn: func(context.Context, *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
			return &pb.ClusterJoinResponse{}, nil
		},
	}
	if err := runNodeJoin(context.Background(), client, "10.0.0.1:7000", "tok", false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("systemdEnabler called %d times, want 0", calls)
	}
}

func TestRunNodeJoin_DoesNotEnableOnError(t *testing.T) {
	calls := 0
	orig := systemdEnabler
	systemdEnabler = func(io.Writer) { calls++ }
	defer func() { systemdEnabler = orig }()

	client := &fakeClusterClient{
		joinFn: func(context.Context, *pb.ClusterJoinRequest) (*pb.ClusterJoinResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}
	if err := runNodeJoin(context.Background(), client, "10.0.0.1:7000", "tok", true, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
	if calls != 0 {
		t.Errorf("systemdEnabler called %d times on failed join, want 0", calls)
	}
}

// TestEnableJacoService_AlwaysReportsWhatItDid verifies the real helper never
// panics and always writes one of its documented status lines (no-op note,
// success, or failure warning) regardless of whether systemctl is present on
// the host the test runs on.
func TestEnableJacoService_AlwaysReportsWhatItDid(t *testing.T) {
	var out bytes.Buffer
	enableJacoService(&out)
	got := out.String()
	if !strings.Contains(got, "systemctl") && !strings.Contains(got, "boot") {
		t.Errorf("enableJacoService produced no recognizable output:\n%s", got)
	}
}
