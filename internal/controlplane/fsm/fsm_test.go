package fsm_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newFSM(t *testing.T) (*fsm.FSM, *state.State, *watch.Registry) {
	t.Helper()
	brokers := watch.NewRegistry()
	s := state.New(brokers)
	return fsm.New(s, brokers), s, brokers
}

func applyCmd(t *testing.T, f *fsm.FSM, index uint64, cmd *pb.Command) {
	t.Helper()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if res := f.Apply(&hraft.Log{Index: index, Data: data}); res != nil {
		t.Fatalf("Apply at %d: %v", index, res)
	}
}

func TestApplyNodeJoinUpdatesStateBrokerAndAudit(t *testing.T) {
	f, s, brokers := newFSM(t)
	sub := brokers.Nodes.Subscribe()
	t.Cleanup(sub.Cancel)

	applyCmd(t, f, 42, &pb.Command{
		ClusterId: "cluster-x",
		Identity:  "operator",
		Ts:        timestamppb.Now(),
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{
			Hostname: "node-a",
			Address:  "10.0.0.1:7000",
		}},
	})

	// State assertion.
	n, ok := s.Nodes.Get("node-a")
	if !ok {
		t.Fatalf("node-a not present in state")
	}
	if n.GetAddress() != "10.0.0.1:7000" {
		t.Errorf("address = %q, want 10.0.0.1:7000", n.GetAddress())
	}
	if n.GetStatus() != pb.NodeStatus_NODE_STATUS_JOINING {
		t.Errorf("status = %v, want JOINING", n.GetStatus())
	}

	// Watch event assertion.
	select {
	case ev := <-sub.Events():
		if ev.Kind != watch.KindAdded {
			t.Errorf("event kind = %v, want Added", ev.Kind)
		}
		if ev.RaftIndex != 42 {
			t.Errorf("event raft_index = %d, want 42", ev.RaftIndex)
		}
		if got := ev.After.GetHostname(); got != "node-a" {
			t.Errorf("event after.hostname = %q, want node-a", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no watch event arrived")
	}

	// Audit assertion.
	audits := s.AuditEvents.List()
	if len(audits) != 1 {
		t.Fatalf("audit count = %d, want 1", len(audits))
	}
	if audits[0].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN {
		t.Errorf("audit type = %v, want NODE_JOIN", audits[0].GetType())
	}
	if audits[0].GetIdentity() != "operator" {
		t.Errorf("audit identity = %q, want operator", audits[0].GetIdentity())
	}
	if audits[0].GetRaftIndex() != 42 {
		t.Errorf("audit raft_index = %d, want 42", audits[0].GetRaftIndex())
	}
	if got := audits[0].GetPayload()["hostname"]; got != "node-a" {
		t.Errorf("audit payload[hostname] = %q, want node-a", got)
	}
}

func TestApplyTokenIssueRevokeRoundTrip(t *testing.T) {
	f, s, _ := newFSM(t)

	applyCmd(t, f, 1, &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_TokenIssue{TokenIssue: &pb.TokenIssue{
			Identity:     "alice",
			HashedSecret: []byte{0xde, 0xad},
		}},
	})

	tok, ok := s.Tokens.Get("alice")
	if !ok {
		t.Fatalf("token alice not stored")
	}
	if !bytes.Equal(tok.GetHashedSecret(), []byte{0xde, 0xad}) {
		t.Errorf("hashed secret mismatch")
	}
	if tok.GetRevokedAt() != nil {
		t.Errorf("revoked_at should be nil before revoke")
	}

	applyCmd(t, f, 2, &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_TokenRevoke{TokenRevoke: &pb.TokenRevoke{Identity: "alice"}},
	})

	tok, _ = s.Tokens.Get("alice")
	if tok.GetRevokedAt() == nil {
		t.Errorf("revoked_at should be set after revoke")
	}

	audits := s.AuditEvents.List()
	if len(audits) != 2 {
		t.Fatalf("audit count = %d, want 2", len(audits))
	}
	if audits[0].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE {
		t.Errorf("audit[0].type = %v, want TOKEN_ISSUE", audits[0].GetType())
	}
	if audits[1].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_REVOKE {
		t.Errorf("audit[1].type = %v, want TOKEN_REVOKE", audits[1].GetType())
	}
}

