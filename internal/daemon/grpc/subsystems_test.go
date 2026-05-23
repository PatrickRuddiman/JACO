package grpc_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const subsystemsTestCompose = `services:
  web:
    image: nginx:1.27
`

// TestSubsystems_SchedulerMaterializesReplicaDesired proves the scheduler
// goroutine OpenRaft spawns is actively reconciling: after Init seeds the
// local node + we raft-Apply a DeploymentApply, ReplicasDesired must show
// the new replica within ~1s (scheduler debounce = 50ms + raft Apply RTT).
func TestSubsystems_SchedulerMaterializesReplicaDesired(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)

	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Wait for raft leader election on the single-voter cluster.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Raft() == nil || !s.Raft().IsLeader() {
		t.Fatalf("never became leader")
	}

	// Apply a Deployment via raft.
	cmd := &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_DeploymentApply{
			DeploymentApply: &pb.DeploymentApply{
				Deployment:  "smoke",
				Revision:    1,
				ComposeYaml: []byte(subsystemsTestCompose),
				Services: []*pb.ServiceSpec{{
					Name:           "web",
					Replicas:       1,
					ComposeService: "web",
					Placement:      pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
				}},
			},
		},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.Raft().Apply(data, 2*time.Second); err != nil {
		t.Fatalf("raft.Apply DeploymentApply: %v", err)
	}

	// Poll state.ReplicasDesired — the scheduler subscribed to Deployments
	// at OpenRaft time, so it should fire reconcile within DebounceWindow
	// after the FSM applies the upsert.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.State().ReplicasDesired.Len() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("scheduler never materialized a ReplicaDesired entry")
}

// TestSubsystems_StopDrainsGoroutinesCleanly verifies Stop cancels every
// subsystem goroutine inside the 5s budget hardcoded in Server.Stop. If a
// subsystem ignored ctx.Done() this test would hit the cleanup timeout.
func TestSubsystems_StopDrainsGoroutinesCleanly(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Wait for the OpenRaft path to have completed (Init returns after it).
	if s.Raft() == nil {
		t.Fatalf("raft handle nil after Init")
	}
	_ = conn.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	s.Stop(ctx)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("Stop took %v; subsystem goroutines did not cancel promptly", elapsed)
	}
}
