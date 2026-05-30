package grpc

import (
	"context"
	"errors"
	"testing"

	hraft "github.com/hashicorp/raft"
)

// Bug #88: the firewall reconciler's Audit + UpdateStatus callbacks used
// to call node.Apply directly. On a follower that returns
// hraft.ErrNotLeader → the audit emit failed → the firewall Tick
// returned the error → every genuine reconcile was logged as
// "Audit(...) failed" + "firewall.Reconciler.Tick failed" even though
// the nft apply itself succeeded. The fix routes those raft writes
// through applyOrForwardCommand, which falls back to forwarding via
// Internal.Submit when the local node is not the leader.

func TestApplyOrForwardCommand_LeaderPath(t *testing.T) {
	localCalled, forwardCalled := 0, 0
	err := applyOrForwardCommand(context.Background(), []byte("cmd"),
		func(b []byte) (uint64, error) { localCalled++; return 42, nil },
		func(ctx context.Context, b []byte) error { forwardCalled++; return nil },
	)
	if err != nil {
		t.Fatalf("leader path: unexpected err: %v", err)
	}
	if localCalled != 1 {
		t.Errorf("local Apply calls = %d, want 1", localCalled)
	}
	if forwardCalled != 0 {
		t.Errorf("forward called %d times on leader path; want 0", forwardCalled)
	}
}

func TestApplyOrForwardCommand_FollowerForwards(t *testing.T) {
	var forwardedPayload []byte
	err := applyOrForwardCommand(context.Background(), []byte("cmd"),
		func(b []byte) (uint64, error) { return 0, hraft.ErrNotLeader },
		func(ctx context.Context, b []byte) error { forwardedPayload = b; return nil },
	)
	if err != nil {
		t.Fatalf("follower path: unexpected err: %v", err)
	}
	if string(forwardedPayload) != "cmd" {
		t.Errorf("forward got payload %q, want %q", forwardedPayload, "cmd")
	}
}

func TestApplyOrForwardCommand_PropagatesNonLeaderApplyError(t *testing.T) {
	boom := errors.New("raft on fire")
	forwardCalled := false
	err := applyOrForwardCommand(context.Background(), []byte("cmd"),
		func(b []byte) (uint64, error) { return 0, boom },
		func(ctx context.Context, b []byte) error { forwardCalled = true; return nil },
	)
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom err, got %v", err)
	}
	if forwardCalled {
		t.Error("forward must NOT be called when local Apply returns a non-ErrNotLeader error")
	}
}

func TestApplyOrForwardCommand_PropagatesForwardError(t *testing.T) {
	boom := errors.New("leader unreachable")
	err := applyOrForwardCommand(context.Background(), []byte("cmd"),
		func(b []byte) (uint64, error) { return 0, hraft.ErrNotLeader },
		func(ctx context.Context, b []byte) error { return boom },
	)
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom err, got %v", err)
	}
}

func TestDialAndSubmit_NoLeaderAddress(t *testing.T) {
	err := dialAndSubmit(context.Background(), "", []byte("cmd"))
	if err == nil || err.Error() != "no leader gRPC address known" {
		t.Fatalf("expected no-leader err, got %v", err)
	}
}
