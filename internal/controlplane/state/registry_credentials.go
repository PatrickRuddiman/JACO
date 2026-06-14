package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// newRegistryCredentials constructs the in-memory Store backing
// state.RegistryCredentials. Keyed by RegistryCredential.registry — the
// canonical "host[:port]" or, for namespace-scoped credentials,
// "host[:port]/namespace" (e.g. "ghcr.io/owner"). Callers MUST canonicalize
// the key before Apply (the FSM does this via canonicalRegistryKey, which
// matches the reconciler's pull.CanonicalRepo + pull.MatchCredentialKey
// resolver); the FSM is the only production writer.
//
// The credential is held in the same in-memory shape on every node. At-rest
// persistence is whatever the raft snapshot encodes — see fsm.Snapshot and
// docs/concepts/auth-and-tokens.md for the at-rest posture.
func newRegistryCredentials(b *watch.Broker[*pb.RegistryCredential]) *Store[*pb.RegistryCredential] {
	return NewStore(b, func(c *pb.RegistryCredential) string { return c.GetRegistry() })
}
