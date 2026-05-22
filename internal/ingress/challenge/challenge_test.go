package challenge_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{now: t} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newHarness(t *testing.T, clock challenge.Clock) (*challenge.Issuer, *challenge.Handler, *state.State) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	return challenge.NewIssuer(apply, clock), challenge.NewHandler(st, clock), st
}

func TestIssue_PersistsChallengeToken(t *testing.T) {
	clock := newClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	i, _, st := newHarness(t, clock)

	if err := i.Issue(context.Background(), "example.com", "token-xyz", "keyauth-xyz"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ct, ok := st.ChallengeTokens.Get("token-xyz")
	if !ok {
		t.Fatalf("ChallengeToken not persisted")
	}
	if ct.GetDomain() != "example.com" || ct.GetKeyAuth() != "keyauth-xyz" {
		t.Errorf("token shape wrong: %+v", ct)
	}
	if got := ct.GetExpiresAt().AsTime(); !got.Equal(clock.Now().Add(challenge.TokenTTL)) {
		t.Errorf("expires_at = %v, want now+TokenTTL", got)
	}
}

func TestIssue_RejectsEmptyArgs(t *testing.T) {
	i, _, _ := newHarness(t, newClock(time.Now()))
	cases := []struct{ domain, token, keyAuth string }{
		{"", "t", "k"},
		{"d", "", "k"},
		{"d", "t", ""},
	}
	for _, c := range cases {
		if err := i.Issue(context.Background(), c.domain, c.token, c.keyAuth); err == nil {
			t.Errorf("Issue(%+v) accepted; want error", c)
		}
	}
}

func TestHandler_ServesKeyAuthForKnownToken(t *testing.T) {
	clock := newClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	i, h, _ := newHarness(t, clock)
	_ = i.Issue(context.Background(), "example.com", "tok1", "keyauth-1")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+challenge.ChallengePath+"tok1", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	if string(body) != "keyauth-1" {
		t.Errorf("body = %q, want keyauth-1", string(body))
	}
}

func TestHandler_UnknownTokenReturns404(t *testing.T) {
	_, h, _ := newHarness(t, newClock(time.Now()))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+challenge.ChallengePath+"missing", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandler_ExpiredTokenReturns404(t *testing.T) {
	clock := newClock(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	i, h, _ := newHarness(t, clock)
	_ = i.Issue(context.Background(), "example.com", "tok1", "keyauth-1")
	// Fast-forward past TokenTTL.
	clock.Advance(challenge.TokenTTL + time.Second)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+challenge.ChallengePath+"tok1", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expired token status = %d, want 404", w.Code)
	}
}

func TestHandler_NonChallengePathReturns404(t *testing.T) {
	_, h, _ := newHarness(t, newClock(time.Now()))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/other/path", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandler_TokenWithSlashRejected(t *testing.T) {
	clock := newClock(time.Now())
	i, h, _ := newHarness(t, clock)
	_ = i.Issue(context.Background(), "example.com", "tok1", "keyauth-1")
	w := httptest.NewRecorder()
	// Path traversal attempt — must not match any token.
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+challenge.ChallengePath+"tok1/extra", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for slashed token", w.Code)
	}
}

func TestHandler_ConcurrentReadsSafe(t *testing.T) {
	// Quick race-detector sweep — multiple readers + a writer should not
	// trip the race flag.
	clock := newClock(time.Now())
	i, h, _ := newHarness(t, clock)
	_ = i.Issue(context.Background(), "example.com", "tok1", "keyauth-1")

	done := make(chan struct{})
	for j := 0; j < 8; j++ {
		go func() {
			for k := 0; k < 100; k++ {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "http://example.com"+challenge.ChallengePath+"tok1", nil)
				h.ServeHTTP(w, req)
			}
			done <- struct{}{}
		}()
	}
	for j := 0; j < 8; j++ {
		<-done
	}
}
