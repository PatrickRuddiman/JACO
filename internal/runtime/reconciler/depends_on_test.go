package reconciler_test

import (
	"context"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// composeWithDependsOn is the deployment fixture for the depends_on gate
// tests: web waits on api with the default (service_started) condition.
const composeWithDependsOn = `services:
  api:
    image: api:1.0
  web:
    image: nginx:1.27
    depends_on: [api]
`

// seedDependsOn seeds the same shape as seedAll but with a 2-service
// deployment carrying compose-level depends_on. raftIdx is advanced
// in place so the caller can keep applying commands afterward.
func seedDependsOn(t *testing.T, f *fsm.FSM, raftIdx *uint64) {
	t.Helper()
	*raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
		ClusterId: "cluster-x", SelfHostname: "host-a", SelfAddress: "host-a:7000",
	}}}, *raftIdx)

	*raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "host-a", Status: pb.NodeStatus_NODE_STATUS_READY},
	}}, *raftIdx)

	*raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "stack", Revision: 1, ComposeYaml: []byte(composeWithDependsOn),
			Services: []*pb.ServiceSpec{
				{Name: "api", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD},
				{Name: "web", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD},
			},
		},
	}}, *raftIdx)
}

func applyReplicaDesired(t *testing.T, f *fsm.FSM, raftIdx *uint64, id, service, host string) {
	t.Helper()
	*raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: id, Deployment: "stack", Service: service, Index: 0, Host: host, Image: "x:1",
		}},
	}}, *raftIdx)
}

func applyReplicaObserved(t *testing.T, f *fsm.FSM, raftIdx *uint64, id string, st pb.ReplicaState) {
	t.Helper()
	*raftIdx++
	cmd := &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{Id: id, State: st},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.Apply(&hraft.Log{Index: *raftIdx, Data: data})
}

// TestReconciler_DependsOnDefersStartUntilDepStarted — issue #130. A web
// replica with `depends_on: [api]` must NOT have a container created
// while api is still in PENDING. The reconciler returns
// ErrDependsOnUnmet and the dispatchStart goroutine self-clears so the
// next watch tick can retry.
func TestReconciler_DependsOnDefersStartUntilDepStarted(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedDependsOn(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	// Both replicas desired on host-a. The api replica is desired but
	// has no observation yet → PENDING → web's depends_on is unmet.
	applyReplicaDesired(t, f, &raftIdx, "stack-api-0", "api", "host-a")
	applyReplicaDesired(t, f, &raftIdx, "stack-web-0", "web", "host-a")

	// Pin the api side as PENDING so the gate has a deterministic state
	// to read (without this it inherits the default PENDING from absence
	// of an observation, which is also unmet — but recording it makes
	// the negative assertion below honest).
	applyReplicaObserved(t, f, &raftIdx, "stack-api-0", pb.ReplicaState_REPLICA_STATE_PENDING)

	// Give the reconciler a few cycles to run.
	time.Sleep(300 * time.Millisecond)
	snap := d.snapshotByReplicaID()
	if _, ok := snap["stack-web-0"]; ok {
		t.Fatalf("web container started while dep api is PENDING")
	}
	// api itself has no depends_on so its container MUST come up.
	if _, ok := snap["stack-api-0"]; !ok {
		t.Fatalf("api container never started; dep gate should not block a dep-less replica")
	}

	cancel()
	<-done
}

// TestReconciler_DependsOnStartsAfterDepRunning — issue #130. Once api
// transitions to RUNNING, the ReplicasObserved watch fires, the
// reconciler re-resyncs, and the web replica's gate passes → container
// created. The whole loop should complete well under the 30s safety tick.
func TestReconciler_DependsOnStartsAfterDepRunning(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedDependsOn(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	applyReplicaDesired(t, f, &raftIdx, "stack-api-0", "api", "host-a")
	applyReplicaDesired(t, f, &raftIdx, "stack-web-0", "web", "host-a")

	// Wait until api is up locally, then publish RUNNING so the gate
	// unblocks. The reconciler's own health.Watcher would normally do
	// this — we drive it directly so the test is deterministic.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["stack-api-0"]; ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := d.snapshotByReplicaID()["stack-api-0"]; !ok {
		t.Fatalf("api container never created")
	}
	applyReplicaObserved(t, f, &raftIdx, "stack-api-0", pb.ReplicaState_REPLICA_STATE_RUNNING)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["stack-web-0"]; ok {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("web container never started after dep api → RUNNING")
}

// TestReconciler_DependsOnHealthyRequiresRunning — issue #130. A
// `condition: service_healthy` wait must NOT satisfy on DEGRADED — the
// operator chose `service_healthy` explicitly because a degraded peer
// is not the same as a healthy one. The gate stays closed until the
// dep transitions to RUNNING.
func TestReconciler_DependsOnHealthyRequiresRunning(t *testing.T) {
	const composeHealthy = `services:
  api:
    image: api:1.0
  web:
    image: nginx:1.27
    depends_on:
      api:
        condition: service_healthy
`
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	// Reuse seedDependsOn's cluster+node bits, then re-apply the
	// deployment with the healthy-condition compose.
	seedDependsOn(t, f, &raftIdx)
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "stack", Revision: 2, ComposeYaml: []byte(composeHealthy),
			Services: []*pb.ServiceSpec{
				{Name: "api", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD},
				{Name: "web", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD},
			},
		},
	}}, raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	applyReplicaDesired(t, f, &raftIdx, "stack-api-0", "api", "host-a")
	applyReplicaDesired(t, f, &raftIdx, "stack-web-0", "web", "host-a")

	// DEGRADED must NOT satisfy service_healthy.
	applyReplicaObserved(t, f, &raftIdx, "stack-api-0", pb.ReplicaState_REPLICA_STATE_DEGRADED)
	time.Sleep(300 * time.Millisecond)
	if _, ok := d.snapshotByReplicaID()["stack-web-0"]; ok {
		t.Fatalf("web started while api is DEGRADED; service_healthy must require RUNNING")
	}

	// RUNNING does satisfy.
	applyReplicaObserved(t, f, &raftIdx, "stack-api-0", pb.ReplicaState_REPLICA_STATE_RUNNING)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["stack-web-0"]; ok {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("web never started after dep api → RUNNING")
}
