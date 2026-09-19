package grpc

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestPeerTrustReloadAndCertificateRotation(t *testing.T) {
	oldCA, oldKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	newCA, newKey, err := ca.GenerateClusterCA()
	if err != nil {
		t.Fatal(err)
	}
	oldLeaf := peerCertificate(t, oldCA, oldKey, "localhost", net.ParseIP("127.0.0.1"))
	addr, peer := startRecordingPeer(t, oldLeaf, nil)
	s := peerServer(t, oldCA, oldKey, addr)
	_, port, _ := net.SplitHostPort(addr)
	dnsAddr := net.JoinHostPort("localhost", port)
	submit := func(phase, target string, allowed bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := s.dialAndSubmit(ctx, target, []byte("synthetic-private-command"))
		if (err == nil) != allowed {
			t.Errorf("%s: submit error = %v; allowed = %v", phase, err, allowed)
		}
		select {
		case received := <-peer.requests:
			req, ok := received.(*pb.SubmitRequest)
			if !allowed || !ok || !bytes.Equal(req.GetCommandBytes(), []byte("synthetic-private-command")) {
				t.Errorf("%s: unexpected remote request %v", phase, received)
			}
		default:
			if allowed {
				t.Errorf("%s: trusted peer did not receive the command", phase)
			}
		}
	}
	submit("initial IP", addr, true)
	submit("DNS reconnect", dnsAddr, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.streamDeploymentLogs(&pb.LogsRequest{Deployment: "private"}, peerLogStream{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-peer.requests:
		if _, ok := received.(*pb.LogsRequest); !ok {
			t.Fatalf("unexpected log fanout request: %v", received)
		}
	default:
		t.Fatal("trusted log fanout did not reach the peer")
	}
	conn, err := s.dialPeer(dnsAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pb.NewInternalClient(conn).EnsureSubnet(ctx, &pb.EnsureSubnetRequest{
		Deployment: "private", Network: "app", Host: "local",
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-peer.requests:
		if _, ok := received.(*pb.EnsureSubnetRequest); !ok {
			t.Fatalf("unexpected subnet request: %v", received)
		}
	default:
		t.Fatal("trusted subnet request did not reach the peer")
	}

	renewed := peerCertificate(t, oldCA, oldKey, "localhost", net.ParseIP("127.0.0.1"))
	if bytes.Equal(renewed.Certificate[0], oldLeaf.Certificate[0]) {
		t.Fatal("rotation fixture did not replace the certificate")
	}
	peer.tlsState.swap(renewed)
	submit("same-CA key and leaf renewal", addr, true)
	newLeaf := peerCertificate(t, newCA, newKey, "localhost", net.ParseIP("127.0.0.1"))
	peer.tlsState.swap(newLeaf)
	submit("unprovisioned CA rejected", addr, false)

	caPath := filepath.Join(s.dataDir, "node", "ca.crt")
	bundle := bytes.Join([][]byte{oldCA, newCA}, []byte("\n"))
	if err := os.WriteFile(caPath, bundle, 0644); err != nil {
		t.Fatal(err)
	}
	submit("independently provisioned overlapping CA bundle", dnsAddr, true)
	localLeaf := peerCertificate(t, newCA, newKey, "local", net.ParseIP("127.0.0.1"))
	writeNodeCredentials(t, s.dataDir, "local", localLeaf, newCA)
	peer.tlsState.swap(oldLeaf)
	submit("retired CA rejected", addr, false)
	peer.tlsState.swap(newLeaf)
	submit("new CA after retirement", addr, true)
}

func TestPeerSubnetRejectsUntrustedServerBeforeRequest(t *testing.T) {
	trustedCA, trustedKey, certificates := untrustedPeerCertificates(t)
	for name, cert := range certificates {
		t.Run(name, func(t *testing.T) {
			addr, peer := startRecordingPeer(t, cert, nil)
			s := peerServer(t, trustedCA, trustedKey, addr)
			conn, err := s.dialPeer(addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := pb.NewInternalClient(conn).EnsureSubnet(ctx, &pb.EnsureSubnetRequest{
				Deployment: "private", Network: "private-network", Host: "local",
			}); err == nil {
				t.Error("untrusted peer accepted")
			}
			select {
			case req := <-peer.requests:
				t.Fatalf("untrusted server received private subnet request: %v", req)
			default:
			}
		})
	}
}
