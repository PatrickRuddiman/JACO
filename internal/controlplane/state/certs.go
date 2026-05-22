package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newCerts(b *watch.Broker[*pb.Cert]) *Store[*pb.Cert] {
	return NewStore(b, func(c *pb.Cert) string { return c.GetDomain() })
}
