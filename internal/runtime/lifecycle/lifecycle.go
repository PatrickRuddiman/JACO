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
	"github.com/docker/docker/api/types/network"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/pull"
)

const (
	labelClusterID    = "jaco.cluster_id"
	labelDeployment   = "jaco.deployment"
	labelService      = "jaco.service"
	labelReplicaID    = "jaco.replica_id"
	labelReplicaIndex = "jaco.replica_index"
	labelRaftIndex    = "jaco.raft_index"
)

// IsolationGate is the optional state + self-hostname + pull-auth bag the
// daemon passes to Start. Zero value disables every gate — existing test
// paths with an in-memory fake state work untouched.
//
// State + SelfHostname populate the isolation gate (refuse to bring up a
// container on an isolation_unavailable node). AuthResolver injects the
// replicated-credential lookup into the image-pull path; nil resolves to an
// anonymous pull (the production reconciler wires the cluster-state lookup
// closure, tests can pass an inline fn).
type IsolationGate struct {
	State        *state.State
	SelfHostname string
	AuthResolver pull.AuthResolver

	// NetworkModeResolver, when non-nil, resolves a `network_mode:
	// service:<name>` reference to the currently-running primary replica
	// container id within the same deployment (issue #121). Nil
	// disables the lookup — any `service:<name>` spec then fails with
	// ErrNetworkModeTargetNotReady, which the reconciler treats as a
	// retry-able transient. Production callers wire this from
	// state.ReplicasObserved + state.ReplicasDesired; tests pass an
	// inline fn (or nil for `none` / no network_mode).
	NetworkModeResolver NetworkModeResolver

}

// Start brings spec's container up. Idempotent on the (replica_id, raft_index)
// pair: if a container with the same replica_id already exists and its
// jaco.raft_index label matches spec.RaftIndex, Start is a no-op and returns
// the existing container id. Otherwise an existing container is stopped +
// removed before a fresh one is created.
//
// Isolation gate: when gate.State + gate.SelfHostname are populated, Start
// calls CheckIsolationAvailable up-front and refuses with
// ErrIsolationUnavailable when the local node is in
// NODE_STATUS_ISOLATION_UNAVAILABLE. The daemon's runtime reconciler is
// the production caller; existing test paths pass the zero-value gate and
// bypass the check.
func Start(ctx context.Context, d dockerx.Docker, spec compose.ContainerSpec, gate ...IsolationGate) (containerID string, err error) {
	return StartWithPullState(ctx, d, spec, nil, gate...)
}

// StartWithPullState is Start with an image-pull state callback. onPull (may be
// nil) receives every pull transition (pulling / failed-then-retrying / done)
// so the caller can surface pull progress and failures — e.g. the runtime
// reconciler logs them and submits a ReplicaObserved so a stuck pull shows up
// in `jaco status` instead of failing silently.
func StartWithPullState(ctx context.Context, d dockerx.Docker, spec compose.ContainerSpec, onPull pull.StateFn, gate ...IsolationGate) (containerID string, err error) {
	if spec.ReplicaID == "" {
		return "", errors.New("Start: ContainerSpec.ReplicaID is required")
	}
	if spec.Image == "" {
		return "", errors.New("Start: ContainerSpec.Image is required")
	}
	if len(gate) > 0 && gate[0].State != nil {
		if err := CheckIsolationAvailable(gate[0].State, gate[0].SelfHostname); err != nil {
			return "", err
		}
	}

	existing, err := findContainerByReplicaID(ctx, d, spec.ReplicaID)
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}
	if existing != nil {
		if matchesRaftIndex(existing.Labels, spec.RaftIndex) {
			// Bug 015: label-match is necessary but not sufficient — the
			// container might be in "exited" after a host reboot. Probe
			// running state and ContainerStart it back when needed.
			if existing.State == "running" {
				return existing.ID, nil
			}
			if err := d.ContainerStart(ctx, existing.ID, container.StartOptions{}); err != nil {
				// Couldn't restart — fall through to full re-create.
				if rmErr := stopAndRemove(ctx, d, existing.ID); rmErr != nil {
					return "", fmt.Errorf("stop+remove stale (after start failure %v): %w", err, rmErr)
				}
			} else {
				return existing.ID, nil
			}
		} else {
			if err := stopAndRemove(ctx, d, existing.ID); err != nil {
				return "", fmt.Errorf("stop+remove stale %s: %w", existing.ID, err)
			}
		}
	}

	// Bug 006: pull the image before ContainerCreate. Fresh hosts that
	// haven't cached the image error with "No such image" on Create
	// otherwise. pull.Pull retries with exponential backoff so transient
	// registry failures don't abort the reconcile.
	//
	// Issue #120: `pull_policy: never` short-circuits the pull path so
	// air-gapped deployments can rely on side-loaded images. Any other
	// policy (always / missing / build / unset) flows through the
	// existing pull. When the image is absent under `never`, the
	// subsequent ContainerCreate surfaces docker's typed
	// "No such image" error untouched — exactly the signal the operator
	// needs to know which replica's image was forgotten.
	if pull.ShouldPull(pull.Policy(spec.PullPolicy)) {
		var resolver pull.AuthResolver
		if len(gate) > 0 {
			resolver = gate[0].AuthResolver
		}
		if err := pull.Pull(ctx, d, spec.Image, resolver, nil, onPull); err != nil {
			return "", fmt.Errorf("ImagePull %s: %w", spec.Image, err)
		}
	}

	var netResolver NetworkModeResolver
	if len(gate) > 0 {
		netResolver = gate[0].NetworkModeResolver
	}
	cfg, hostCfg, netCfg, err := buildConfig(spec, netResolver)
	if err != nil {
		return "", fmt.Errorf("buildConfig: %w", err)
	}
	name := compose.ContainerName(compose.SpecOptions{ReplicaID: spec.ReplicaID})
	resp, err := d.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("ContainerCreate: %w", err)
	}
	// Attach the container to each JACO bridge declared on the spec
	// BEFORE starting it. Bug 010: spec.Networks[0] is already attached
	// at create-time via NetworkingConfig.EndpointsConfig, so we skip it
	// here and NetworkConnect any additional networks.
	//
	// Issue #121: when network_mode is set the container shares another
	// container's netns (or has none); attaching to per-deployment
	// bridges is mutually exclusive at the docker level, so skip the
	// loop entirely.
	if spec.NetworkMode == "" {
		for i, net := range spec.Networks {
			if i == 0 {
				continue
			}
			if err := d.NetworkConnect(ctx, net, resp.ID, &network.EndpointSettings{Aliases: serviceAliases(spec)}); err != nil {
				return "", fmt.Errorf("NetworkConnect %s -> %s: %w", net, resp.ID, err)
			}
		}
	}
	if err := d.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("ContainerStart %s: %w", resp.ID, err)
	}
	return resp.ID, nil
}

// Stop sends the container's configured StopSignal (compose `stop_signal`,
// docker default SIGTERM) and waits up to gracePeriodSeconds before SIGKILL.
// gracePeriodSeconds<0 defers to the container's persisted Config.StopTimeout
// (compose `stop_grace_period`) — used by the runtime reconciler so each
// service honors its own grace period without the reconciler tracking specs.
// No-op when the container isn't found.
func Stop(ctx context.Context, d dockerx.Docker, replicaID string, gracePeriodSeconds int) error {
	c, err := findContainerByReplicaID(ctx, d, replicaID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	opts := container.StopOptions{}
	if gracePeriodSeconds >= 0 {
		t := gracePeriodSeconds
		opts.Timeout = &t
	}
	return d.ContainerStop(ctx, c.ID, opts)
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
