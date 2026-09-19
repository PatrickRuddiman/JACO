package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type enrollmentCLIRequest struct {
	request       *pb.IssueJoinTokenRequest
	authorization []string
}

type enrollmentCLIPeer struct {
	pb.UnimplementedClusterServer
	requests chan enrollmentCLIRequest
	caPEM    []byte
}

func (p *enrollmentCLIPeer) IssueJoinToken(ctx context.Context, req *pb.IssueJoinTokenRequest) (*pb.IssueJoinTokenResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	p.requests <- enrollmentCLIRequest{request: req, authorization: md.Get("authorization")}
	return &pb.IssueJoinTokenResponse{Token: "synthetic-issued-token", CaCert: p.caPEM}, nil
}

func TestIssueJoinTokenCLIAuthenticatesServerBeforeBearer(t *testing.T) {
	trustedCA, trustedKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	otherCA, otherKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "trusted-ca.crt")
	if err := os.WriteFile(caPath, trustedCA, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trusted-ip", "trusted-dns", "wrong-ca", "wrong-name"} {
		t.Run(name, func(t *testing.T) {
			root, rootKey := trustedCA, trustedKey
			certName, ips := "leader", []net.IP{net.ParseIP("127.0.0.1")}
			switch name {
			case "wrong-ca":
				root, rootKey = otherCA, otherKey
			case "wrong-name":
				ips = nil
			case "trusted-dns":
				certName, ips = "localhost", nil
			}
			key, csr, err := ca.GenerateNodeKeypair(certName, ips...)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := ca.SignNodeCSR(csr, root, rootKey)
			if err != nil {
				t.Fatal(err)
			}
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12,
			})))
			peer := &enrollmentCLIPeer{requests: make(chan enrollmentCLIRequest, 1), caPEM: trustedCA}
			pb.RegisterClusterServer(server, peer)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			addr := listener.Addr().String()
			if name == "trusted-dns" {
				_, port, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort("localhost", port)
			}
			var out bytes.Buffer
			cmd := nodeIssueJoinTokenCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{
				"--server", addr, "--token", "synthetic-operator-token", "--ca-cert", caPath,
				"--node-name", "new-node", "--san", "10.0.0.2", "--san", "node.private",
			})
			err = cmd.Execute()
			if name == "wrong-ca" || name == "wrong-name" {
				if err == nil {
					t.Error("untrusted server accepted")
				}
				select {
				case <-peer.requests:
					t.Fatal("untrusted server received the operator bearer token")
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-peer.requests:
				if got.request.GetNodeName() != "new-node" ||
					!reflect.DeepEqual(got.request.GetAllowedSans(), []string{"10.0.0.2", "node.private"}) {
					t.Errorf("approved enrollment scope changed: %v", got.request)
				}
				if !reflect.DeepEqual(got.authorization, []string{"Bearer synthetic-operator-token"}) {
					t.Errorf("operator authorization missing: %v", got.authorization)
				}
			default:
				t.Fatal("trusted server did not receive the request")
			}
		})
	}
}