func TestApplyDeploymentApplyThenDeleteCascadesRoutes(t *testing.T) {
	f, s, _ := newFSM(t)

	applyCmd(t, f, 1, &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample",
			Revision:   1,
			Routes: []*pb.Route{
				{Domain: "a.example", Deployment: "sample", Service: "web", Port: 80, TlsAuto: true},
				{Domain: "b.example", Deployment: "sample", Service: "api", Port: 8080},
			},
		}},
	})

	if s.Routes.Len() != 2 {
		t.Errorf("after apply, Routes.Len = %d, want 2", s.Routes.Len())
	}
	if _, ok := s.Deployments.Get("sample"); !ok {
		t.Fatalf("deployment not stored")
	}

	applyCmd(t, f, 2, &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload:  &pb.Command_DeploymentDelete{DeploymentDelete: &pb.DeploymentDelete{Deployment: "sample"}},
	})

	if s.Routes.Len() != 0 {
		t.Errorf("after delete, Routes.Len = %d, want 0 (cascade)", s.Routes.Len())
	}
	if _, ok := s.Deployments.Get("sample"); ok {
		t.Errorf("deployment still present after delete")
	}
}

func TestApplyReplicaCounterIncrement(t *testing.T) {
	f, s, _ := newFSM(t)

	for i := 1; i <= 5; i++ {
		applyCmd(t, f, uint64(i), &pb.Command{
			Payload: &pb.Command_ReplicaCounterIncrement{
				ReplicaCounterIncrement: &pb.ReplicaCounterIncrement{
					Deployment: "sample", Service: "web",
				},
			},
		})
	}

	c, ok := s.ReplicaCounters.Get(state.ReplicaCounterKey("sample", "web"))
	if !ok {
		t.Fatalf("counter missing")
	}
	if c.GetNextIndex() != 5 {
		t.Errorf("next_index = %d, want 5 after 5 increments", c.GetNextIndex())
	}
}

func TestApplyBatchRecurses(t *testing.T) {
	f, s, _ := newFSM(t)

	batch := &pb.Command{
		Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
			{Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}}},
			{Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-b"}}},
			{Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-c"}}},
		}}},
		Identity: "operator",
		Ts:       timestamppb.Now(),
	}
	applyCmd(t, f, 10, batch)

	if got := s.Nodes.Len(); got != 3 {
		t.Errorf("Nodes.Len after batch = %d, want 3", got)
	}
}

func TestApplyUnmarshalErrorReturned(t *testing.T) {
	f, _, _ := newFSM(t)
	res := f.Apply(&hraft.Log{Index: 1, Data: []byte("not-a-protobuf")})
	if res == nil {
		t.Fatal("Apply with invalid protobuf returned nil")
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	f1, s1, _ := newFSM(t)

	// Populate every store with a representative entry.
	applyCmd(t, f1, 1, &pb.Command{
		Ts: timestamppb.Now(), Identity: "op",
		Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
			ClusterId: "cluster-x", CaCert: []byte("ca"), CaKey: []byte("key"),
			OperatorTokenHashedSecret: []byte{0x01},
		}},
	})
	applyCmd(t, f1, 2, &pb.Command{
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}},
	})
	applyCmd(t, f1, 3, &pb.Command{
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1,
			Routes: []*pb.Route{{Domain: "a.example", Deployment: "sample", Service: "web", Port: 80}},
		}},
	})
	applyCmd(t, f1, 4, &pb.Command{
		Payload: &pb.Command_SubnetAllocate{SubnetAllocate: &pb.SubnetAllocate{
			Deployment: "sample", Network: "_default", Cidr: "10.42.0.0/24", Host: "node-a",
		}},
	})

	// Take snapshot.
	snap, err := f1.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := newRecordingSink()
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Restore into a fresh FSM and assert state matches.
	f2, s2, _ := newFSM(t)
	if err := f2.Restore(io.NopCloser(bytes.NewReader(sink.data.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := s2.Cluster.Get().GetClusterId(); got != "cluster-x" {
		t.Errorf("restored cluster_id = %q", got)
	}
	if _, ok := s2.Nodes.Get("node-a"); !ok {
		t.Errorf("restored node-a missing")
	}
	if _, ok := s2.Deployments.Get("sample"); !ok {
		t.Errorf("restored deployment missing")
	}
	if _, ok := s2.Routes.Get("a.example"); !ok {
		t.Errorf("restored route missing")
	}
	if _, ok := s2.Subnets.Get(state.SubnetKey("sample", "_default", "node-a")); !ok {
		t.Errorf("restored subnet missing")
	}
	_ = s1 // silence unused (kept for clarity in the test setup)
}

// recordingSink is a minimal hraft.SnapshotSink that just buffers writes.
type recordingSink struct {
	data *bytes.Buffer
}

func newRecordingSink() *recordingSink               { return &recordingSink{data: &bytes.Buffer{}} }
func (s *recordingSink) Write(p []byte) (int, error) { return s.data.Write(p) }
func (s *recordingSink) Close() error                { return nil }
func (s *recordingSink) ID() string                  { return "test" }
func (s *recordingSink) Cancel() error               { return nil }
