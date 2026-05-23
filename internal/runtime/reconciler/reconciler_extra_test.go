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
// ReplicaDesired references a service whose compose_service doesn't
// exist in the deployment's compose. startReplica should surface an
// error (logged), and no container should be created.
func TestReconciler_StartReplicaFailsWhenComposeServiceMissing(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64

	// Seed cluster + node manually so we can reference a deployment whose
	// compose_service doesn't match the YAML.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ClusterInit{
		ClusterInit: &pb.ClusterInit{ClusterId: "cluster-x", SelfHostname: "host-a", SelfAddress: "host-a:7000"},
	}}, raftIdx)
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{Hostname: "host-a", Status: pb.NodeStatus_NODE_STATUS_READY},
	}}, raftIdx)

	// Deployment whose service references a compose_service that isn't
	// in the compose YAML.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: "smoke", Revision: 1,
			ComposeYaml: []byte("services:\n  web:\n    image: nginx:1.27\n"),
			Services:    []*pb.ServiceSpec{{Name: "web", Replicas: 1, ComposeService: "ghost"}},
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
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	// Wait briefly — startReplica should fail; no container created.
	time.Sleep(200 * time.Millisecond)
	if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; ok {
		t.Errorf("startReplica created a container despite missing compose_service")
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

// TestResolveDNSServers_PicksGatewayPerKnownSubnet — the per-bridge
// DNS gateway IPs (the per-network resolvers running on the local
// node). Networks the daemon doesn't yet know a CIDR for are skipped.
//
// resolveDNSServers is unexported so we exercise it via the reconciler
// startReplica path. Easier: seed Subnets and let lifecycle.Start
// observe the resulting spec.DNSServers via the fake docker's
// containerCreate config.
//
// Here we just confirm the indirect plumbing works end-to-end: with a
// Subnet present, the started container has a DNS entry; without, it
// doesn't.
func TestResolveDNSServers_PopulatesContainerDNSWhenSubnetKnown(t *testing.T) {
	t.Skip("the fakeDocker in this package doesn't capture HostConfig.DNS; covered indirectly by integration tests")

	// Left as a marker — the reason this isn't covered:
	// reconciler.startReplica calls resolveDNSServers and writes
	// spec.DNSServers into the lifecycle.ContainerSpec, which passes
	// through to docker via HostConfig.DNS. The package's fakeDocker in
	// reconciler_test.go doesn't capture HostConfig (it ignores the
	// argument). Adding capture would require modifying fakeDocker
	// signature; the function is small enough that the build-tag
	// integration test exercises it on a real docker engine.
	_ = state.SubnetKey
}
