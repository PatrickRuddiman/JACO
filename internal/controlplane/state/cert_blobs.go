package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newCertBlobs(b *watch.Broker[*pb.CertBlob]) *Store[*pb.CertBlob] {
	return NewStore(b, func(c *pb.CertBlob) string { return c.GetKey() })
}
