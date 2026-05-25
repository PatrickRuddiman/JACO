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
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// identityCtxKey is the typed key under which authResolve stashes the
// authenticated identity in the request context.
type identityCtxKey struct{}

// LocalIdentity is the principal attributed to RPCs that arrive over the
// unix socket. The socket's filesystem permissions (mode 0660, owned by the
// jaco group) ARE the auth mechanism — the kernel checks group membership on
// connect(2) — so the bearer token is not required. Audit still records this
// principal so on-node actions remain attributed.
const LocalIdentity = "local"

// IdentityFromContext returns the authenticated identity for the current RPC,
// or the empty string when admission has not attached one (e.g. a no-auth RPC
// or test).
func IdentityFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(identityCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// UnauthMethods lists gRPC methods that bypass the bearer-token check. These
// RPCs gate themselves via a body-carried credential — e.g. NodeJoin verifies
// the single-use join_token in the request body — or are explicitly safe to
// expose unauthenticated (Cluster.Status is read-only and reveals only the
// liveness summary an operator needs to confirm the daemon is reachable).
var UnauthMethods = map[string]bool{
	"/jaco.v1.Cluster/NodeJoin": true,
	"/jaco.v1.Cluster/Status":   true,
	// Internal.* is the peer-to-peer surface follower nodes use to
	// forward raft.Apply work to the leader. Today it relies on the
	// overlay network for auth; peer mTLS lands in a follow-up iter.
	"/jaco.v1.Internal/Submit":       true,
	"/jaco.v1.Internal/SignNodeCert": true,
	"/jaco.v1.Internal/Logs":         true,
	"/jaco.v1.Internal/EnsureSubnet": true,
}

// UnaryInterceptor returns a grpc.UnaryServerInterceptor that runs
// authResolve on every incoming RPC unless the method is unauthenticated.
func UnaryInterceptor(s *state.State) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if UnauthMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		newCtx, err := authResolve(ctx, s)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// StreamInterceptor returns a grpc.StreamServerInterceptor that runs
// authResolve on every incoming stream's initial metadata unless the method
// is unauthenticated.
func StreamInterceptor(s *state.State) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if UnauthMethods[info.FullMethod] {
			return handler(srv, ss)
		}
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

// isUnixPeer reports whether the RPC arrived over the local unix socket.
// It fails closed: when no peer info is present (ok=false) or the peer's
// network is anything other than "unix", it returns false so the caller
// requires the bearer token. This is a security boundary — the bypass must
// trigger ONLY for genuine unix-socket peers, never for TCP.
func isUnixPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return false
	}
	return p.Addr.Network() == "unix"
}

// authResolve authenticates the RPC and returns a context carrying the
// resolved identity. Unix-socket peers are trusted by the socket's
// filesystem permissions and bypass the bearer check (attributed to
// LocalIdentity); TCP peers must present a valid, non-revoked Bearer token.
func authResolve(ctx context.Context, s *state.State) (context.Context, error) {
	if isUnixPeer(ctx) {
		return context.WithValue(ctx, identityCtxKey{}, LocalIdentity), nil
	}
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
