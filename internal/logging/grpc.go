package logging

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// requestIDMeta is the inbound metadata key honored for caller-supplied
// correlation IDs (issue #38 Q3). When present its first value is reused as
// the request_id; otherwise a fresh UUID is minted per RPC.
const requestIDMeta = "x-request-id"

// requestID resolves the correlation id for an inbound RPC: an incoming
// x-request-id metadata value if the caller supplied one, else a fresh UUID.
func requestID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(requestIDMeta); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return uuid.NewString()
}

// peerAddr returns the remote peer's address, or "unknown" when the transport
// didn't record one (e.g. some in-process unix-socket paths).
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

// rpcLogger derives the per-RPC contextual logger from base, tagging it with
// request_id, method, and peer.
func rpcLogger(ctx context.Context, base *slog.Logger, fullMethod string) *slog.Logger {
	if base == nil {
		base = Discard()
	}
	return base.With(
		KeyRequestID, requestID(ctx),
		KeyMethod, fullMethod,
		KeyPeer, peerAddr(ctx),
	)
}

// UnaryServerInterceptor attaches a request-scoped logger (request_id +
// method + peer) to the context before the handler runs. Handlers downstream
// read it via FromContext. It logs an RPC start at DEBUG; per-RPC end logging
// is left to handlers so they can choose the level by operation type
// (issue #38: write ops at INFO, reads at DEBUG).
//
// This is composed alongside (not instead of) the existing admission
// interceptor chain in internal/daemon/grpc/server.go — it must run first so
// admission decisions are logged with the request_id attached.
func UnaryServerInterceptor(base *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		l := rpcLogger(ctx, base, info.FullMethod)
		ctx = IntoContext(ctx, l)
		l.DebugContext(ctx, "rpc start")
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is the streaming counterpart of
// UnaryServerInterceptor. It wraps the ServerStream so the request-scoped
// logger flows to the handler via stream.Context().
func StreamServerInterceptor(base *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		l := rpcLogger(ctx, base, info.FullMethod)
		l.DebugContext(ctx, "stream start")
		wrapped := &loggingStream{ServerStream: ss, ctx: IntoContext(ctx, l)}
		return handler(srv, wrapped)
	}
}

// loggingStream overrides Context() so the request-scoped logger reaches the
// streaming handler.
type loggingStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *loggingStream) Context() context.Context { return s.ctx }
