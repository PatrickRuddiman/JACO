package state

import (
	"bytes"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newTokens(b *watch.Broker[*pb.Token]) *Store[*pb.Token] {
	return NewStore(b, func(t *pb.Token) string { return t.GetIdentity() })
}

// LookupTokenByHash returns the Token whose hashed_secret matches hash, if
// any. Linear scan over the in-memory map; token counts are O(operators) so
// this is cheap in practice. The admission interceptor calls this on every
// authenticated RPC.
func LookupTokenByHash(s *Store[*pb.Token], hash []byte) (*pb.Token, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.byKey {
		if bytes.Equal(t.GetHashedSecret(), hash) {
			return clone(t), true
		}
	}
	return nil, false
}
