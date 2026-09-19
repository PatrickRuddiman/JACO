package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func networkEnrollmentDaemon(t *testing.T, name, dir, raftAddr, grpcAddr string) *Server {
	t.Helper()
	s, err := New(Options{
		UnixSocketPath: filepath.Join(dir, "rpc.sock"), UnixListener: bufconn.Listen(1024 * 1024),
		DataDir: dir, Hostname: name, ClusterAddr: raftAddr, ListenAddr: grpcAddr,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	return s
}

func awaitPeerState(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestVerifiedEnrollmentForwardingAndRestart(t *testing.T) {
	aDir, bDir := t.TempDir(), t.TempDir()
	aRaftAddr, bRaftAddr := unusedLoopbackAddress(t), unusedLoopbackAddress(t)
	var leader, follower *Server
	stopped := make(map[*Server]bool)
	stop := func(s *Server) {
		if s == nil || stopped[s] {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Stop(ctx)
		stopped[s] = true
	}
	t.Cleanup(func() {
		// Stop the heartbeat source before closing the non-voter's store.
		stop(leader)
		stop(follower)
	})
	leader = networkEnrollmentDaemon(t, "node-a", aDir, aRaftAddr, "127.0.0.1:0")
	follower = networkEnrollmentDaemon(t, "node-b", bDir, bRaftAddr, "127.0.0.1:0")
	aGRPCAddr, bGRPCAddr := leader.TCPAdvertiseAddr(), follower.TCPAdvertiseAddr()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := leader.cluster.Init(ctx, &pb.ClusterInitRequest{}); err != nil {
		t.Fatal(err)
	}
	awaitPeerState(t, "initialized leader", func() bool {
		return leader.Raft().IsLeader() && len(leader.State().Cluster.Get().GetCaCert()) > 0
	})
	if err := leader.publishSelf(ctx, leader.Raft(), leader.State(), "node-a", wgtypesKey{}); err != nil {
		t.Fatal(err)
	}
	issued, err := leader.cluster.IssueJoinToken(ctx, &pb.IssueJoinTokenRequest{
		NodeName: "node-b", AllowedSans: []string{"127.0.0.1", "localhost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	trustedCA, err := os.ReadFile(filepath.Join(aDir, "node", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.cluster.Join(ctx, &pb.ClusterJoinRequest{
		PeerAddr: aGRPCAddr, JoinToken: issued.GetToken(), CaCert: trustedCA,
	}); err != nil {
		t.Fatal(err)
	}
	waitForReplication := func() {
		awaitPeerState(t, "verified joined membership", func() bool {
			a, hasLeader := follower.State().Nodes.Get("node-a")
			_, hasSelf := follower.State().Nodes.Get("node-b")
			return leader.Raft().IsLeader() && !follower.Raft().IsLeader() &&
				follower.Raft().Leader() != "" && hasLeader && hasSelf && a.GetGrpcAddress() == aGRPCAddr
		})
	}
	waitForReplication()
	publish := func(value byte) {
		t.Helper()
		key := wgtypesKey{value}
		if err := follower.publishSelf(ctx, follower.Raft(), follower.State(), "node-b", key); err != nil {
			t.Fatalf("verified follower forwarding: %v", err)
		}
		awaitPeerState(t, "forwarded node update", func() bool {
			member, ok := leader.State().Nodes.Get("node-b")
			return ok && bytes.Equal(member.GetWireguardPubkey(), key[:]) && member.GetGrpcAddress() == bGRPCAddr
		})
	}
	publish(41)
	publish(42)

	conn, err := leader.dialPeer(bGRPCAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, csr, err := ca.GenerateNodeKeypair("unused-node")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	_, err = pb.NewClusterClient(conn).NodeJoin(ctx, &pb.NodeJoinRequest{
		Name: "unused-node", JoinToken: "synthetic-unused-token", CsrPem: csr,
		AdvertiseAddr: "127.0.0.1:7001",
	})
	conn.Close()
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "no_leader") {
		t.Fatalf("follower enrollment must return no_leader, not forward an administrative batch: %v", err)
	}

	stop(leader)
	stop(follower)
	leader = networkEnrollmentDaemon(t, "node-a", aDir, aRaftAddr, aGRPCAddr)
	if err := leader.OpenRaft("node-a", aRaftAddr, ""); err != nil {
		t.Fatal(err)
	}
	leader.Gate().MarkInitialized()
	follower = networkEnrollmentDaemon(t, "node-b", bDir, bRaftAddr, bGRPCAddr)
	if err := follower.OpenRaft("node-b", bRaftAddr, ""); err != nil {
		t.Fatal(err)
	}
	follower.Gate().MarkInitialized()
	waitForReplication()
	publish(43)
	hash := sha256.Sum256([]byte(issued.GetToken()))
	tok, ok := follower.State().JoinTokens.Get(hex.EncodeToString(hash[:]))
	if !ok || tok.GetConsumedAt() == nil || tok.GetNodeName() != "node-b" {
		t.Fatalf("scoped single-use enrollment did not survive restart: %v", tok)
	}
}
