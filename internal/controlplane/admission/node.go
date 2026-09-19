package admission

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
)

type nodeCertificateCtxKey struct{}

// NodeCertificateFromContext returns the verified member certificate, never a
// caller-supplied metadata identity. Unix-local operator requests return nil.
func NodeCertificateFromContext(ctx context.Context) *x509.Certificate {
	cert, _ := ctx.Value(nodeCertificateCtxKey{}).(*x509.Certificate)
	return cert
}

// AuthenticateNode verifies the live certificate, CA, and membership and returns
// the bound node context. Streams also use it before releasing each log line.
func AuthenticateNode(ctx context.Context, s *state.State) (context.Context, error) {
	if isUnixPeer(ctx) {
		return context.WithValue(ctx, identityCtxKey{}, LocalIdentity), nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, authError("peer_invalid", "TLS client certificate required")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || !info.State.HandshakeComplete || len(info.State.PeerCertificates) == 0 {
		return nil, authError("peer_invalid", "TLS client certificate required")
	}
	cert := info.State.PeerCertificates[0]
	if cert.IsCA || cert.Subject.CommonName == "" || s == nil {
		return nil, authError("peer_invalid", "node certificate required")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(s.Cluster.Get().GetCaCert()) {
		return nil, authError("peer_invalid", "cluster CA unavailable")
	}
	intermediates := x509.NewCertPool()
	for _, intermediate := range info.State.PeerCertificates[1:] {
		intermediates.AddCert(intermediate)
	}
	// Reverify on each RPC, not just at connection establishment: expiration,
	// CA replacement, and member removal must also affect existing connections.
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		DNSName:       cert.Subject.CommonName,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, authError("peer_invalid", "invalid node certificate")
	}
	if _, ok := s.Nodes.Get(cert.Subject.CommonName); !ok {
		return nil, status.Error(codes.PermissionDenied, "peer_not_member")
	}
	ctx = context.WithValue(ctx, nodeCertificateCtxKey{}, cert)
	return context.WithValue(ctx, identityCtxKey{}, "node:"+cert.Subject.CommonName), nil
}
