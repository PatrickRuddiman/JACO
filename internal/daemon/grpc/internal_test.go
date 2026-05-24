package grpc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestInternalSubmit_AppliesCommandWhenLeader proves the daemon's
// Internal.Submit handler raft-applies the given bytes on the local node.
// Used by follower runtimes to forward ReplicaObserved updates to the
// leader.
func TestInternalSubmit_AppliesCommandWhenLeader(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Build a real Command (ReplicaObservedUpdate-shaped) so the FSM
	// processes it without complaint.
	cmd := &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_ReplicaObservedUpdate{
			ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: &pb.ReplicaObserved{
				Id: "smoke-web-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
			}},
		},
	}
	data, _ := proto.Marshal(cmd)

	internal := pb.NewInternalClient(conn)
	resp, err := internal.Submit(context.Background(), &pb.SubmitRequest{CommandBytes: data})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if resp.GetRaftIndex() == 0 {
		t.Errorf("raft_index = 0; expected >0")
	}
}

// TestInternalSubmit_RejectsEmptyBody — sanity check that the handler
// validates the input rather than blindly applying empty bytes.
func TestInternalSubmit_RejectsEmptyBody(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	internal := pb.NewInternalClient(conn)
	_, err := internal.Submit(context.Background(), &pb.SubmitRequest{})
	if err == nil {
		t.Fatalf("Submit with empty body succeeded")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestInternalEnsureSubnet_ReturnsCIDROnLeader — the leader allocates a
// per-host /24 and the second call for the same tuple is idempotent.
func TestInternalEnsureSubnet_ReturnsCIDROnLeader(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	internal := pb.NewInternalClient(conn)
	req := &pb.EnsureSubnetRequest{Deployment: "sample", Network: "frontend", Host: "host-a"}
	first, err := internal.EnsureSubnet(context.Background(), req)
	if err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	if first.GetCidr() == "" {
		t.Fatalf("EnsureSubnet returned empty cidr")
	}
	second, err := internal.EnsureSubnet(context.Background(), req)
	if err != nil {
		t.Fatalf("EnsureSubnet (idempotent): %v", err)
	}
	if first.GetCidr() != second.GetCidr() {
		t.Errorf("not idempotent: %s vs %s", first.GetCidr(), second.GetCidr())
	}
}

// TestInternalEnsureSubnet_RejectsEmptyFields — every tuple field is required.
func TestInternalEnsureSubnet_RejectsEmptyFields(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	internal := pb.NewInternalClient(conn)
	_, err := internal.EnsureSubnet(context.Background(), &pb.EnsureSubnetRequest{Deployment: "sample", Network: "frontend"})
	if err == nil {
		t.Fatalf("EnsureSubnet with empty host succeeded")
	}
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestInternalEnsureSubnet_UnavailableBeforeInit — before OpenRaft the
// handler has no raft handle and must report Unavailable (the same class a
// follower returns as no_leader), never a panic.
func TestInternalEnsureSubnet_UnavailableBeforeInit(t *testing.T) {
	conn, _ := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	internal := pb.NewInternalClient(conn)
	_, err := internal.EnsureSubnet(context.Background(), &pb.EnsureSubnetRequest{
		Deployment: "sample", Network: "frontend", Host: "host-a",
	})
	if err == nil {
		t.Fatalf("EnsureSubnet before Init succeeded")
	}
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
}

// TestInternalEnsureSubnet_PoolExhaustion — once all 256 /24s are taken the
// next request surfaces ResourceExhausted carrying subnet_pool_exhausted.
func TestInternalEnsureSubnet_PoolExhaustion(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	internal := pb.NewInternalClient(conn)
	for n := 0; n < 256; n++ {
		_, err := internal.EnsureSubnet(context.Background(), &pb.EnsureSubnetRequest{
			Deployment: fmt.Sprintf("dep-%d", n), Network: "default", Host: "host-a",
		})
		if err != nil {
			t.Fatalf("alloc %d: %v", n, err)
		}
	}
	_, err := internal.EnsureSubnet(context.Background(), &pb.EnsureSubnetRequest{
		Deployment: "overflow", Network: "default", Host: "host-a",
	})
	if err == nil {
		t.Fatal("expected pool exhaustion error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", st.Code())
	}
	if !strings.Contains(st.Message(), "subnet_pool_exhausted") {
		t.Errorf("message %q lacks subnet_pool_exhausted", st.Message())
	}
}
