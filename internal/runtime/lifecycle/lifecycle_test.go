package lifecycle_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
)

// fakeDocker is an in-memory partial implementation of dockerx.Docker. It
// tracks containers by id and supports the methods lifecycle exercises:
// ContainerList (label filters), Create, Start, Stop, Remove, Inspect.
type fakeDocker struct {
	dockerx.Docker
	mu         sync.Mutex
	containers map[string]*fakeContainer
	idSeq      int
	createErr  error
}

type fakeContainer struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string
	State  string
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]*fakeContainer{}} }

func (f *fakeDocker) ContainerCreate(_ context.Context, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	f.idSeq++
	id := fmt.Sprintf("c-%d", f.idSeq)
	labels := map[string]string{}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	f.containers[id] = &fakeContainer{
		ID:     id,
		Name:   name,
		Image:  cfg.Image,
		Labels: labels,
		State:  "created",
	}
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
		return nil
	}
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
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
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:    c.ID,
			Name:  c.Name,
			Image: c.Image,
			State: &types.ContainerState{Status: c.State, Running: c.State == "running"},
		},
		Config: &container.Config{Labels: c.Labels},
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
			out = append(out, types.Container{
				ID:     c.ID,
				Names:  []string{c.Name},
				Image:  c.Image,
				Labels: c.Labels,
				State:  c.State,
			})
		}
	}
	return out, nil
}

// silence unused imports in the file (helps when the fake grows)
var (
	_ image.PullOptions
	_ volume.Volume
	_ io.Reader
)

// --- Test helpers ------------------------------------------------------------

func sampleSpec(replicaID string, raftIndex uint64) compose.ContainerSpec {
	return compose.ContainerSpec{
		ClusterID:    "cluster-x",
		Deployment:   "sample",
		Service:      "web",
		ReplicaID:    replicaID,
		ReplicaIndex: 0,
		RaftIndex:    raftIndex,
		Image:        "nginx:1.27",
		Labels: map[string]string{
			"jaco.cluster_id":    "cluster-x",
			"jaco.deployment":    "sample",
			"jaco.service":       "web",
			"jaco.replica_id":    replicaID,
			"jaco.replica_index": "0",
			"jaco.raft_index":    fmt.Sprintf("%d", raftIndex),
		},
	}
}

// --- Tests ------------------------------------------------------------------

func TestStart_CreatesContainerWithLabels(t *testing.T) {
	d := newFakeDocker()
	id, err := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatalf("empty container id")
	}
	c := d.containers[id]
	if c == nil {
		t.Fatalf("container not stored")
	}
	if c.State != "running" {
		t.Errorf("state = %q, want running", c.State)
	}
	for _, want := range []string{"jaco.cluster_id", "jaco.deployment", "jaco.service", "jaco.replica_id", "jaco.replica_index", "jaco.raft_index"} {
		if _, ok := c.Labels[want]; !ok {
			t.Errorf("missing label %s", want)
		}
	}
	if c.Name != "jaco_sample-web-0" {
		t.Errorf("name = %q, want jaco_sample-web-0", c.Name)
	}
}

func TestStart_IsNoopWhenReplicaAlreadyMatchesRaftIndex(t *testing.T) {
	d := newFakeDocker()
	id1, err := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("Start returned different ids (%s vs %s) — expected idempotent no-op", id1, id2)
	}
	if got := len(d.containers); got != 1 {
		t.Errorf("container count = %d, want 1 (Start should NOT create a second container)", got)
	}
}

func TestStart_StopRemovesAndRecreatesWhenRaftIndexChanged(t *testing.T) {
	d := newFakeDocker()
	id1, _ := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))

	id2, err := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 43))
	if err != nil {
		t.Fatalf("Start v2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("Start returned same id; expected stop+remove+recreate when raft_index changed")
	}
	if _, exists := d.containers[id1]; exists {
		t.Errorf("old container should have been removed")
	}
	if got := len(d.containers); got != 1 {
		t.Errorf("container count = %d, want 1", got)
	}
}

