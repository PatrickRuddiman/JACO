package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const rolloutKeySep = "\x00"

func newRolloutPlans(b *watch.Broker[*pb.RolloutPlan]) *Store[*pb.RolloutPlan] {
	return NewStore(b, func(r *pb.RolloutPlan) string {
		return r.GetDeployment() + rolloutKeySep + r.GetService()
	})
}

// RolloutPlanKey is the canonical composite-key helper.
func RolloutPlanKey(deployment, service string) string {
	return deployment + rolloutKeySep + service
}
