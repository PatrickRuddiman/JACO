package grpc

import (
	"bytes"
	"context"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestJoinRequiresProvisionedCA(t *testing.T) {
	trustedCA, trustedKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	cert := peerCertificate(t, trustedCA, trustedKey, "peer", net.ParseIP("127.0.0.1"))
	for name, caPEM := range map[string][]byte{
		"missing": nil,
		"invalid": []byte("not a CA"),
		"leaf":    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		"key-in-bundle": append(append([]byte{}, trustedCA...),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a CA")})...),
	} {
		t.Run(name, func(t *testing.T) {
			addr, peer := startRecordingPeer(t, cert, nil)
			s := joiningServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := s.cluster.Join(ctx, &pb.ClusterJoinRequest{
				PeerAddr: addr, JoinToken: "synthetic-join-token", CaCert: caPEM,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("invalid provisioned CA returned %v", err)
			}
			select {
			case <-peer.requests:
				t.Fatal("join credential was sent without valid provisioned trust")
			default:
			}
			if s.gate.IsInitialized() || s.Raft() != nil {
				t.Fatal("invalid provisioned CA initialized the node")
			}
		})
	}
}

func TestJoinPersistsProvisionedRotationBundle(t *testing.T) {
	oldCA, oldKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	newCA, newKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	bundle := bytes.Join([][]byte{oldCA, newCA}, []byte("\n"))
	serverCert := peerCertificate(t, oldCA, oldKey, "peer", net.ParseIP("127.0.0.1"))
	addr, peer := startRecordingPeer(t, serverCert, func(req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
		leaf, err := ca.SignNodeCSRForIdentity(req.GetCsrPem(), newCA, newKey, req.GetName(), []string{"127.0.0.1"})
		if err != nil {
			return nil, err
		}
		return &pb.NodeJoinResponse{
			ClusterId: "rotation-test", CaCert: newCA, SignedCert: leaf,
			PeerAddrs: []string{"127.0.0.1:7001"},
		}, nil
	})
	s := joiningServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.cluster.Join(ctx, &pb.ClusterJoinRequest{
		PeerAddr: addr, JoinToken: "synthetic-join-token", CaCert: bundle,
	}); err != nil {
		t.Fatal(err)
	}
	if !s.gate.IsInitialized() || s.Raft() == nil {
		t.Fatal("trusted enrollment did not initialize the node")
	}
	saved, err := os.ReadFile(filepath.Join(s.dataDir, "node", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, bundle) {
		t.Fatal("remote response replaced or narrowed independently provisioned trust")
	}
	select {
	case req := <-peer.requests:
		join, ok := req.(*pb.NodeJoinRequest)
		if !ok || join.GetJoinToken() != "synthetic-join-token" {
			t.Fatalf("unexpected authenticated enrollment request: %v", req)
		}
	default:
		t.Fatal("authenticated peer did not receive the token")
	}
}
