package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newChallengeTokens(b *watch.Broker[*pb.ChallengeToken]) *Store[*pb.ChallengeToken] {
	return NewStore(b, func(c *pb.ChallengeToken) string { return c.GetToken() })
}
