package admission_test

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func newStateWithToken(identity, secret string, revoked bool) *state.State {
	st := state.New(watch.NewRegistry())
	hash := sha256.Sum256([]byte(secret))
	tok := &pb.Token{
		Identity:     identity,
		HashedSecret: hash[:],
		IssuedAt:     timestamppb.Now(),
	}
	if revoked {
		tok.RevokedAt = timestamppb.Now()
	}
	st.Tokens.Apply(tok, 1)
	return st
}

func ctxWithAuth(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.MD{"authorization": []string{"Bearer " + token}})
}

// resolveOnce invokes the unary interceptor with a do-nothing handler and
// returns the context observed by the handler plus any error.
func resolveOnce(t *testing.T, st *state.State, ctx context.Context) (context.Context, error) {
	t.Helper()
	var seen context.Context
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = ctx
		return "ok", nil
	}
	// Status is in UnauthMethods; use a non-unauth method so the admission
	// interceptor actually exercises the bearer-token resolve path.
	info := &grpc.UnaryServerInfo{FullMethod: "/jaco.v1.Cluster/NodeList"}
	_, err := admission.UnaryInterceptor(st)(ctx, nil, info, handler)
	return seen, err
}

func TestAuthValidTokenAttachesIdentity(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	ctx, err := resolveOnce(t, st, ctxWithAuth("s3cret"))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if id := admission.IdentityFromContext(ctx); id != "alice" {
		t.Errorf("identity = %q, want alice", id)
	}
}

func TestAuthMissingMetadataRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	_, err := resolveOnce(t, st, context.Background())
	assertCode(t, err, codes.Unauthenticated, "token_invalid")
}

func TestAuthMissingHeaderRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	_, err := resolveOnce(t, st, ctx)
	assertCode(t, err, codes.Unauthenticated, "token_invalid")
}

func TestAuthNonBearerSchemeRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.MD{"authorization": []string{"Basic deadbeef"}})
	_, err := resolveOnce(t, st, ctx)
	assertCode(t, err, codes.Unauthenticated, "token_invalid")
}

func TestAuthUnknownTokenRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	_, err := resolveOnce(t, st, ctxWithAuth("wrong"))
	assertCode(t, err, codes.Unauthenticated, "token_invalid")
}

func TestAuthRevokedTokenRejected(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", true)
	_, err := resolveOnce(t, st, ctxWithAuth("s3cret"))
	assertCode(t, err, codes.Unauthenticated, "token_revoked")
}

func TestAuthErrorCarriesTypedErrorProtoInDetails(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	_, err := resolveOnce(t, st, ctxWithAuth("wrong"))
	sErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status error: %v", err)
	}
	details := sErr.Details()
	if len(details) == 0 {
		t.Fatalf("expected at least one detail proto attached")
	}
	pbErr, ok := details[0].(*pb.Error)
	if !ok {
		t.Fatalf("detail[0] type = %T, want *pb.Error", details[0])
	}
	if pbErr.GetCode() != "token_invalid" {
		t.Errorf("detail.code = %q, want token_invalid", pbErr.GetCode())
	}
}

func assertCode(t *testing.T, err error, want codes.Code, wantCodeStr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	sErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status error: %v", err)
	}
	if sErr.Code() != want {
		t.Errorf("code = %v, want %v", sErr.Code(), want)
	}
	if !strings.Contains(sErr.Message(), wantCodeStr) {
		t.Errorf("message %q does not contain %q", sErr.Message(), wantCodeStr)
	}
}
