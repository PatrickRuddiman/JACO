package state

import (
	"strconv"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TCPRouteKey is the cluster-global identity of a TCP ingress route: its
// published host port. A node can bind a given port only once, so two
// deployments publishing the same host port collide; keying on the port alone
// makes that collision a single store lookup at admission.
func TCPRouteKey(publishedPort int32) string { return strconv.Itoa(int(publishedPort)) }

func newTCPRoutes(b *watch.Broker[*pb.TCPRoute]) *Store[*pb.TCPRoute] {
	return NewStore(b, func(r *pb.TCPRoute) string { return TCPRouteKey(r.GetPublishedPort()) })
}
