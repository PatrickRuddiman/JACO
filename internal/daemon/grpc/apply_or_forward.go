package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// raftLocalApplier is the subset of *raftnode.Node that
// applyOrForwardCommand needs. Tests substitute a fake.
type raftLocalApplier interface {
	Apply(cmd []byte, timeoutHint int64) (uint64, error)
}

// leaderForwarderFn forwards an already-marshaled command to whichever
// node currently holds the raft leadership, via its Internal.Submit RPC.
// Returns the same wrapped error shape as the inline production path so
// callers can log it the same way regardless of which path was taken.
type leaderForwarderFn func(ctx context.Context, data []byte) error

// applyOrForwardCommand attempts a local raft Apply; on hraft.ErrNotLeader
// it falls back to forward. Any other Apply error is returned directly.
// This is the single source of truth for the leader-or-forward pattern used
// by the firewall reconciler's Audit + UpdateStatus callbacks (issue #88).
//
// Production wiring passes localApply = node.Apply and forward = a closure
// that dials the leader's gRPC address (from state.Nodes) and calls
// Internal.Submit; tests inject fakes for both.
func applyOrForwardCommand(
	ctx context.Context,
	data []byte,
	localApply func(cmd []byte) (uint64, error),
	forward leaderForwarderFn,
) error {
	if _, err := localApply(data); err == nil {
		return nil
	} else if !errors.Is(err, hraft.ErrNotLeader) {
		return err
	}
	return forward(ctx, data)
}

// dialAndSubmit is the production leaderForwarderFn body factored out so
// the inline production closure stays a one-liner. It assumes the caller
// has already resolved the leader's gRPC address; an empty addr surfaces
// as a no-leader error rather than a dial attempt.
func dialAndSubmit(ctx context.Context, leaderAddr string, data []byte) error {
	if leaderAddr == "" {
		return fmt.Errorf("no leader gRPC address known")
	}
	conn, err := grpc.NewClient(leaderAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		return fmt.Errorf("dial leader %s: %w", leaderAddr, err)
	}
	defer conn.Close()
	if _, err := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data}); err != nil {
		return fmt.Errorf("forward to leader %s: %w", leaderAddr, err)
	}
	return nil
}
