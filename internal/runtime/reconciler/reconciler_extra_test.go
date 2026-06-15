package reconciler_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestReconciler_StopReplicaWhenHostChangesAway — a ReplicaDesired
// previously assigned to host-a gets Updated with Host=host-b. The
// local reconciler must stop+remove the container.
func TestReconciler_StopReplicaWhenHostChangesAway(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	// Initial upsert places replica on host-a.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Migrate to host-b — replica still exists in state but on a different
	// host. The local reconciler should stop+remove the local container.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-b", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; !ok {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("container for smoke-web-0 still present after migration to host-b")
}

// TestReconciler_OrphanSweepFailsBeforeClusterMeta — orphanSweep
// returns "cluster meta not populated" when state.Cluster is empty
// (the pre-Init defensive path).
func TestReconciler_OrphanSweepFailsBeforeClusterMeta(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	d := newFakeDocker()
	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	if err := rec.OrphanSweep(context.Background()); err == nil {
		t.Errorf("OrphanSweep with empty cluster meta returned nil err")
	}
}

// TestReconciler_StartReplicaFailsWhenComposeServiceMissing — a
// ReplicaDesired references a service whose name doesn't match any key
// in the deployment's compose file. startReplica should surface an
// error (logged), and no container should be created.
func TestReconciler_StartReplicaFailsWhenComposeServiceMissing(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64

	// Seed cluster + node manually so we can reference a deployment whose
	// service name doesn't match the compose YAML.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ClusterInit{
		ClusterInit: &pb.ClusterInit{ClusterId: "cluster-x", SelfHostname: "host-a", SelfAddress: "host-a:7000"},
	}}, raftIdx)
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "host-a", Status: pb.NodeStatus_NODE_STATUS_READY},
	}}, raftIdx)

	// Deployment whose service name "ghost" isn't a key in the compose YAML.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "smoke", Revision: 1,
			ComposeYaml: []byte("services:\n  web:\n    image: nginx:1.27\n"),
			Services:    []*pb.ServiceSpec{{Name: "ghost", Replicas: 1}},
		},
	}}, raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-ghost-0", Deployment: "smoke", Service: "ghost", Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	// Wait briefly — startReplica should fail; no container created.
	time.Sleep(200 * time.Millisecond)
	if _, ok := d.snapshotByReplicaID()["smoke-ghost-0"]; ok {
		t.Errorf("startReplica created a container despite service not in compose")
	}
	cancel()
	<-done
}

// TestReconciler_StartReplicaFailsWhenDeploymentMissing — replica
// references a deployment that isn't in state.Deployments. Container
// must not be created.
func TestReconciler_StartReplicaFailsWhenDeploymentMissing(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64

	// Seed cluster + node but NOT the deployment.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ClusterInit{
		ClusterInit: &pb.ClusterInit{ClusterId: "cluster-x", SelfHostname: "host-a", SelfAddress: "host-a:7000"},
	}}, raftIdx)
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "host-a", Status: pb.NodeStatus_NODE_STATUS_READY},
	}}, raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "ghost-web-0", Deployment: "ghost-deployment", Service: "web", Host: "host-a", Image: "x",
		}},
	}}, raftIdx)
	time.Sleep(150 * time.Millisecond)
	if _, ok := d.snapshotByReplicaID()["ghost-web-0"]; ok {
		t.Errorf("startReplica created a container for missing deployment")
	}
	cancel()
	<-done
}

// TestWatcher_AccessorReturnsNonNil — Watcher() exposes the per-replica
// health watcher used by tests and the daemon's reconcile loop.
func TestWatcher_AccessorReturnsNonNil(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	d := newFakeDocker()
	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	if rec.Watcher() == nil {
		t.Errorf("Watcher() = nil")
	}
}

// TestReconciler_FirstReplicaOnFreshBridgeGetsBridgeDNS is the regression test
// for #181. The first replica scheduled onto a fresh (host, deployment,
// network) bridge must still receive the per-bridge gateway as its DNS server,
// even though that subnet hasn't yet replicated into local state.Subnets.
//
// seedAll seeds cluster/node/deployment but never state.Subnets, and
// okEnsureSubnet hands back 10.244.0.0/24 without writing state — exactly the
// "fresh bridge, replication lag" condition from the bug. The reconciler now
// derives the gateway (10.244.0.1) from the cidr ensureSubnet returns instead
// of re-reading state.Subnets, so HostConfig.DNS is populated. Pre-fix it was
// empty and docker fell back to the host resolver.
func TestReconciler_FirstReplicaOnFreshBridgeGetsBridgeDNS(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dns, ok := d.dnsForReplica("smoke-web-0"); ok {
			cancel()
			<-done
			if len(dns) != 1 || dns[0] != "10.244.0.1" {
				t.Fatalf("HostConfig.DNS = %v, want [10.244.0.1]", dns)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("container for replica smoke-web-0 never created")
}
