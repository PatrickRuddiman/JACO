package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newRoutes(b *watch.Broker[*pb.Route]) *Store[*pb.Route] {
	return NewStore(b, func(r *pb.Route) string { return r.GetDomain() })
}
