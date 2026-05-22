package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newNodes(b *watch.Broker[*pb.Node]) *Store[*pb.Node] {
	return NewStore(b, func(n *pb.Node) string { return n.GetHostname() })
}
