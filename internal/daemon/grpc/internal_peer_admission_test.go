package grpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func reissueRPCCertificate(t *testing.T, cert tls.Certificate, caPEM, keyPEM []byte, change func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	root, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(root.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	change(leaf)
	leaf.RawSubject = nil
	der, err := x509.CreateCertificate(rand.Reader, leaf, issuer, leaf.PublicKey, root.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert.Certificate = [][]byte{der}
	cert.Leaf = nil
	return cert
}

func TestInternalRPCRejectsInvalidPeersOnEveryMethod(t *testing.T) {
	s := internalRPCServer(t)
	caPEM, caKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	s.state.Cluster.Set(&pb.ClusterMeta{ClusterId: "test", CaCert: caPEM}, 1)
	s.state.Nodes.Apply(&pb.Node{Hostname: "worker"}, 1)
	hash := sha256.Sum256([]byte("operator-secret"))
	s.state.Tokens.Apply(&pb.Token{Identity: "operator", HashedSecret: hash[:]}, 1)
	member := nodeRPCCertificate(t, "worker", caPEM, caKey)
	otherCA, otherKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		cert   *tls.Certificate
		bearer bool
		code   codes.Code
	}{
		{name: "anonymous", code: codes.Unauthenticated},
		{name: "operator bearer without node certificate", bearer: true, code: codes.Unauthenticated},
		{name: "nonmember", cert: certPointer(nodeRPCCertificate(t, "outsider", caPEM, caKey)), code: codes.PermissionDenied},
		{name: "wrong CA", cert: certPointer(nodeRPCCertificate(t, "worker", otherCA, otherKey)), code: codes.Unauthenticated},
	}
	for name, mutate := range map[string]func(*x509.Certificate){
		"expired":       func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) },
		"not yet valid": func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) },
		"server only":   func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"CA identity":   func(c *x509.Certificate) { c.IsCA, c.BasicConstraintsValid = true, true },
		"empty name":    func(c *x509.Certificate) { c.Subject.CommonName = "" },
		"wrong SAN":     func(c *x509.Certificate) { c.DNSNames = []string{"another-node"} },
	} {
		cert := reissueRPCCertificate(t, member, caPEM, caKey, mutate)
		cases = append(cases, struct {
			name   string
			cert   *tls.Certificate
			bearer bool
			code   codes.Code
		}{name: name, cert: &cert, code: codes.Unauthenticated})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var certs []tls.Certificate
			if tt.cert != nil {
				certs = append(certs, *tt.cert)
			}
			client := pb.NewInternalClient(internalRPCClient(t, s, certs...))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tt.bearer {
				ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-secret"))
			}
			for name, call := range map[string]func() error{
				"Submit": func() error {
					_, err := client.Submit(ctx, &pb.SubmitRequest{CommandBytes: []byte{1}})
					return err
				},
				"Logs": func() error {
					_, err := receiveInternalLog(ctx, client, &pb.LogsRequest{Deployment: "private"})
					return err
				},
				"EnsureSubnet": func() error {
					_, err := client.EnsureSubnet(ctx, &pb.EnsureSubnetRequest{Deployment: "private", Network: "_default", Host: "worker"})
					return err
				},
				"SignNodeCert": func() error {
					_, err := client.SignNodeCert(ctx, &pb.SignNodeCertRequest{})
					return err
				},
			} {
				if err := call(); status.Code(err) != tt.code {
					t.Errorf("%s: %v, want %v", name, err, tt.code)
				}
			}
		})
	}
}

func certPointer(cert tls.Certificate) *tls.Certificate { return &cert }

func TestInternalRPCRechecksLiveTrustOnExistingConnection(t *testing.T) {
	s := internalRPCServer(t)
	caPEM, caKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	meta := &pb.ClusterMeta{ClusterId: "test", CaCert: caPEM}
	s.state.Cluster.Set(meta, 1)
	s.state.Nodes.Apply(&pb.Node{Hostname: "worker"}, 1)
	client := pb.NewInternalClient(internalRPCClient(t, s, nodeRPCCertificate(t, "worker", caPEM, caKey)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assertCode := func(want codes.Code) {
		t.Helper()
		if _, err := client.SignNodeCert(ctx, &pb.SignNodeCertRequest{}); status.Code(err) != want {
			t.Fatalf("SignNodeCert = %v, want %v", err, want)
		}
	}
	assertCode(codes.Unimplemented)
	s.state.Nodes.Remove("worker", 2)
	assertCode(codes.PermissionDenied)
	s.state.Nodes.Apply(&pb.Node{Hostname: "worker"}, 3)
	assertCode(codes.Unimplemented)
	replacement, _, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	meta.CaCert = replacement
	s.state.Cluster.Set(meta, 4)
	assertCode(codes.Unauthenticated)
}

func TestInternalRPCDoesNotGrantPublicOperatorAuthority(t *testing.T) {
	s, memberConn := internalRPCLeader(t)
	s.tokens.set(grpcsrv.NewTokensServer(s.state, s.Raft()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &pb.TokenIssueRequest{Identity: "created-by-operator"}
	if _, err := pb.NewTokensClient(memberConn).Issue(ctx, req); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("node certificate granted public operator authority: %v", err)
	}
	hash := sha256.Sum256([]byte("operator-secret"))
	s.state.Tokens.Apply(&pb.Token{Identity: "operator", HashedSecret: hash[:]}, 1)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-secret"))
	resp, err := pb.NewTokensClient(internalRPCClient(t, s)).Issue(ctx, req)
	if err != nil || resp.GetToken() == "" {
		t.Fatalf("operator bearer without client cert must retain public API access: response=%v err=%v", resp, err)
	}
	if _, ok := s.state.Tokens.Get(req.GetIdentity()); !ok {
		t.Fatal("authorized public token mutation did not persist")
	}
}
