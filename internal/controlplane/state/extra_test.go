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

