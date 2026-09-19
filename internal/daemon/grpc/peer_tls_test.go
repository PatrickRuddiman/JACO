package grpc

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type recordingPeer struct {
	pb.UnimplementedInternalServer
	pb.UnimplementedClusterServer
	requests     chan any
	joinResponse func(*pb.NodeJoinRequest) (*pb.NodeJoinResponse, error)
	tlsState     *dynamicTLS
}

func (p *recordingPeer) Logs(req *pb.LogsRequest, _ pb.Internal_LogsServer) error {
	p.requests <- req
	return nil
}

func (p *recordingPeer) Submit(_ context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	p.requests <- req
	return &pb.SubmitResponse{}, nil
}

func (p *recordingPeer) EnsureSubnet(_ context.Context, req *pb.EnsureSubnetRequest) (*pb.EnsureSubnetResponse, error) {
	p.requests <- req
	return &pb.EnsureSubnetResponse{Cidr: "10.42.0.0/24"}, nil
}

func (p *recordingPeer) NodeJoin(_ context.Context, req *pb.NodeJoinRequest) (*pb.NodeJoinResponse, error) {
	p.requests <- req
	if p.joinResponse != nil {
		return p.joinResponse(req)
	}
	return nil, status.Error(codes.PermissionDenied, "peer received join credential")
}

type peerLogStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s peerLogStream) Context() context.Context { return s.ctx }
func (s peerLogStream) Send(*pb.LogLine) error   { return nil }

func peerCertificate(t *testing.T, certPEM, keyPEM []byte, name string, ips ...net.IP) tls.Certificate {
	t.Helper()
	key, csr, err := ca.GenerateNodeKeypair(name, ips...)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignNodeCSR(csr, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func startRecordingPeer(t *testing.T, cert tls.Certificate, joinResponse func(*pb.NodeJoinRequest) (*pb.NodeJoinResponse, error)) (string, *recordingPeer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &recordingPeer{
		requests: make(chan any, 8), joinResponse: joinResponse,
		tlsState: newDynamicTLS(cert),
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: peer.tlsState.GetCertificate,
	})))
	pb.RegisterInternalServer(server, peer)
	pb.RegisterClusterServer(server, peer)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return lis.Addr().String(), peer
}

func peerServer(t *testing.T, caPEM, caKey []byte, addr string) *Server {
	t.Helper()
	dir := t.TempDir()
	writeNodeCredentials(t, dir, "local", peerCertificate(t, caPEM, caKey, "local", net.ParseIP("127.0.0.1")), caPEM)
	st := state.New(watch.NewRegistry())
	st.Nodes.Apply(&pb.Node{Hostname: "peer", GrpcAddress: addr}, 1)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "private-web-0", Deployment: "private", Service: "web", Host: "peer",
	}, 2)
	return &Server{dataDir: dir, state: st, cluster: &clusterServer{hostname: "local", dataDir: dir}}
}

func TestPeerLogsRejectUntrustedServerBeforeRequest(t *testing.T) {
	trustedCA, trustedKey, certificates := untrustedPeerCertificates(t)
	for name, cert := range certificates {
		t.Run(name, func(t *testing.T) {
			addr, peer := startRecordingPeer(t, cert, nil)
			s := peerServer(t, trustedCA, trustedKey, addr)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := s.streamDeploymentLogs(&pb.LogsRequest{Deployment: "private"}, peerLogStream{ctx: ctx})
			if err == nil {
				t.Error("untrusted peer accepted")
			}
			select {
			case req := <-peer.requests:
				t.Fatalf("untrusted peer received sensitive log request: %v", req)
			default:
			}
		})
	}
}

func TestPeerSubmitRejectsUntrustedServerBeforeCommand(t *testing.T) {
	trustedCA, trustedKey, certificates := untrustedPeerCertificates(t)
	for name, cert := range certificates {
		t.Run(name, func(t *testing.T) {
			addr, peer := startRecordingPeer(t, cert, nil)
			s := peerServer(t, trustedCA, trustedKey, addr)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := s.dialAndSubmit(ctx, addr, []byte("synthetic-private-key"))
			if err == nil {
				t.Error("untrusted peer accepted")
			}
			select {
			case req := <-peer.requests:
				t.Fatalf("untrusted peer received sensitive command: %v", req)
			default:
			}
		})
	}
}

func TestJoinRejectsUntrustedServerBeforeToken(t *testing.T) {
	trustedCA, trustedKey, certificates := untrustedPeerCertificates(t)
	for name, cert := range certificates {
		t.Run(name, func(t *testing.T) {
			addr, peer := startRecordingPeer(t, cert, nil)
			s := peerServer(t, trustedCA, trustedKey, addr)
			s.cluster.gate = admission.New()
			s.cluster.server = s
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.cluster.Join(ctx, &pb.ClusterJoinRequest{
				PeerAddr: addr, JoinToken: "synthetic-join-token", CaCert: trustedCA,
			}); err == nil {
				t.Error("untrusted enrollment server accepted")
			}
			select {
			case req := <-peer.requests:
				t.Fatalf("untrusted server received join credential: %v", req)
			default:
			}
			if s.cluster.gate.IsInitialized() {
				t.Fatal("untrusted enrollment initialized the daemon")
			}
		})
	}
}

func untrustedPeerCertificates(t *testing.T) ([]byte, []byte, map[string]tls.Certificate) {
	t.Helper()
	trustedCA, trustedKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	wrongCA, wrongKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	selfSigned, err := bootstrapCert("peer")
	if err != nil {
		t.Fatal(err)
	}
	return trustedCA, trustedKey, map[string]tls.Certificate{
		"self-signed": selfSigned,
		"wrong-ca":    peerCertificate(t, wrongCA, wrongKey, "peer", net.ParseIP("127.0.0.1")),
		"wrong-name":  peerCertificate(t, trustedCA, trustedKey, "different-peer"),
	}
}
