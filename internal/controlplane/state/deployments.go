package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newDeployments(b *watch.Broker[*pb.Deployment]) *Store[*pb.Deployment] {
	return NewStore(b, func(d *pb.Deployment) string { return d.GetName() })
}
