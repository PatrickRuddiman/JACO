package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const replicaCounterKeySep = "\x00"

func newReplicaCounters(b *watch.Broker[*pb.ReplicaCounter]) *Store[*pb.ReplicaCounter] {
	return NewStore(b, func(c *pb.ReplicaCounter) string {
		return c.GetDeployment() + replicaCounterKeySep + c.GetService()
	})
}

// ReplicaCounterKey is the canonical composite-key helper.
func ReplicaCounterKey(deployment, service string) string {
	return deployment + replicaCounterKeySep + service
}
