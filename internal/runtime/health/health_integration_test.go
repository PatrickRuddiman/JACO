//go:build docker

package health_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestIntegration_WatcherSubmitsRunningObservation starts a real
// container without a healthcheck, runs the Watcher for ~1s, asserts
// the SubmitFn observed at least one RUNNING ReplicaObserved.
// Skipped when JACO_INTEGRATION_DOCKER is unset.
func TestIntegration_WatcherSubmitsRunningObservation(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_DOCKER") == "" {
		t.Skip("set JACO_INTEGRATION_DOCKER=1 to enable")
	}
	d, err := dockerx.New("")
	if err != nil {
		t.Skipf("docker unreachable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := d.ContainerCreate(ctx,
		&container.Config{
			Image:  "busybox",
			Cmd:    []string{"sleep", "30"},
			Labels: map[string]string{"jaco.replica_id": "health-int-1"},
		},
		&container.HostConfig{}, &network.NetworkingConfig{}, &ocispec.Platform{},
		"jaco-health-int")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = d.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	if err := d.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	var running atomic.Bool
	submit := func(_ context.Context, obs *pb.ReplicaObserved) error {
		if obs.GetState() == pb.ReplicaState_REPLICA_STATE_RUNNING {
			running.Store(true)
		}
		return nil
	}
	w := health.NewWatcher(d, submit, nil)
	t.Cleanup(w.StopAll)

	w.Start(ctx, "health-int-1", resp.ID, false /*no healthcheck*/)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if running.Load() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("no RUNNING observation within 10s")
}
