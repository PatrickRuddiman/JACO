// Package admission implements the bearer-token authentication gate for the
// control-plane gRPC surface. UnaryInterceptor and StreamInterceptor extract
// `authorization: Bearer <token>` from request metadata, SHA-256 the token,
// match it against state.Tokens, and attach the resolved identity to the
// downstream context. Bad tokens surface as Error{code:"token_invalid"};
// revoked tokens surface as Error{code:"token_revoked"}.
package admission

import (
	"context"
	"crypto/sha256"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// identityCtxKey is the typed key under which authResolve stashes the
// authenticated identity in the request context.
type identityCtxKey struct{}

// IdentityFromContext returns the authenticated identity for the current RPC,
// or the empty string when admission has not attached one (e.g. a no-auth RPC
// or test).
func IdentityFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(identityCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// UnaryInterceptor returns a grpc.UnaryServerInterceptor that runs
// authResolve on every incoming RPC.
func UnaryInterceptor(s *state.State) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := authResolve(ctx, s)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// StreamInterceptor returns a grpc.StreamServerInterceptor that runs
// authResolve on every incoming stream's initial metadata.
func StreamInterceptor(s *state.State) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := authResolve(ss.Context(), s)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: newCtx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// authResolve extracts the Bearer token, looks it up in state.Tokens, and
// returns a context carrying the resolved identity.
func authResolve(ctx context.Context, s *state.State) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, authError("token_invalid", "missing metadata")
	}
	auths := md.Get("authorization")
	if len(auths) == 0 {
		return nil, authError("token_invalid", "missing authorization header")
	}
	token := strings.TrimPrefix(auths[0], "Bearer ")
	if token == "" || token == auths[0] {
		return nil, authError("token_invalid", "authorization header must use Bearer scheme")
	}

	sum := sha256.Sum256([]byte(token))
	t, found := state.LookupTokenByHash(s.Tokens, sum[:])
	if !found {
		return nil, authError("token_invalid", "unknown token")
	}
	if t.GetRevokedAt() != nil {
		return nil, authError("token_revoked", "token has been revoked")
	}
	return context.WithValue(ctx, identityCtxKey{}, t.GetIdentity()), nil
}

// authError builds a codes.Unauthenticated status whose Message is the
// typed `code` (so simple text matchers see `token_invalid` /
// `token_revoked` directly) and whose details carry the full pb.Error
// envelope for richer client decoding.
func authError(code, msg string) error {
	st := status.New(codes.Unauthenticated, code)
	if detailed, err := st.WithDetails(&pb.Error{
		Code:    code,
		Message: msg,
	}); err == nil {
		st = detailed
	}
	return st.Err()
}
