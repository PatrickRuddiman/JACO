package reconciler_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	hraft "github.com/hashicorp/raft"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeDocker mirrors the one in internal/runtime/lifecycle/lifecycle_test.go
// but kept local to keep packages independent.
type fakeDocker struct {
	dockerx.Docker
	mu         sync.Mutex
	containers map[string]*fakeContainer
	idSeq      int
}

type fakeContainer struct {
	ID, Name, Image string
	Labels          map[string]string
	State           string
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]*fakeContainer{}} }

func (f *fakeDocker) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idSeq++
	id := fmt.Sprintf("c-%d", f.idSeq)
	labels := map[string]string{}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	f.containers[id] = &fakeContainer{ID: id, Name: name, Image: cfg.Image, Labels: labels, State: "created"}
	return container.CreateResponse{ID: id}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.State = "running"
		return nil
	}
	return fmt.Errorf("no such container %s", id)
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.State = "exited"
	}
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	return nil
}

func (f *fakeDocker) NetworkConnect(_ context.Context, _, _ string, _ *network.EndpointSettings) error {
	return nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return types.ContainerJSON{}, fmt.Errorf("no such container %s", id)
	}
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: c.ID, Name: c.Name, Image: c.Image, State: &types.ContainerState{Status: c.State, Running: c.State == "running"}},
		Config:            &container.Config{Labels: c.Labels},
	}, nil
}

func (f *fakeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	labelFilters := opts.Filters.Get("label")
	var out []types.Container
	for _, c := range f.containers {
		ok := true
		for _, lf := range labelFilters {
			parts := strings.SplitN(lf, "=", 2)
			if len(parts) != 2 || c.Labels[parts[0]] != parts[1] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, types.Container{ID: c.ID, Names: []string{c.Name}, Image: c.Image, Labels: c.Labels, State: c.State})
		}
	}
	return out, nil
}

func (f *fakeDocker) snapshotByReplicaID() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for id, c := range f.containers {
		if rid := c.Labels["jaco.replica_id"]; rid != "" {
			out[rid] = id
		}
	}
	return out
}

const composeYAML = `services:
  web:
    image: nginx:1.27
`

// seedAll seeds cluster meta + a node + a deployment so the reconciler has
// every state piece it needs to project a ReplicaDesired into a spec.
func seedAll(t *testing.T, f *fsm.FSM, raftIdx *uint64) {
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
			Deployment: "smoke", Revision: 1, ComposeYaml: []byte(composeYAML),
			Services: []*pb.ServiceSpec{{Name: "web", Replicas: 1, ComposeService: "web", Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}},
		},
	}}, *raftIdx)
}

func apply(t *testing.T, f *fsm.FSM, cmd *pb.Command, idx uint64) {
	t.Helper()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.Apply(&hraft.Log{Index: idx, Data: data})
}

func TestReconciler_StartsContainerOnReplicaDesiredAdd(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	// Apply a ReplicaDesiredUpsert for host-a.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; ok {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("container for replica smoke-web-0 never created")
}

func TestReconciler_IgnoresReplicaForDifferentHost(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-b", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	time.Sleep(200 * time.Millisecond)
	if got := len(d.snapshotByReplicaID()); got != 0 {
		t.Errorf("created %d containers for host-b replica; want 0", got)
	}
	cancel()
	<-done
}

func TestReconciler_RemovesContainerOnReplicaDesiredDelete(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, silentLogger())
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

	// Wait for the container to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; !ok {
		t.Fatal("container never created")
	}

	// Now apply a Remove.
	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredRemove{
		ReplicaDesiredRemove: &pb.ReplicaDesiredRemove{Id: "smoke-web-0"},
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
	t.Fatalf("container still present after Remove")
}

// noopSubmit is the SubmitFn used in tests — drops observations on the floor
// since the reconciler tests assert on docker state, not raft state.
func noopSubmit(_ context.Context, _ *pb.ReplicaObserved) error { return nil }

func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// Ensure the SubmitFn type satisfies health.SubmitFn at compile time.
var _ health.SubmitFn = noopSubmit
