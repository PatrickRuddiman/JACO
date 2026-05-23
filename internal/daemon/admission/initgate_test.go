package admission_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
)

func info(method string) *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: method}
}

// noopHandler returns "ok" so tests can tell handler-was-called vs. gated.
func noopHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

func TestInitGate_PreInitAllowedMethodsPassThrough(t *testing.T) {
	g := admission.New()
	intr := g.UnaryInterceptor(nil)
	for _, method := range []string{
		"/jaco.v1.Cluster/Init",
		"/jaco.v1.Cluster/Join",
		"/jaco.v1.Cluster/Status",
	} {
		got, err := intr(context.Background(), nil, info(method), noopHandler)
		if err != nil {
			t.Errorf("%s: err = %v, want nil", method, err)
		}
		if got != "ok" {
			t.Errorf("%s: handler not called", method)
		}
	}
}

func TestInitGate_PreInitBlocksEverythingElse(t *testing.T) {
	g := admission.New()
	intr := g.UnaryInterceptor(nil)

	cases := []string{
		"/jaco.v1.Deploy/Apply",
		"/jaco.v1.Cluster/Bootstrap",
		"/jaco.v1.Cluster/NodeJoin",
		"/jaco.v1.Tokens/Issue",
		"/jaco.v1.Audit/Query",
	}
	for _, method := range cases {
		_, err := intr(context.Background(), nil, info(method), noopHandler)
		if err == nil {
			t.Errorf("%s should be gated; got nil error", method)
			continue
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.Unavailable {
			t.Errorf("%s code = %v, want Unavailable", method, st.Code())
		}
		if !strings.Contains(st.Message(), "cluster_uninitialized") {
			t.Errorf("%s message = %q, want cluster_uninitialized", method, st.Message())
		}
	}
}

func TestInitGate_PostInitFallsThroughToWrapped(t *testing.T) {
	g := admission.New()
	called := false
	wrapped := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		called = true
		return h(ctx, req)
	}
	intr := g.UnaryInterceptor(wrapped)

	// Pre-init: wrapped not called.
	_, _ = intr(context.Background(), nil, info("/jaco.v1.Deploy/Apply"), noopHandler)
	if called {
		t.Errorf("wrapped called pre-init")
	}

	g.MarkInitialized()
	// Post-init: wrapped called.
	got, err := intr(context.Background(), nil, info("/jaco.v1.Deploy/Apply"), noopHandler)
	if err != nil || got != "ok" || !called {
		t.Errorf("post-init pass-through failed: got=%v err=%v called=%v", got, err, called)
	}
}

func TestInitGate_PostInitNoWrappedJustCallsHandler(t *testing.T) {
	g := admission.New()
	g.MarkInitialized()
	intr := g.UnaryInterceptor(nil)
	got, err := intr(context.Background(), nil, info("/jaco.v1.Deploy/Apply"), noopHandler)
	if err != nil || got != "ok" {
		t.Errorf("nil wrapped + initialized should call handler: got=%v err=%v", got, err)
	}
}

func TestInitGate_StreamInterceptorParallelsUnary(t *testing.T) {
	g := admission.New()
	intr := g.StreamInterceptor(nil)

	streamInfo := &grpc.StreamServerInfo{FullMethod: "/jaco.v1.Deploy/Logs"}
	err := intr(nil, nil, streamInfo, func(any, grpc.ServerStream) error { return nil })
	if err == nil {
		t.Fatal("stream Deploy.Logs should be gated pre-init")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}

	// Allowed stream RPC (Status is unary but Watch.Subscribe is stream; we
	// can confirm the allow path by faking the method name).
	allowed := &grpc.StreamServerInfo{FullMethod: "/jaco.v1.Cluster/Status"}
	err = intr(nil, nil, allowed, func(any, grpc.ServerStream) error { return nil })
	if err != nil {
		t.Errorf("Cluster.Status stream should pass pre-init: %v", err)
	}

	g.MarkInitialized()
	err = intr(nil, nil, streamInfo, func(any, grpc.ServerStream) error { return errors.New("stream-handler-called") })
	if err == nil || err.Error() != "stream-handler-called" {
		t.Errorf("post-init should reach handler; got %v", err)
	}
}

func TestInitGate_IsInitializedReflectsMark(t *testing.T) {
	g := admission.New()
	if g.IsInitialized() {
		t.Errorf("fresh gate reports initialized")
	}
	g.MarkInitialized()
	if !g.IsInitialized() {
		t.Errorf("after MarkInitialized still uninitialized")
	}
}
