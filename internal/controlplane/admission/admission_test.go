package admission_test

import (
	"context"
	"crypto/sha256"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ctxWithUnixPeer returns a context whose gRPC peer reports a unix-socket
// network, mirroring what the daemon sees when an operator dials over the
// local socket. A *net.UnixAddr's Network() returns "unix".
func ctxWithUnixPeer(ctx context.Context) context.Context {
	return peer.NewContext(ctx, &peer.Peer{
		Addr: &net.UnixAddr{Name: "/var/run/jaco/jaco.sock", Net: "unix"},
	})
}

// ctxWithTCPPeer returns a context whose gRPC peer reports a tcp network,
// so the bearer-token path is still exercised even when peer info exists.
func ctxWithTCPPeer(ctx context.Context) context.Context {
	return peer.NewContext(ctx, &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 7000},
	})
}

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

// TestAuthUnixPeerBypassesBearer verifies the security boundary: a genuine
// unix-socket peer is trusted by the socket's filesystem permissions and
// proceeds with NO bearer token, attributed to the local on-node principal.
func TestAuthUnixPeerBypassesBearer(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	// No authorization metadata at all — the unix transport is the auth.
	ctx, err := resolveOnce(t, st, ctxWithUnixPeer(context.Background()))
	if err != nil {
		t.Fatalf("expected unix peer to bypass bearer, got %v", err)
	}
	if id := admission.IdentityFromContext(ctx); id != admission.LocalIdentity {
		t.Errorf("identity = %q, want %q", id, admission.LocalIdentity)
	}
}

// TestAuthUnixPeerIgnoresProvidedToken confirms the bypass does not consult
// any token presented over the socket — the transport alone authenticates.
func TestAuthUnixPeerIgnoresProvidedToken(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	// Even a bogus bearer over the unix socket succeeds; the token is ignored.
	ctx := ctxWithUnixPeer(ctxWithAuth("totally-wrong"))
	got, err := resolveOnce(t, st, ctx)
	if err != nil {
		t.Fatalf("expected unix peer to bypass bearer, got %v", err)
	}
	if id := admission.IdentityFromContext(got); id != admission.LocalIdentity {
		t.Errorf("identity = %q, want %q", id, admission.LocalIdentity)
	}
}

// TestAuthTCPPeerStillRequiresToken confirms that when peer info is present
// and reports tcp, the bearer token is still required (the bypass must NOT
// trigger for TCP).
func TestAuthTCPPeerStillRequiresToken(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	_, err := resolveOnce(t, st, ctxWithTCPPeer(context.Background()))
	assertCode(t, err, codes.Unauthenticated, "token_invalid")
}

// TestAuthTCPPeerValidTokenAttachesIdentity confirms the TCP+bearer path is
// unchanged: a valid token over a tcp peer resolves to that token's identity.
func TestAuthTCPPeerValidTokenAttachesIdentity(t *testing.T) {
	st := newStateWithToken("alice", "s3cret", false)
	ctx, err := resolveOnce(t, st, ctxWithTCPPeer(ctxWithAuth("s3cret")))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if id := admission.IdentityFromContext(ctx); id != "alice" {
		t.Errorf("identity = %q, want alice", id)
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
