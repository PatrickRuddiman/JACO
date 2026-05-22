package state

import (
	"encoding/hex"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// JoinTokens are keyed by the hex-encoded hash of the single-use secret. Hex
// because the raw bytes aren't safe to interpret as a UTF-8 map key in dumps /
// logs, and the lookup path (server-side validation) already hashes the
// presented token before comparing.
func newJoinTokens(b *watch.Broker[*pb.JoinToken]) *Store[*pb.JoinToken] {
	return NewStore(b, func(t *pb.JoinToken) string {
		return hex.EncodeToString(t.GetHashedSecret())
	})
}
