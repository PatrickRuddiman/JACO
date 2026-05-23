package grpc_test

import (
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/ca"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestNodeJoin_SignsCSRAndAddsVoter drives the full peer-join flow against
// a real daemon: Init creates a single-node cluster, we raft-apply a
// JoinTokenIssue, then submit NodeJoin as a "second node" would.
func TestNodeJoin_SignsCSRAndAddsVoter(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)

	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Wait for leadership.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Raft() == nil || !s.Raft().IsLeader() {
		t.Fatalf("never became leader")
	}

	// Mint a join token via raft.Apply.
	const tokenStr = "abcdefabcdef0123456789abcdefabcdef0123456789abcdef0123456789ffff"
	hash := sha256.Sum256([]byte(tokenStr))
	issueCmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_JoinTokenIssue{
		JoinTokenIssue: &pb.JoinTokenIssue{
			HashedSecret: hash[:],
			ExpiresAt:    timestamppb.New(time.Now().Add(time.Hour)),
		},
	}}
	issueData, _ := proto.Marshal(issueCmd)
	if _, err := s.Raft().Apply(issueData, 2*time.Second); err != nil {
		t.Fatalf("apply JoinTokenIssue: %v", err)
	}

	// Generate a node CSR as the joining peer would.
	_, csrPEM, err := ca.GenerateNodeKeypair("test-host-2")
	if err != nil {
		t.Fatalf("GenerateNodeKeypair: %v", err)
	}

	// Start a real second raft node so AddVoter can heartbeat it (without
	// this, quorum collapses and the batch Apply fails with
	// "leadership lost while committing log").
	bAddr := freePort(t)
	bDir := t.TempDir()
	bBrokers := watch.NewRegistry()
	bState := state.New(bBrokers)
	bFSM := fsm.New(bState, bBrokers)
	bRaft, err := raftnode.New(raftnode.Config{
		DataDir: bDir, BindAddr: bAddr, LocalID: "test-host-2", Bootstrap: false, FSM: bFSM, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("start node-b raft: %v", err)
	}
	t.Cleanup(func() { _ = bRaft.Shutdown() })

	resp, err := c.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name:          "test-host-2",
		JoinToken:     tokenStr,
		CsrPem:        csrPEM,
		AdvertiseAddr: bAddr,
	})
	if err != nil {
		t.Fatalf("NodeJoin: %v", err)
	}
	if len(resp.GetSignedCert()) == 0 {
		t.Errorf("signed_cert empty")
	}
	if len(resp.GetCaCert()) == 0 {
		t.Errorf("ca_cert empty")
	}
	if resp.GetClusterId() == "" {
		t.Errorf("cluster_id empty")
	}
	if len(resp.GetPeerAddrs()) == 0 {
		t.Errorf("peer_addrs empty (should include the leader)")
	}

	// State should show the joined node as READY (auto-promote in
	// NodeJoin batch — iter 15). Without READY status the scheduler
	// would skip the node and never place workloads on it.
	deadline = time.Now().Add(2 * time.Second)
	var seen bool
	for time.Now().Before(deadline) {
		for _, n := range s.State().Nodes.List() {
			if n.GetHostname() == "test-host-2" && n.GetStatus() == pb.NodeStatus_NODE_STATUS_READY {
				seen = true
				break
			}
		}
		if seen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !seen {
		t.Errorf("state.Nodes missing test-host-2 in READY status after NodeJoin")
	}

	// Token must be single-use: second call rejected.
	_, err = c.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "test-host-3", JoinToken: tokenStr, CsrPem: csrPEM, AdvertiseAddr: bAddr,
	})
	if err == nil {
		t.Errorf("second NodeJoin with same token succeeded; want join_token_consumed")
	}
}

func TestNodeJoin_RejectsPreInit(t *testing.T) {
	// Before Init, the daemon's gate intercepts everything not in
	// AllowedPreInit. NodeJoin is not in that list — should return
	// Unavailable with cluster_uninitialized.
	conn, _ := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)

	_, err := c.NodeJoin(context.Background(), &pb.NodeJoinRequest{Name: "x", JoinToken: "x", CsrPem: []byte("x"), AdvertiseAddr: "x"})
	if err == nil {
		t.Fatalf("pre-Init NodeJoin succeeded; want gated")
	}
}

func TestNodeJoin_RejectsInvalidToken(t *testing.T) {
	conn, s := startServerWithDataDir(t, t.TempDir())
	defer conn.Close()
	c := pb.NewClusterClient(conn)
	if _, err := c.Init(context.Background(), &pb.ClusterInitRequest{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Raft() != nil && s.Raft().IsLeader() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, csrPEM, _ := ca.GenerateNodeKeypair("test-host-x")
	_, err := c.NodeJoin(context.Background(), &pb.NodeJoinRequest{
		Name: "test-host-x", JoinToken: "definitely-not-a-valid-token", CsrPem: csrPEM, AdvertiseAddr: "127.0.0.1:7999",
	})
	if err == nil {
		t.Fatalf("NodeJoin with invalid token succeeded")
	}
}
