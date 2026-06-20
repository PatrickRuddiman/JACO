package storage_test

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestStore_ChallengePublisherTap pins issue #189: Store invokes the
// challenge publisher with the raw value bytes only for CertMagic's
// challenge-token blobs (key contains "/challenge_tokens/"), and leaves
// ordinary cert/key blobs untapped. The daemon wires the hook to republish
// the token through the CA-agnostic raft ChallengeToken path so any node can
// serve the HTTP-01 validation.
func TestStore_ChallengePublisherTap(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s, _ := newHarness(t, "node-a", clock)

	var mu sync.Mutex
	var got [][]byte
	s.SetChallengePublisher(func(value []byte) {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]byte, len(value))
		copy(cp, value)
		got = append(got, cp)
	})

	ctx := context.Background()
	challengeKey := "acme/acme-staging-v02.api.letsencrypt.org-directory/challenge_tokens/web.example.com.json"
	if err := s.Store(ctx, challengeKey, []byte(`{"type":"http-01","token":"tokA"}`)); err != nil {
		t.Fatalf("Store challenge: %v", err)
	}
	// An ordinary cert blob must NOT be tapped.
	if err := s.Store(ctx, "certificates/acme-v02/web.example.com/web.example.com.crt", []byte("PEM")); err != nil {
		t.Fatalf("Store cert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("publisher invoked %d times; want exactly 1 (challenge blob only)", len(got))
	}
	if string(got[0]) != `{"type":"http-01","token":"tokA"}` {
		t.Errorf("publisher value = %q; want the challenge JSON", got[0])
	}
}

// TestStore_NoPublisherIsNoop confirms Store works unchanged when no
// publisher is installed (e.g. ACME disabled or external-caddy mode).
func TestStore_NoPublisherIsNoop(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s, st := newHarness(t, "node-a", clock)

	key := "x/challenge_tokens/web.example.com.json"
	if err := s.Store(context.Background(), key, []byte("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, ok := st.CertBlobs.Get(key); !ok {
		t.Errorf("blob not stored when no publisher set")
	}
}
