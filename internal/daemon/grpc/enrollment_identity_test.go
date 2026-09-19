package grpc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func enrollmentLeader(t *testing.T) *Server {
	t.Helper()
	s := joiningServer(t)
	if _, err := s.cluster.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		meta := s.State().Cluster.Get()
		_, hasSelf := s.State().Nodes.Get("joining-node")
		if s.Raft().IsLeader() && hasSelf && len(meta.GetCaKey()) > 0 {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enrollment server did not become leader")
	return nil
}

func TestIssueJoinTokenRequiresApprovedIdentity(t *testing.T) {
	s := enrollmentLeader(t)
	handler := grpcsrv.NewClusterServer(s.State(), s.Raft())
	for _, req := range []*pb.IssueJoinTokenRequest{
		{},
		{NodeName: "new-node", AllowedSans: []string{"*.private"}},
		{NodeName: "new-node", AllowedSans: []string{"not a hostname"}},
		{NodeName: "joining-node"},
	} {
		before := s.State().JoinTokens.Len()
		if _, err := handler.IssueJoinToken(context.Background(), req); err == nil {
			t.Errorf("invalid enrollment scope accepted: %v", req)
		}
		if s.State().JoinTokens.Len() != before {
			t.Errorf("invalid scope persisted a usable join token: %v", req)
		}
	}
	req := &pb.IssueJoinTokenRequest{NodeName: "new-node", AllowedSans: []string{"127.0.0.1", "node.private"}}
	resp, err := handler.IssueJoinToken(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(resp.GetToken()))
	tok, ok := s.State().JoinTokens.Get(hex.EncodeToString(hash[:]))
	if !ok || tok.GetNodeName() != req.GetNodeName() || !reflect.DeepEqual(tok.GetAllowedSans(), req.GetAllowedSans()) {
		t.Fatalf("token did not retain its approved identity: %v", tok)
	}
}

func TestNodeJoinRejectsUnapprovedIdentityBeforeMembership(t *testing.T) {
	for _, implementation := range []string{"daemon", "control-plane"} {
		for _, name := range []string{"legacy-token", "different-node", "different-csr", "unapproved-raft", "unapproved-grpc", "existing-member"} {
			t.Run(implementation+"/"+name, func(t *testing.T) {
				s := enrollmentLeader(t)
				var handler pb.ClusterServer = s.cluster
				if implementation == "control-plane" {
					handler = grpcsrv.NewClusterServer(s.State(), s.Raft())
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				hash := sha256.Sum256([]byte("synthetic-scoped-token"))
				tok := &pb.JoinToken{
					HashedSecret: hash[:], ExpiresAt: timestamppb.New(time.Now().Add(time.Hour)),
					NodeName: "new-node", AllowedSans: []string{"127.0.0.1"},
				}
				req := &pb.NodeJoinRequest{
					Name: "new-node", JoinToken: "synthetic-scoped-token",
					AdvertiseAddr: listener.Addr().String(), GrpcAddress: "127.0.0.1:7000",
				}
				csrName := req.Name
				wantCode := codes.PermissionDenied
				switch name {
				case "legacy-token":
					tok.NodeName = ""
				case "different-node":
					req.Name = "another-node"
				case "different-csr":
					csrName = "another-node"
					wantCode = codes.InvalidArgument
				case "unapproved-raft":
					req.AdvertiseAddr = "127.0.0.2:7001"
				case "unapproved-grpc":
					req.GrpcAddress = "127.0.0.2:7000"
				case "existing-member":
					tok.NodeName, req.Name, csrName = "joining-node", "joining-node", "joining-node"
					wantCode = codes.AlreadyExists
				}
				_, req.CsrPem, err = ca.GenerateNodeKeypair(csrName)
				if err != nil {
					t.Fatal(err)
				}
				s.State().JoinTokens.Apply(tok, 0)
				before := s.Raft().Raft.GetConfiguration()
				if err := before.Error(); err != nil {
					t.Fatal(err)
				}
				_, err = handler.NodeJoin(context.Background(), req)
				if status.Code(err) != wantCode {
					t.Errorf("unapproved enrollment returned %v; want %v", err, wantCode)
				}
				stored, _ := s.State().JoinTokens.Get(hex.EncodeToString(hash[:]))
				if stored.GetConsumedAt() != nil {
					t.Error("unapproved enrollment consumed the join token")
				}
				after := s.Raft().Raft.GetConfiguration()
				if err := after.Error(); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.Configuration(), after.Configuration()) {
					t.Error("unapproved enrollment changed Raft membership")
				}
			})
		}
	}

}

func TestNodeJoinConsumesScopedTokenOnceUnderConcurrency(t *testing.T) {
	for _, implementation := range []string{"daemon", "control-plane"} {
		t.Run(implementation, func(t *testing.T) {
			s := enrollmentLeader(t)
			var handler pb.ClusterServer = s.cluster
			if implementation == "control-plane" {
				handler = grpcsrv.NewClusterServer(s.State(), s.Raft())
			}
			scope := &pb.IssueJoinTokenRequest{
				NodeName: "new-node", AllowedSans: []string{"127.0.0.1", "node.private"},
			}
			issued, err := handler.IssueJoinToken(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			type result struct {
				response *pb.NodeJoinResponse
				key      []byte
				err      error
			}
			const attempts = 8
			results := make(chan result, attempts)
			start := make(chan struct{})
			for i := 0; i < attempts; i++ {
				key, csr, err := ca.GenerateNodeKeypair("new-node", net.ParseIP("127.0.0.2"))
				if err != nil {
					t.Fatal(err)
				}
				req := &pb.NodeJoinRequest{
					Name: "new-node", JoinToken: issued.GetToken(), CsrPem: csr,
					AdvertiseAddr: listener.Addr().String(), GrpcAddress: "127.0.0.1:7000",
				}
				go func() {
					<-start
					resp, err := handler.NodeJoin(context.Background(), req)
					results <- result{response: resp, key: key, err: err}
				}()
			}
			close(start)
			successes := 0
			for i := 0; i < attempts; i++ {
				var got result
				select {
				case got = <-results:
				case <-time.After(10 * time.Second):
					t.Fatal("concurrent enrollment did not finish")
				}
				if got.err != nil {
					if status.Code(got.err) != codes.PermissionDenied {
						t.Errorf("replayed token returned unexpected error: %v", got.err)
					}
					continue
				}
				successes++
				pair, err := tls.X509KeyPair(got.response.GetSignedCert(), got.key)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(pair.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"new-node", "node.private", "127.0.0.1"} {
					if err := leaf.VerifyHostname(name); err != nil {
						t.Errorf("approved identity %q missing: %v", name, err)
					}
				}
				if leaf.VerifyHostname("127.0.0.2") == nil {
					t.Error("unapproved CSR alias was issued")
				}
			}
			if successes != 1 {
				t.Errorf("one single-use token issued %d certificates", successes)
			}
			member, ok := s.State().Nodes.Get("new-node")
			if !ok || member.GetGrpcAddress() != "127.0.0.1:7000" {
				t.Errorf("approved gRPC endpoint missing from membership: %v", member)
			}
		})
	}
}
