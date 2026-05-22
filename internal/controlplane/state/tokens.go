package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newTokens(b *watch.Broker[*pb.Token]) *Store[*pb.Token] {
	return NewStore(b, func(t *pb.Token) string { return t.GetIdentity() })
}
