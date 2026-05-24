package firewall_test

import (
	"context"
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
