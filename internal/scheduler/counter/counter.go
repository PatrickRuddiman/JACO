// Package counter owns the per-service monotonic replica-index counter that
// guarantees replica ids are never reused across delete+recreate of a
// service. The next-index value lives in raft (state.ReplicaCounters) so it
// survives crashes + leader failover.
package counter

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Applier wraps the raft Apply call. Returns once the FSM has applied the
// command on the leader.
type Applier func(cmd []byte) error

// Counter generates monotonic replica indices per (deployment, service).
// Single-writer: the scheduler runs on the raft leader only, so reads and
// writes against ReplicaCounters can't race with another scheduler.
type Counter struct {
	state *state.State
	apply Applier
}

// New constructs a Counter wired to the state store + raft applier.
func New(s *state.State, apply Applier) *Counter {
	return &Counter{state: s, apply: apply}
}

// Next returns the next replica index for (deployment, service) and
// raft-Applies a ReplicaCounterIncrement command so the new value is
// durable. Returned indices start at 1 and never repeat — even after the
// service is deleted and recreated under the same name.
func (c *Counter) Next(deployment, service string) (uint64, error) {
	if deployment == "" || service == "" {
		return 0, fmt.Errorf("Counter.Next: deployment and service are required")
	}

	var current uint64
	if rc, ok := c.state.ReplicaCounters.Get(state.ReplicaCounterKey(deployment, service)); ok {
		current = rc.GetNextIndex()
	}
	next := current + 1

	cmd := &pb.Command{
		Identity: "scheduler",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_ReplicaCounterIncrement{
			ReplicaCounterIncrement: &pb.ReplicaCounterIncrement{
				Deployment: deployment,
				Service:    service,
			},
		},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("marshal ReplicaCounterIncrement: %w", err)
	}
	if err := c.apply(data); err != nil {
		return 0, fmt.Errorf("raft apply: %w", err)
	}
	return next, nil
}

// ReplicaID composes the per-replica id in the canonical format used by
// scheduler / runtime / discovery / ingress: `<deployment>-<service>-<index>`.
func ReplicaID(deployment, service string, index uint64) string {
	return fmt.Sprintf("%s-%s-%d", deployment, service, index)
}

// ReplicaIDForHost composes the per-replica id for a GLOBAL (daemonset)
// service, which runs exactly one replica per node. The id is keyed by host
// (not a positional index) so a node's replica keeps a stable id across
// reconciles regardless of membership churn: when another node leaves, the
// surviving nodes' ids are unchanged, so their containers are not needlessly
// torn down and recreated. The hostname is sanitized to [a-zA-Z0-9_.-] so the
// id stays a valid container/DNS label component.
func ReplicaIDForHost(deployment, service, host string) string {
	return fmt.Sprintf("%s-%s-%s", deployment, service, sanitizeHost(host))
}

// sanitizeHost replaces any character outside [a-zA-Z0-9_.-] with '-' so a
// hostname can be embedded in a replica id without breaking container names
// or DNS labels.
func sanitizeHost(host string) string {
	b := make([]rune, 0, len(host))
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b = append(b, r)
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}
