//go:build docker

package lifecycle_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
)

// TestIntegration_StartStopRemoveAgainstRealDocker drives lifecycle.Start
// against the real docker daemon. Skipped when JACO_INTEGRATION_DOCKER
// is unset or the dockerx client can't connect. Cleans up on teardown
// even when the test fails partway.
func TestIntegration_StartStopRemoveAgainstRealDocker(t *testing.T) {
	if os.Getenv("JACO_INTEGRATION_DOCKER") == "" {
		t.Skip("set JACO_INTEGRATION_DOCKER=1 to enable")
	}
	d, err := dockerx.New("")
	if err != nil {
		t.Skipf("docker unreachable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := compose.ContainerSpec{
		ClusterID:    "integration-cluster",
		Deployment:   "integration",
		Service:      "web",
		ReplicaID:    "integration-web-0",
		ReplicaIndex: 0,
		RaftIndex:    1,
		Image:        "nginx:alpine",
		Labels: map[string]string{
			"jaco.cluster_id":    "integration-cluster",
			"jaco.deployment":    "integration",
			"jaco.service":       "web",
			"jaco.replica_id":    "integration-web-0",
			"jaco.replica_index": "0",
			"jaco.raft_index":    "1",
		},
	}

	t.Cleanup(func() {
		_ = lifecycle.Remove(context.Background(), d, spec.ReplicaID)
	})

	id, err := lifecycle.Start(ctx, d, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatalf("empty container id")
	}

	// Wait for the container to reach RUNNING via Inspect.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, state, err := lifecycle.Inspect(ctx, d, spec.ReplicaID)
		if err == nil && state == "running" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := lifecycle.Stop(ctx, d, spec.ReplicaID, 5); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := lifecycle.Remove(ctx, d, spec.ReplicaID); err != nil {
		t.Errorf("Remove: %v", err)
	}

	// Container should be gone now.
	gotID, _, _ := lifecycle.Inspect(ctx, d, spec.ReplicaID)
	if gotID != "" {
		t.Errorf("container still present after Remove: %s", gotID)
	}
}