func TestStop_NoOpWhenContainerMissing(t *testing.T) {
	d := newFakeDocker()
	if err := lifecycle.Stop(context.Background(), d, "ghost", 10); err != nil {
		t.Errorf("Stop on missing replica should be a no-op; got %v", err)
	}
}

func TestStop_TransitionsRunningContainerToExited(t *testing.T) {
	d := newFakeDocker()
	id, _ := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	if err := lifecycle.Stop(context.Background(), d, "sample-web-0", 10); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := d.containers[id].State; got != "exited" {
		t.Errorf("state after Stop = %q, want exited", got)
	}
}

func TestRemove_DeletesContainer(t *testing.T) {
	d := newFakeDocker()
	id, _ := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	if err := lifecycle.Remove(context.Background(), d, "sample-web-0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := d.containers[id]; ok {
		t.Errorf("container still present after Remove")
	}
}

func TestRemove_NoOpWhenMissing(t *testing.T) {
	d := newFakeDocker()
	if err := lifecycle.Remove(context.Background(), d, "ghost"); err != nil {
		t.Errorf("Remove on missing replica should be a no-op; got %v", err)
	}
}

func TestInspect_ReturnsStateAndID(t *testing.T) {
	d := newFakeDocker()
	id, _ := lifecycle.Start(context.Background(), d, sampleSpec("sample-web-0", 42))
	gotID, state, err := lifecycle.Inspect(context.Background(), d, "sample-web-0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if gotID != id {
		t.Errorf("Inspect id = %q, want %q", gotID, id)
	}
	if state != "running" {
		t.Errorf("Inspect state = %q, want running", state)
	}
}

func TestInspect_EmptyResultWhenMissing(t *testing.T) {
	d := newFakeDocker()
	id, state, err := lifecycle.Inspect(context.Background(), d, "ghost")
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if id != "" || state != "" {
		t.Errorf("expected empty id+state for missing replica; got id=%q state=%q", id, state)
	}
}

func TestReconcile_RemovesOrphans(t *testing.T) {
	d := newFakeDocker()
	// Three containers for cluster-x: two with replica_ids the FSM knows
	// about, one orphan.
	for _, spec := range []compose.ContainerSpec{
		sampleSpec("sample-web-0", 42),
		sampleSpec("sample-web-1", 42),
		sampleSpec("sample-ghost-0", 42), // orphan
	} {
		if _, err := lifecycle.Start(context.Background(), d, spec); err != nil {
			t.Fatal(err)
		}
	}
	// And one container from a *different* cluster — must be left alone.
	d.idSeq++
	otherID := fmt.Sprintf("c-%d", d.idSeq)
	d.containers[otherID] = &fakeContainer{
		ID: otherID, Name: "stranger", Image: "alpine",
		Labels: map[string]string{"jaco.cluster_id": "other", "jaco.replica_id": "other-svc-0"},
		State:  "running",
	}

	expected := map[string]bool{"sample-web-0": true, "sample-web-1": true}
	removed, err := lifecycle.Reconcile(context.Background(), d, "cluster-x", expected)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(removed) != 1 || removed[0] != "sample-ghost-0" {
		t.Errorf("removed = %v, want [sample-ghost-0]", removed)
	}
	// Survivors: two desired replicas + the other-cluster container.
	if got := len(d.containers); got != 3 {
		t.Errorf("container count = %d, want 3 (orphan removed; other cluster's container untouched)", got)
	}
	if _, ok := d.containers[otherID]; !ok {
		t.Errorf("other-cluster container should NOT have been removed")
	}
}

func TestReconcile_RequiresClusterID(t *testing.T) {
	d := newFakeDocker()
	_, err := lifecycle.Reconcile(context.Background(), d, "", nil)
	if err == nil {
		t.Errorf("expected error when clusterID is empty")
	}
}

func TestStart_RequiresReplicaIDAndImage(t *testing.T) {
	d := newFakeDocker()
	if _, err := lifecycle.Start(context.Background(), d, compose.ContainerSpec{Image: "x"}); err == nil {
		t.Errorf("expected error on missing replica_id")
	}
	if _, err := lifecycle.Start(context.Background(), d, compose.ContainerSpec{ReplicaID: "x"}); err == nil {
		t.Errorf("expected error on missing image")
	}
}
