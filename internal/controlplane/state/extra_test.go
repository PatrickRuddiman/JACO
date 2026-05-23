package state_test

import (
	"bytes"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestLookupTokenByHash_HappyPath — a token stored under identity X
// is retrievable by its hashed_secret.
func TestLookupTokenByHash_HappyPath(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Tokens.Apply(&pb.Token{Identity: "alice", HashedSecret: []byte{0xab, 0xcd}}, 1)

	got, ok := state.LookupTokenByHash(st.Tokens, []byte{0xab, 0xcd})
	if !ok {
		t.Fatalf("LookupTokenByHash miss; want hit")
	}
	if got.GetIdentity() != "alice" {
		t.Errorf("identity = %q, want alice", got.GetIdentity())
	}
	// Defensive copy: mutating the returned token must not affect state.
	got.HashedSecret = []byte{0xff}
	again, _ := state.LookupTokenByHash(st.Tokens, []byte{0xab, 0xcd})
	if !bytes.Equal(again.GetHashedSecret(), []byte{0xab, 0xcd}) {
		t.Errorf("defensive copy violated: stored token mutated")
	}
}

// TestLookupTokenByHash_Miss — unknown hash returns nil, false.
func TestLookupTokenByHash_Miss(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Tokens.Apply(&pb.Token{Identity: "alice", HashedSecret: []byte{0x01}}, 1)
	got, ok := state.LookupTokenByHash(st.Tokens, []byte{0x99})
	if ok {
		t.Errorf("LookupTokenByHash unknown hash returned ok; got %+v", got)
	}
}

// TestReplicaCounterKey — deterministic key format used by the FSM
// and the scheduler to look up per-(deployment, service) replica
// counters.
func TestReplicaCounterKey(t *testing.T) {
	if got := state.ReplicaCounterKey("d", "s"); got != "d\x00s" {
		t.Errorf("ReplicaCounterKey = %q, want d\x00s", got)
	}
	// Bracket each side with empty
	if got := state.ReplicaCounterKey("", "s"); got != "\x00s" {
		t.Errorf("empty deployment = %q", got)
	}
	if got := state.ReplicaCounterKey("d", ""); got != "d\x00" {
		t.Errorf("empty service = %q", got)
	}
}

// TestRolloutPlanKey — same shape; ensures keys match what the
// scheduler reads via state.RolloutPlans.Get.
func TestRolloutPlanKey(t *testing.T) {
	if got := state.RolloutPlanKey("d", "s"); got != "d\x00s" {
		t.Errorf("RolloutPlanKey = %q, want d\\x00s", got)
	}
}
