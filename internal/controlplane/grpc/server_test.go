package grpcsrv_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// startTestServer generates a fresh cluster CA + node cert for 127.0.0.1,
// builds a state with a known operator token, starts the gRPC server on a
// random port, and returns a ready client + the cleartext operator token.
func startTestServer(t *testing.T) (pb.ClusterClient, string, *grpcsrv.Server) {
	t.Helper()

	caCertPEM, caKeyPEM, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	_, csrPEM, err := ca.GenerateNodeKeypair("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	nodeKeyPEM, _, err := ca.GenerateNodeKeypair("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	// We need the key that goes with the CSR — regenerate as a pair.
	nodeKeyPEM, csrPEM, err = ca.GenerateNodeKeypair("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}
	nodeCertPEM, err := ca.SignNodeCSR(csrPEM, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}

	st := state.New(watch.NewRegistry())
	operatorToken := "operator-secret-token-hex-form"
	hash := sha256.Sum256([]byte(operatorToken))
	st.Tokens.Apply(&pb.Token{
		Identity:     "operator",
		HashedSecret: hash[:],
		IssuedAt:     timestamppb.Now(),
	}, 1)

	srv, err := grpcsrv.NewServer(grpcsrv.Options{
		BindAddr: "127.0.0.1:0",
		NodeCert: nodeCertPEM,
		NodeKey:  nodeKeyPEM,
		CACert:   caCertPEM,
		State:    st,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Stop)

	// Wait for listener to be reachable.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := dialClient(srv.Addr().String(), caCertPEM)
		if err == nil {
			client := pb.NewClusterClient(conn)
			return client, operatorToken, srv
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s", srv.Addr())
	return nil, "", nil
}

func dialClient(addr string, caPEM []byte) (*grpc.ClientConn, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

func TestServer_ValidTokenAuthenticatedRPCSucceeds(t *testing.T) {
	client, token, _ := startTestServer(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	resp, err := client.Status(ctx, &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp == nil {
		t.Fatalf("nil response")
	}
}

func TestServer_BadTokenSurfacesTokenInvalid(t *testing.T) {
	client, _, _ := startTestServer(t)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer wrong-token")
	_, err := client.Status(ctx, &pb.ClusterStatusRequest{})
	if err == nil {
		t.Fatalf("expected Unauthenticated error, got nil")
	}
	sErr, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status error: %v", err)
	}
	if sErr.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", sErr.Code())
	}
	if !strings.Contains(sErr.Message(), "token_invalid") {
		t.Errorf("message %q does not contain 'token_invalid'", sErr.Message())
	}
	// Typed Error proto should ride in details too.
	for _, d := range sErr.Details() {
		if e, ok := d.(*pb.Error); ok && e.GetCode() == "token_invalid" {
			return
		}
	}
	t.Errorf("expected pb.Error{code:token_invalid} in details; got %+v", sErr.Details())
}

func TestServer_NoTokenSurfacesTokenInvalid(t *testing.T) {
	client, _, _ := startTestServer(t)
	_, err := client.Status(context.Background(), &pb.ClusterStatusRequest{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	sErr, _ := status.FromError(err)
	if sErr.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", sErr.Code())
	}
}
