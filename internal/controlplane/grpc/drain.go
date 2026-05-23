package grpcsrv

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/drain"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// drainHost runs the graceful drain step machine for hostname:
//  1. Plan migrations via drain.Plan (state.ReplicasDesired + state.Nodes).
//  2. Apply a ReplicaDesiredUpsert for each migration so the runtime on
//     the remaining hosts starts the replacement containers.
//  3. Poll state.ReplicasObserved until every migrated replica is
//     RUNNING on its new host, with a 60s deadline.
//
// Returns a typed status error on plan / apply / timeout failures.
//
// Note: drain.Plan refuses when a service has nowhere to go (e.g. a
// hosts-pinned service whose only eligible host is being drained); that
// error surfaces as FailedPrecondition + drain_no_placement.
func (c *clusterServer) drainHost(ctx context.Context, hostname string) error {
	migrations, err := drain.Plan(c.state, hostname)
	if err != nil {
		return errorStatus(codes.FailedPrecondition, "drain_no_placement", err.Error())
	}
	if len(migrations) == 0 {
		return nil
	}

	now := timestamppb.Now()
	for _, m := range migrations {
		cmd := &pb.Command{
			Identity: "drain:" + hostname,
			Ts:       now,
			Payload: &pb.Command_ReplicaDesiredUpsert{
				ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
					Replica: &pb.ReplicaDesired{
						Id:         m.ReplicaID,
						Deployment: m.Deployment,
						Service:    m.Service,
						Host:       m.ToHost,
						Image:      m.Image,
					},
				},
			},
		}
		if err := c.applyCommand(cmd); err != nil {
			return errorStatus(codes.Internal, "drain_apply_failed", err.Error())
		}
	}

	// Poll until every migrated replica reports RUNNING on its new host
	// (or the deadline expires).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if drainComplete(c.state, migrations) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errorStatus(codes.DeadlineExceeded, "drain_cancelled", ctx.Err().Error())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errorStatus(codes.FailedPrecondition, "drain_timeout",
		"drain did not complete within 60s; retry or pass --force to skip the drain step")
}

// drainComplete returns true when every migrated replica is reported
// RUNNING in state.ReplicasObserved. The reconciler on the destination
// host writes the observation through Internal.Submit / raft.Apply once
// the container is healthy.
func drainComplete(st *state.State, migrations []drain.Migration) bool {
	for _, m := range migrations {
		obs, ok := st.ReplicasObserved.Get(m.ReplicaID)
		if !ok {
			return false
		}
		if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
			return false
		}
	}
	return true
}
