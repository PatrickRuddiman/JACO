package grpc

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// purgeHostlessSubnets removes every pre-#28 Subnet entry that has no host —
// the old per-(deployment, network) allocations from before per-host /24s.
// Reconcilers re-allocate the per-host subnets lazily afterward. Returns the
// number of host-less entries freed. apply is the leader's raft.Apply; the
// caller must already be the leader.
func purgeHostlessSubnets(st *state.State, apply func([]byte) error) (int, error) {
	purged := 0
	for _, sn := range st.Subnets.List() {
		if sn.GetHost() != "" {
			continue
		}
		cmd := &pb.Command{
			Identity: "migration",
			Ts:       timestamppb.Now(),
			Payload: &pb.Command_SubnetFree{SubnetFree: &pb.SubnetFree{
				Deployment: sn.GetDeployment(),
				Network:    sn.GetNetwork(),
				// Host left empty: this is a pre-#28 host-less entry.
			}},
		}
		data, err := proto.Marshal(cmd)
		if err != nil {
			return purged, err
		}
		if err := apply(data); err != nil {
			return purged, err
		}
		purged++
	}
	return purged, nil
}
