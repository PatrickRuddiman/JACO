package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// newRegistryCredentials constructs the in-memory Store backing
// state.RegistryCredentials. Keyed by RegistryCredential.registry — callers
// MUST canonicalize the host before Apply (e.g. via runtime/pull.CanonicalHost
// which normalizes Docker Hub variants to "docker.io"); the FSM is the only
// production writer and does that canonicalization before applying.
//
// The credential is held in the same in-memory shape on every node. At-rest
// persistence is whatever the raft snapshot encodes — see fsm.Snapshot and
// docs/concepts/auth-and-tokens.md for the at-rest posture.
func newRegistryCredentials(b *watch.Broker[*pb.RegistryCredential]) *Store[*pb.RegistryCredential] {
	return NewStore(b, func(c *pb.RegistryCredential) string { return c.GetRegistry() })
}
