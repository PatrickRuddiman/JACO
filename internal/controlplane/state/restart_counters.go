package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newRestartCounters(b *watch.Broker[*pb.RestartCounter]) *Store[*pb.RestartCounter] {
	return NewStore(b, func(r *pb.RestartCounter) string { return r.GetReplicaId() })
}
