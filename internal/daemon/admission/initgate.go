// Package admission wraps gRPC admission to gate RPCs while the daemon is
// uninitialized. Pre-init only Cluster.{Init, Join, Status} accept; every
// other RPC returns codes.Unavailable + typed pb.Error{cluster_uninitialized}.
//
// Once the daemon's Initialized atomic flag flips, this layer becomes a
// pass-through to the wrapped (token-based) interceptor.
package admission

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AllowedPreInit lists the full gRPC method names that bypass the gate
// while the daemon is uninitialized. Anything not in this set returns
// cluster_uninitialized.
var AllowedPreInit = map[string]bool{
	"/jaco.v1.Cluster/Init":   true,
	"/jaco.v1.Cluster/Join":   true,
	"/jaco.v1.Cluster/Status": true,
}

// InitGate tracks the daemon's initialized state and produces the two
// interceptors that wrap the regular token-based admission.
type InitGate struct {
	initialized atomic.Bool
}

// New constructs an InitGate. Starts in the uninitialized state.
func New() *InitGate { return &InitGate{} }

// MarkInitialized flips the gate open. Subsequent RPCs fall through to the
// wrapped interceptor.
func (g *InitGate) MarkInitialized() { g.initialized.Store(true) }

// IsInitialized reports whether the gate is open.
func (g *InitGate) IsInitialized() bool { return g.initialized.Load() }

// UnaryInterceptor wraps next so it only runs when the gate is open OR the
// method is in AllowedPreInit. next may be nil — then pre-gate methods
// dispatch to the handler directly.
func (g *InitGate) UnaryInterceptor(next grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if g.initialized.Load() {
			if next == nil {
				return handler(ctx, req)
			}
			return next(ctx, req, info, handler)
		}
		if AllowedPreInit[info.FullMethod] {
			return handler(ctx, req)
		}
		return nil, status.Error(codes.Unavailable,
			"cluster_uninitialized: daemon has no raft state — run `jaco cluster init` or `jaco node join`")
	}
}

// StreamInterceptor mirrors UnaryInterceptor for streaming RPCs.
func (g *InitGate) StreamInterceptor(next grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if g.initialized.Load() {
			if next == nil {
				return handler(srv, ss)
			}
			return next(srv, ss, info, handler)
		}
		if AllowedPreInit[info.FullMethod] {
			return handler(srv, ss)
		}
		return status.Error(codes.Unavailable,
			"cluster_uninitialized: daemon has no raft state — run `jaco cluster init` or `jaco node join`")
	}
}
