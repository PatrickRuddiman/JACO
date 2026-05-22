package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newReplicasObserved(b *watch.Broker[*pb.ReplicaObserved]) *Store[*pb.ReplicaObserved] {
	return NewStore(b, func(r *pb.ReplicaObserved) string { return r.GetId() })
}
