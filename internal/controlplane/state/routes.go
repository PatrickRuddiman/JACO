package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// RouteKey is the (domain, path) identity of a route — the same uniqueness key
// the apply validator enforces. Routes MUST key on both: path-based routing
// (#34) puts several routes on one domain (e.g. "/api" → api and the catch-all
// → web), so keying on domain alone makes them collide — the last one wins and
// silently drops every other path route.
func RouteKey(domain, path string) string { return domain + "\x00" + path }

func newRoutes(b *watch.Broker[*pb.Route]) *Store[*pb.Route] {
	return NewStore(b, func(r *pb.Route) string { return RouteKey(r.GetDomain(), r.GetPath()) })
}
