package admission_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
)

// stubStream is a minimal grpc.ServerStream so we can drive
// StreamInterceptor through its handler.
type stubStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stubStream) Context() context.Context { return s.ctx }

func TestStreamInterceptor_AttachesIdentityOnValidToken(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)

	var observedID string
	handler := func(_ any, ss grpc.ServerStream) error {
		observedID = admission.IdentityFromContext(ss.Context())
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/jaco.v1.Watch/Subscribe"}
	stream := &stubStream{ctx: ctxWithAuth("s3cret")}

	if err := admission.StreamInterceptor(st)(nil, stream, info, handler); err != nil {
		t.Fatalf("StreamInterceptor: %v", err)
	}
	if observedID != "alice" {
		t.Errorf("identity = %q, want alice", observedID)
	}
}

func TestStreamInterceptor_UnauthMethodBypassesResolve(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	handler := func(_ any, ss grpc.ServerStream) error {
		// No identity should be attached.
		if id := admission.IdentityFromContext(ss.Context()); id != "" {
			t.Errorf("unauth method got identity = %q", id)
		}
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/jaco.v1.Internal/Logs"}
	// No bearer token at all — UnauthMethods should bypass resolve.
	stream := &stubStream{ctx: context.Background()}
	if err := admission.StreamInterceptor(st)(nil, stream, info, handler); err != nil {
		t.Errorf("unauth method err = %v", err)
	}
}

func TestStreamInterceptor_InvalidTokenRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	handler := func(_ any, _ grpc.ServerStream) error {
		t.Errorf("handler should not be invoked on rejected token")
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/jaco.v1.Watch/Subscribe"}
	stream := &stubStream{ctx: ctxWithAuth("wrong-token")}
	err := admission.StreamInterceptor(st)(nil, stream, info, handler)
	if err == nil {
		t.Fatalf("expected rejection")
	}
	se, _ := status.FromError(err)
	if se.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", se.Code())
	}
}

// TestIdentityFromContext_AbsentReturnsEmpty — defensive guard
// exercised when admission hasn't attached an identity (e.g. a test
// invoking the handler directly).
func TestIdentityFromContext_AbsentReturnsEmpty(t *testing.T) {
	if got := admission.IdentityFromContext(context.Background()); got != "" {
		t.Errorf("IdentityFromContext(bare) = %q, want \"\"", got)
	}
	// Even with metadata but no admission, returns empty.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	if got := admission.IdentityFromContext(ctx); got != "" {
		t.Errorf("IdentityFromContext(md only) = %q, want \"\"", got)
	}
}

// silence — used to keep state import live across both _test files.
var _ = state.New
