package grpc_test

import (
	"context"
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
