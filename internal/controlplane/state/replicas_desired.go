package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newReplicasDesired(b *watch.Broker[*pb.ReplicaDesired]) *Store[*pb.ReplicaDesired] {
	return NewStore(b, func(r *pb.ReplicaDesired) string { return r.GetId() })
}
