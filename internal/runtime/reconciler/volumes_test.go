package reconciler_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	schedulerhealth "github.com/PatrickRuddiman/jaco/internal/scheduler/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type legacyVolumeDocker struct {
	*fakeDocker
}

func (d *legacyVolumeDocker) ContainerInspect(ctx context.Context, id string) (types.ContainerJSON, error) {
	info, err := d.fakeDocker.ContainerInspect(ctx, id)
	if err == nil {
		info.Mounts = []types.MountPoint{{Type: "volume", Name: "jaco_orders_prod_data", Destination: "/data"}}
	}
	return info, err
}

func (d *legacyVolumeDocker) VolumeInspect(_ context.Context, name string) (volume.Volume, error) {
	if name == "jaco_orders_prod_data" {
		return volume.Volume{Name: name}, nil
	}
	return volume.Volume{}, errdefs.NotFound(errors.New("no such volume"))
}

type volumeTestLeader struct{}

func (volumeTestLeader) IsLeader() bool { return true }

func TestReconciler_VolumePreflightReportsPendingAndPreservesContainer(t *testing.T) {
	for _, tc := range []struct {
		name, definition, code string
	}{
		{"legacy", "{}", "volume_migration_required"},
		{"missing_external", "\n    name: missing-data\n    external: true", "external_volume_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brokers := watch.NewRegistry()
			st := state.New(brokers)
			st.Cluster.Set(&pb.ClusterMeta{ClusterId: "cluster-x"}, 1)
			st.Nodes.Apply(&pb.Node{Hostname: "host-a", Status: pb.NodeStatus_NODE_STATUS_READY}, 1)
			st.Deployments.Apply(&pb.Deployment{
				Name: "orders",
				ComposeYaml: []byte(fmt.Sprintf(`services:
  app:
    image: alpine:3.20
    pull_policy: never
    network_mode: none
    volumes: [prod_data:/data]
volumes:
  prod_data: %s
`, tc.definition)),
				Services: []*pb.ServiceSpec{{Name: "app"}},
			}, 1)
			rep := &pb.ReplicaDesired{
				Id: "orders-app-0", Deployment: "orders", Service: "app",
				Host: "host-a", Image: "alpine:3.20", RaftIndex: 1,
			}
			st.ReplicasDesired.Apply(rep, 1)
			d := &legacyVolumeDocker{fakeDocker: newFakeDocker()}
			d.containers["legacy"] = &fakeContainer{
				ID: "legacy", Name: "jaco_orders-app-0", Image: rep.Image, State: "running",
				Labels: map[string]string{
					"jaco.cluster_id": "cluster-x", "jaco.deployment": "orders",
					"jaco.replica_id": rep.Id, "jaco.raft_index": "1",
				},
			}
			reports := make(chan *pb.ReplicaObserved, 16)
			submit := func(ctx context.Context, obs *pb.ReplicaObserved) error {
				select {
				case reports <- obs:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			rec := reconciler.New(d, st, brokers, "host-a", submit, okEnsureSubnet, silentLogger())
			ctx, cancel := context.WithCancel(context.Background())
			rec.Watcher().Start(ctx, rep.Id, "legacy", false)
			done := make(chan error, 1)
			go func() { done <- rec.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("reconciler did not stop")
				}
			})

			timeout := time.After(10 * time.Second)
			var failure *pb.ReplicaObserved
			for failure == nil {
				select {
				case obs := <-reports:
					if obs.GetCode() == tc.code {
						failure = obs
					}
				case <-timeout:
					t.Fatalf("reconciler never reported %s", tc.code)
				}
			}
			if failure.GetState() != pb.ReplicaState_REPLICA_STATE_PENDING ||
				failure.GetContainerId() != "legacy" || failure.GetMessage() == "" {
				t.Fatalf("incomplete migration status: %v", failure)
			}
			if rec.Watcher().Active() != 0 {
				t.Fatal("health watcher could overwrite migration-required status")
			}
			if got := d.snapshotByReplicaID(); len(got) != 1 || got[rep.Id] != "legacy" {
				t.Fatalf("existing container was replaced: %v", got)
			}
			info, err := d.ContainerInspect(ctx, "legacy")
			if err != nil || !info.State.Running {
				t.Fatalf("existing container was stopped: %v, %v", info.State, err)
			}
			commands := 0
			restarter := schedulerhealth.New(st, brokers, volumeTestLeader{}, func([]byte) error {
				commands++
				return nil
			})
			restarter.Handle(watch.Event[*pb.ReplicaObserved]{Kind: watch.KindUpdated, After: failure})
			if commands != 0 {
				t.Fatal("migration status triggered automatic health restart")
			}
		})
	}
}
