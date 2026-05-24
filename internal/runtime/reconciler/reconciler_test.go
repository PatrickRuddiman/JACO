package reconciler_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
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

func (f *fakeDocker) ImagePull(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(`{}`)), nil
}

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

func (f *fakeDocker) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	return nil, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, _ string, _ network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, nil
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
			Services: []*pb.ServiceSpec{{Name: "web", Replicas: 1, Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD}},
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

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
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

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
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

// TestReconciler_OrphanSweepStopsContainerWhenDesiredMovedHosts covers bug
// 016: after a drain migrates a replica off this host, the local container
// must be stop+removed even if the watch event was missed. The safety tick
// runs orphanSweep which compares running containers (by cluster_id label)
// against state.ReplicasDesired filtered to host=self; anything not in the
// expected set gets reaped.
func TestReconciler_OrphanSweepStopsContainerWhenDesiredMovedHosts(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	// Pre-seed a container labeled with this cluster_id + a replica_id
	// that doesn't appear in ReplicasDesired host=self. Simulates the
	// post-drain state where the watch event was missed: a container is
	// still running locally for a replica that's now desired elsewhere.
	d.mu.Lock()
	d.idSeq++
	id := fmt.Sprintf("c-%d", d.idSeq)
	d.containers[id] = &fakeContainer{
		ID:    id,
		Name:  "jaco_smoke-web-0",
		Image: "nginx:1.27",
		Labels: map[string]string{
			"jaco.cluster_id": "cluster-x",
			"jaco.replica_id": "smoke-web-0",
		},
		State: "running",
	}
	d.mu.Unlock()

	rec := reconciler.New(d, st, brokers, "host-a", noopSubmit, okEnsureSubnet, silentLogger())
	if err := rec.OrphanSweep(context.Background()); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if _, ok := d.snapshotByReplicaID()["smoke-web-0"]; ok {
		t.Fatalf("orphan container for smoke-web-0 still present; sweep failed")
	}
}

// noopSubmit is the SubmitFn used in tests — drops observations on the floor
// since the reconciler tests assert on docker state, not raft state.
func noopSubmit(_ context.Context, _ *pb.ReplicaObserved) error { return nil }

func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// Ensure the SubmitFn type satisfies health.SubmitFn at compile time.
var _ health.SubmitFn = noopSubmit

// okEnsureSubnet is the happy-path allocator fake: every network resolves to
// a fixed /24 so startReplica proceeds to lifecycle.Start.
func okEnsureSubnet(_ context.Context, _, _, _ string) (string, error) {
	return "10.244.0.0/24", nil
}

// recordingSubmit captures every ReplicaObserved the reconciler publishes.
type recordingSubmit struct {
	mu  sync.Mutex
	obs []*pb.ReplicaObserved
}

func (r *recordingSubmit) fn(_ context.Context, o *pb.ReplicaObserved) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs = append(r.obs, o)
	return nil
}

func (r *recordingSubmit) failedWithCode(code string) *pb.ReplicaObserved {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.obs {
		if o.GetState() == pb.ReplicaState_REPLICA_STATE_FAILED && o.GetCode() == code {
			return o
		}
	}
	return nil
}

// TestReconciler_PoolExhaustionMarksReplicaFailed — when ensureSubnet reports
// the pool is exhausted, the replica is published FAILED/subnet_pool_exhausted
// and no container is created.
func TestReconciler_PoolExhaustionMarksReplicaFailed(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := &recordingSubmit{}
	exhausted := func(_ context.Context, _, _, _ string) (string, error) {
		return "", reconciler.ErrSubnetPoolExhausted
	}
	r := reconciler.New(d, st, brokers, "host-a", rec.fn, exhausted, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.failedWithCode("subnet_pool_exhausted") != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if rec.failedWithCode("subnet_pool_exhausted") == nil {
		t.Fatalf("no FAILED/subnet_pool_exhausted observation was published")
	}
	if got := len(d.snapshotByReplicaID()); got != 0 {
		t.Errorf("created %d containers despite pool exhaustion; want 0", got)
	}
}

// TestReconciler_TransientAllocErrorDoesNotFail — a transient ensureSubnet
// error (e.g. no leader yet) leaves the replica unstarted for the next tick;
// it must NOT be marked FAILED and must NOT create a container.
func TestReconciler_TransientAllocErrorDoesNotFail(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	d := newFakeDocker()
	var raftIdx uint64
	seedAll(t, f, &raftIdx)

	rec := &recordingSubmit{}
	transient := func(_ context.Context, _, _, _ string) (string, error) {
		return "", errors.New("no leader gRPC address known")
	}
	r := reconciler.New(d, st, brokers, "host-a", rec.fn, transient, silentLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	raftIdx++
	apply(t, f, &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{Replica: &pb.ReplicaDesired{
			Id: "smoke-web-0", Deployment: "smoke", Service: "web", Index: 0, Host: "host-a", Image: "nginx:1.27",
		}},
	}}, raftIdx)

	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	if o := rec.failedWithCode("subnet_pool_exhausted"); o != nil {
		t.Errorf("transient error was wrongly marked FAILED")
	}
	if got := len(d.snapshotByReplicaID()); got != 0 {
		t.Errorf("created %d containers despite transient alloc error; want 0", got)
	}
}
