// Package lifecycle owns idempotent container create / start / stop / remove
// against the docker engine, plus orphan reconcile on daemon startup. The
// runtime watches ReplicaDesired and calls Start; ReplicaCommand{op:restart}
// flows through Stop+Start; Deploy.Delete cascades to Remove via the FSM.
//
// All operations take the narrow dockerx.Docker interface so this package
// unit-tests against an in-memory fake; the build-tag-gated integration test
// (lifecycle_integration_test.go) exercises a real engine.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

const (
	labelClusterID    = "jaco.cluster_id"
	labelDeployment   = "jaco.deployment"
	labelService      = "jaco.service"
	labelReplicaID    = "jaco.replica_id"
	labelReplicaIndex = "jaco.replica_index"
	labelRaftIndex    = "jaco.raft_index"
)

// Start brings spec's container up. Idempotent on the (replica_id, raft_index)
// pair: if a container with the same replica_id already exists and its
// jaco.raft_index label matches spec.RaftIndex, Start is a no-op and returns
// the existing container id. Otherwise an existing container is stopped +
// removed before a fresh one is created.
func Start(ctx context.Context, d dockerx.Docker, spec compose.ContainerSpec) (containerID string, err error) {
	if spec.ReplicaID == "" {
		return "", errors.New("Start: ContainerSpec.ReplicaID is required")
	}
	if spec.Image == "" {
		return "", errors.New("Start: ContainerSpec.Image is required")
	}

	existing, err := findContainerByReplicaID(ctx, d, spec.ReplicaID)
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}
	if existing != nil {
		if matchesRaftIndex(existing.Labels, spec.RaftIndex) {
			// Idempotent path — the cluster's desired state already matches
			// what's running.
			return existing.ID, nil
		}
		if err := stopAndRemove(ctx, d, existing.ID); err != nil {
			return "", fmt.Errorf("stop+remove stale %s: %w", existing.ID, err)
		}
	}

	cfg, hostCfg, netCfg := buildConfig(spec)
	name := compose.ContainerName(compose.SpecOptions{ReplicaID: spec.ReplicaID})
	resp, err := d.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("ContainerCreate: %w", err)
	}
	// Attach the container to each JACO bridge declared on the spec BEFORE
	// starting it. NetworkMode=none was used at create-time so the container
	// has no network until we explicitly NetworkConnect for each declared
	// docker network (task 27's bridges).
	for _, net := range spec.Networks {
		if err := d.NetworkConnect(ctx, net, resp.ID, nil); err != nil {
			return "", fmt.Errorf("NetworkConnect %s -> %s: %w", net, resp.ID, err)
		}
	}
	if err := d.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("ContainerStart %s: %w", resp.ID, err)
	}
	return resp.ID, nil
}

// Stop sends SIGTERM (followed by SIGKILL after gracePeriodSeconds) to the
// container associated with replicaID. No-op when the container isn't found.
func Stop(ctx context.Context, d dockerx.Docker, replicaID string, gracePeriodSeconds int) error {
	c, err := findContainerByReplicaID(ctx, d, replicaID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	timeout := gracePeriodSeconds
	return d.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout})
}

// Remove force-removes the container associated with replicaID. No-op when
// the container isn't found.
func Remove(ctx context.Context, d dockerx.Docker, replicaID string) error {
	c, err := findContainerByReplicaID(ctx, d, replicaID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	return d.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
}

// Inspect returns the container id and Docker's reported state string for the
// container associated with replicaID. Both values are empty when there's no
// matching container.
func Inspect(ctx context.Context, d dockerx.Docker, replicaID string) (containerID, state string, err error) {
	c, err := findContainerByReplicaID(ctx, d, replicaID)
	if err != nil {
		return "", "", err
	}
	if c == nil {
		return "", "", nil
	}
	info, err := d.ContainerInspect(ctx, c.ID)
	if err != nil {
		return c.ID, "", err
	}
	if info.State != nil {
		return c.ID, info.State.Status, nil
	}
	return c.ID, "", nil
}

// findContainerByReplicaID lists containers carrying jaco.replica_id=<id>.
// Returns nil when no container matches.
func findContainerByReplicaID(ctx context.Context, d dockerx.Docker, replicaID string) (*types.Container, error) {
	args := filters.NewArgs()
	args.Add("label", labelReplicaID+"="+replicaID)
	list, err := d.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	c := list[0]
	return &c, nil
}

func matchesRaftIndex(labels map[string]string, want uint64) bool {
	got := labels[labelRaftIndex]
	if got == "" {
		return false
	}
	parsed, err := strconv.ParseUint(got, 10, 64)
	if err != nil {
		return false
	}
	return parsed == want
}

func stopAndRemove(ctx context.Context, d dockerx.Docker, id string) error {
	timeout := 10
	if err := d.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		// Continue to Remove — the container may already be stopped.
	}
	return d.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}
