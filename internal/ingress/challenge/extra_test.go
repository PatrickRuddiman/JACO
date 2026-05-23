package challenge_test

import (
	"errors"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
)

// TestSystemClock_NowIsBracketed — SystemClock.Now returns a time
// between two real time.Now()s.
func TestSystemClock_NowIsBracketed(t *testing.T) {
	c := challenge.SystemClock()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after.Add(time.Second)) {
		t.Errorf("Now = %v, not between %v..%v", got, before, after)
	}
}

// TestNewIssuer_DefaultsClockToSystemWhenNil — clock=nil falls
// through to SystemClock.
func TestNewIssuer_DefaultsClockToSystemWhenNil(t *testing.T) {
	i := challenge.NewIssuer(func([]byte) error { return nil }, nil)
	if i == nil {
		t.Errorf("NewIssuer returned nil")
	}
}

// TestNewHandler_DefaultsClockToSystemWhenNil — same for Handler.
func TestNewHandler_DefaultsClockToSystemWhenNil(t *testing.T) {
	h := challenge.NewHandler(nil, nil)
	if h == nil {
		t.Errorf("NewHandler returned nil")
	}
}

// TestIssue_ApplyErrorEmitsCertificateFailedAudit — when the underlying
// apply fails, Issue still emits the CERTIFICATE_FAILED audit (best-
// effort) and surfaces the apply error.
func TestIssue_ApplyErrorEmitsCertificateFailedAudit(t *testing.T) {
	var applied int
	apply := func([]byte) error {
		applied++
		if applied == 1 {
			return errors.New("raft unavailable")
		}
		// Second call is the audit emit — let it succeed.
		return nil
	}
	i := challenge.NewIssuer(apply, nil)
	err := i.Issue(nil, "example.com", "tok-a", "key-a")
	if err == nil {
		t.Fatalf("Issue returned nil err despite failed apply")
	}
	if applied < 2 {
		t.Errorf("apply called %d times, want 2 (token store + audit)", applied)
	}
}
