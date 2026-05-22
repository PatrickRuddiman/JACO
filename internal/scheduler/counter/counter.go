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
