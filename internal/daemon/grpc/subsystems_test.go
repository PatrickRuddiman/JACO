package grpc_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
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

// TestSubsystems_RuntimeReconcilerCreatesContainerEndToEnd plumbs an
// in-memory fake Docker into dgrpc.Options, calls Init, raft-applies a
// DeploymentApply, and asserts a container with the right replica_id
// label appears in the fake. Proves scheduler → reconciler → lifecycle
// wires end-to-end through the daemon.
func TestSubsystems_RuntimeReconcilerCreatesContainerEndToEnd(t *testing.T) {
	d := newFakeDocker()
	sock := filepath.Join(t.TempDir(), "jacod.sock")
	s, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: sock,
		DataDir:        t.TempDir(),
		Hostname:       "test-host",
		ClusterAddr:    freePort(t),
		Docker:         d,
	})
	if err != nil {
		t.Fatalf("dgrpc.New: %v", err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		s.Stop(ctx)
	})

	conn, err := grpc.NewClient(
		"unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", strings.TrimPrefix(addr, "unix://"))
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := pb.NewClusterClient(conn).Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Wait for leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cmd := &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_DeploymentApply{
			DeploymentApply: &pb.DeploymentApply{
				Deployment:  "smoke",
				Revision:    1,
				ComposeYaml: []byte(subsystemsTestCompose),
				Services: []*pb.ServiceSpec{{
					Name: "web", Replicas: 1, ComposeService: "web",
					Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
				}},
			},
		},
	}
	data, _ := proto.Marshal(cmd)
	if _, err := s.Raft().Apply(data, 2*time.Second); err != nil {
		t.Fatalf("raft.Apply DeploymentApply: %v", err)
	}

	// Poll the fake for a container labeled with our replica id.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.hasReplica("smoke-web-0") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("runtime reconciler never created a container for smoke-web-0")
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

// --- Fake docker for the runtime end-to-end test ---------------------------

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

func (f *fakeDocker) hasReplica(rid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Labels["jaco.replica_id"] == rid {
			return true
		}
	}
	return false
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
