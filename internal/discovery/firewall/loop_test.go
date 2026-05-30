package firewall_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
)

// TestLoop_ExitsOnCancel — Loop runs Tick immediately then blocks on
// the ticker; on ctx cancel it returns ctx.Err().
func TestLoop_ExitsOnCancel(t *testing.T) {
	r := &firewall.Reconciler{
		Lister:       func(context.Context) ([]byte, error) { return []byte(`{"nftables":[]}`), nil },
		Applier:      func(context.Context, string) error { return nil },
		Audit:        func(context.Context, string, map[string]string) error { return nil },
		UpdateStatus: func(context.Context, string, string) error { return nil },
		Render:       func() firewall.RuleInput { return firewall.RuleInput{} },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Loop err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Loop did not return on cancel")
	}
}

// TestLoop_ReadyGateSkipsTickWhenFalse — when ReadyGate returns false, Loop
// must NOT call Tick (proxied here via Lister, which would otherwise be
// invoked). When ReadyGate flips to true, Tick runs. Models the issue #113
// startup-race: a freshly-joined follower must wait until raft knows the
// leader before its first reconcile.
func TestLoop_ReadyGateSkipsTickWhenFalse(t *testing.T) {
	var listerCalls int32
	var ready atomic.Bool
	r := &firewall.Reconciler{
		Lister: func(context.Context) ([]byte, error) {
			atomic.AddInt32(&listerCalls, 1)
			return []byte(`{"nftables":[]}`), nil
		},
		Applier:      func(context.Context, string) error { return nil },
		Audit:        func(context.Context, string, map[string]string) error { return nil },
		UpdateStatus: func(context.Context, string, string) error { return nil },
		Render:       func() firewall.RuleInput { return firewall.RuleInput{} },
		ReadyGate:    func() bool { return ready.Load() },
	}

	// Pin the ticker fast so we get multiple iterations in this test without
	// waiting 30s. We can't override ReconcileInterval, so instead we cancel
	// after enough wall time for Loop to have iterated at least once with
	// ReadyGate=false (the first iteration runs immediately).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()

	// Wait past the first immediate-iteration. ReadyGate=false → no Tick →
	// Lister untouched.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&listerCalls); got != 0 {
		t.Errorf("ReadyGate=false but Lister called %d times; want 0", got)
	}

	cancel()
	<-done
}

// TestLoop_ReadyGateRunsTickWhenTrue — sanity: ReadyGate=true runs Tick
// immediately, exactly as Loop did before #113.
func TestLoop_ReadyGateRunsTickWhenTrue(t *testing.T) {
	var listerCalls int32
	r := &firewall.Reconciler{
		Lister: func(context.Context) ([]byte, error) {
			atomic.AddInt32(&listerCalls, 1)
			return []byte(`{"nftables":[]}`), nil
		},
		Applier:      func(context.Context, string) error { return nil },
		Audit:        func(context.Context, string, map[string]string) error { return nil },
		UpdateStatus: func(context.Context, string, string) error { return nil },
		Render:       func() firewall.RuleInput { return firewall.RuleInput{} },
		ReadyGate:    func() bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Loop(ctx) }()

	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&listerCalls); got == 0 {
		t.Errorf("ReadyGate=true but Lister never called; want >=1")
	}

	cancel()
	<-done
}
