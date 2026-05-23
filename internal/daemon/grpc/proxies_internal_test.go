package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// The proxies in proxies.go fall back to errUnavail when their target
// hasn't been swapped in (i.e. pre-OpenRaft). The end-to-end gRPC tests
// exercise the wired-in path; this file pokes the proxies directly with
// nil targets to cover the defensive fallback branches.

func TestTokensProxy_NilTargetReturnsUnavailable(t *testing.T) {
	p := &tokensProxy{}
	ctx := context.Background()

	if _, err := p.Issue(ctx, &pb.TokenIssueRequest{}); !isUnavail(err) {
		t.Errorf("Issue with nil target = %v, want Unavailable", err)
	}
	if _, err := p.Revoke(ctx, &pb.TokenRevokeRequest{}); !isUnavail(err) {
		t.Errorf("Revoke with nil target = %v, want Unavailable", err)
	}
	if _, err := p.List(ctx, &pb.TokenListRequest{}); !isUnavail(err) {
		t.Errorf("List with nil target = %v, want Unavailable", err)
	}
}

func TestDeployProxy_NilTargetReturnsUnavailable(t *testing.T) {
	p := &deployProxy{}
	ctx := context.Background()

	if _, err := p.Apply(ctx, &pb.ApplyRequest{}); !isUnavail(err) {
		t.Errorf("Apply: %v", err)
	}
	if _, err := p.Rollback(ctx, &pb.RollbackRequest{}); !isUnavail(err) {
		t.Errorf("Rollback: %v", err)
	}
	if _, err := p.Delete(ctx, &pb.DeleteRequest{}); !isUnavail(err) {
		t.Errorf("Delete: %v", err)
	}
	if _, err := p.Status(ctx, &pb.DeployStatusRequest{}); !isUnavail(err) {
		t.Errorf("Status: %v", err)
	}
}

// TestDeployProxy_LogsWithoutServerBackref — the Logs proxy method needs
// the server back-reference; a zero deployProxy has server == nil and
// must fall back to errUnavail.
func TestDeployProxy_LogsWithoutServerBackref(t *testing.T) {
	p := &deployProxy{} // server == nil
	if err := p.Logs(&pb.LogsRequest{}, nil); !isUnavail(err) {
		t.Errorf("Logs with nil server = %v, want Unavailable", err)
	}
}

func TestAuditProxy_NilTargetReturnsUnavailable(t *testing.T) {
	p := &auditProxy{}
	if err := p.Query(&pb.AuditQueryRequest{}, nil); !isUnavail(err) {
		t.Errorf("Query: %v", err)
	}
}

func TestWatchProxy_NilTargetReturnsUnavailable(t *testing.T) {
	p := &watchProxy{}
	if err := p.Subscribe(&pb.SubscribeRequest{}, nil); !isUnavail(err) {
		t.Errorf("Subscribe: %v", err)
	}
}

// TestProxyGetters_ReadbackAfterSet — set then get returns the same
// pointer for each proxy type. Covers the get() accessors that the
// gRPC-driven tests don't otherwise exercise on the nil/non-nil
// boundary.
func TestProxyGetters_ReadbackAfterSet(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		p := &tokensProxy{}
		if p.get() != nil {
			t.Errorf("fresh tokensProxy.get() = non-nil")
		}
		stub := &fakeTokens{}
		p.set(stub)
		if got := p.get(); got != stub {
			t.Errorf("tokensProxy.get() did not round-trip")
		}
	})
	t.Run("deploy", func(t *testing.T) {
		p := &deployProxy{}
		if p.get() != nil {
			t.Errorf("fresh deployProxy.get() = non-nil")
		}
		stub := &fakeDeploy{}
		p.set(stub)
		if got := p.get(); got != stub {
			t.Errorf("deployProxy.get() did not round-trip")
		}
	})
	t.Run("audit", func(t *testing.T) {
		p := &auditProxy{}
		if p.get() != nil {
			t.Errorf("fresh auditProxy.get() = non-nil")
		}
		stub := &fakeAudit{}
		p.set(stub)
		if got := p.get(); got != stub {
			t.Errorf("auditProxy.get() did not round-trip")
		}
	})
	t.Run("watch", func(t *testing.T) {
		p := &watchProxy{}
		if p.get() != nil {
			t.Errorf("fresh watchProxy.get() = non-nil")
		}
		stub := &fakeWatch{}
		p.set(stub)
		if got := p.get(); got != stub {
			t.Errorf("watchProxy.get() did not round-trip")
		}
	})
}

// --- helpers ----------------------------------------------------------------

func isUnavail(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Unavailable
}

type fakeTokens struct{ pb.UnimplementedTokensServer }
type fakeDeploy struct{ pb.UnimplementedDeployServer }
type fakeAudit struct{ pb.UnimplementedAuditServer }
type fakeWatch struct{ pb.UnimplementedWatchServer }
