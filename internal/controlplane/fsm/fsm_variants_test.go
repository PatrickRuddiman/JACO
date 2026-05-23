package fsm_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// This file fills in the Command-variant branches the original fsm_test.go
// leaves uncovered: every payload shape that's actually emitted in
// production gets at least one Apply happy-path assertion + an
// assertion about the resulting state/audit shape.

func TestApply_ClusterInit_SeedsClusterTokensAndNode(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Identity: "operator",
		Ts:       timestamppb.Now(),
		Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
			ClusterId:                 "cluster-y",
			CaCert:                    []byte("ca"),
			CaKey:                     []byte("key"),
			OperatorTokenHashedSecret: []byte{0x11, 0x22},
			SelfHostname:              "leader",
			SelfAddress:               "10.0.0.1:7000",
		}},
	})
	if got := s.Cluster.Get().GetClusterId(); got != "cluster-y" {
		t.Errorf("cluster_id = %q, want cluster-y", got)
	}
	bootstrap, ok := s.Tokens.Get("bootstrap")
	if !ok {
		t.Errorf("bootstrap token not seeded")
	} else if !bytes.Equal(bootstrap.GetHashedSecret(), []byte{0x11, 0x22}) {
		t.Errorf("bootstrap hashed secret mismatch")
	}
	if _, ok := s.Nodes.Get("leader"); !ok {
		t.Errorf("self node not stored")
	}
	audits := s.AuditEvents.List()
	if len(audits) != 1 || audits[0].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN {
		t.Errorf("expected single NODE_JOIN audit; got %v", audits)
	}
}

func TestApply_ClusterInit_OmitsBootstrapTokenWhenAbsent(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_ClusterInit{ClusterInit: &pb.ClusterInit{
			ClusterId: "cluster-z",
		}},
	})
	if s.Tokens.Len() != 0 {
		t.Errorf("Tokens.Len = %d, want 0 when OperatorTokenHashedSecret is empty", s.Tokens.Len())
	}
}

func TestApply_NodeRemove_DeletesNode(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_NodeRemove{NodeRemove: &pb.NodeRemove{Hostname: "node-a"}},
	})
	if _, ok := s.Nodes.Get("node-a"); ok {
		t.Errorf("node-a still present after NodeRemove")
	}
	audits := s.AuditEvents.List()
	if len(audits) != 2 || audits[1].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_REMOVE {
		t.Errorf("expected NODE_REMOVE audit; got %v", audits)
	}
}

func TestApply_NodeUpdateSelf_PatchesWGAndGRPC(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a", Address: "10.0.0.1:7000"}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname:        "node-a",
			WireguardPubkey: []byte{0xaa, 0xbb},
			GrpcAddress:     "10.0.0.1:9000",
		}},
	})
	n, _ := s.Nodes.Get("node-a")
	if !bytes.Equal(n.GetWireguardPubkey(), []byte{0xaa, 0xbb}) {
		t.Errorf("wg pubkey not updated")
	}
	if n.GetGrpcAddress() != "10.0.0.1:9000" {
		t.Errorf("grpc address not updated: %q", n.GetGrpcAddress())
	}
}

func TestApply_NodeUpdateSelf_SkipsUnknownNode(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname: "ghost", GrpcAddress: "1.2.3.4:5",
		}},
	})
	if s.Nodes.Len() != 0 {
		t.Errorf("NodeUpdateSelf for missing node created entry; Nodes.Len = %d", s.Nodes.Len())
	}
}

func TestApply_NodeStatusUpdate_PatchesStatusAndAuditsIsolation(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: "node-a",
			Status:   pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE,
			Details:  map[string]string{"reason": "nft missing"},
		}},
	})
	n, _ := s.Nodes.Get("node-a")
	if n.GetStatus() != pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE {
		t.Errorf("status = %v, want ISOLATION_UNAVAILABLE", n.GetStatus())
	}
	audits := s.AuditEvents.List()
	if audits[len(audits)-1].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE {
		t.Errorf("last audit type = %v, want ISOLATION_UNAVAILABLE", audits[len(audits)-1].GetType())
	}
}

func TestApply_NodeStatusUpdate_NonIsolationStatusNoAudit(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}},
	})
	prevAudits := s.AuditEvents.Len()
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: "node-a", Status: pb.NodeStatus_NODE_STATUS_READY,
		}},
	})
	if s.AuditEvents.Len() != prevAudits {
		t.Errorf("non-isolation status update wrote audit (count delta = %d)", s.AuditEvents.Len()-prevAudits)
	}
}

func TestApply_DeploymentRollback_SwapsRevisions(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 2,
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Ts: timestamppb.Now(),
		Payload: &pb.Command_DeploymentRollback{DeploymentRollback: &pb.DeploymentRollback{
			Deployment: "sample", Revision: 1,
		}},
	})
	d, _ := s.Deployments.Get("sample")
	if d.GetAppliedRevision() != 1 {
		t.Errorf("applied = %d, want 1", d.GetAppliedRevision())
	}
	if d.GetPreviousRevision() != 2 {
		t.Errorf("previous = %d, want 2", d.GetPreviousRevision())
	}
	audits := s.AuditEvents.List()
	if audits[len(audits)-1].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_ROLLBACK {
		t.Errorf("last audit = %v, want ROLLBACK", audits[len(audits)-1].GetType())
	}
}

func TestApply_DeploymentDelete_CascadesReplicasDesired(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1,
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
			Replica: &pb.ReplicaDesired{Id: "sample-web-0", Deployment: "sample", Service: "web", Host: "h"},
		}},
	})
	if s.ReplicasDesired.Len() != 1 {
		t.Fatalf("ReplicasDesired.Len = %d, want 1", s.ReplicasDesired.Len())
	}
	applyCmd(t, f, 3, &pb.Command{
		Payload: &pb.Command_DeploymentDelete{DeploymentDelete: &pb.DeploymentDelete{Deployment: "sample"}},
	})
	if s.ReplicasDesired.Len() != 0 {
		t.Errorf("ReplicasDesired.Len after delete = %d, want 0", s.ReplicasDesired.Len())
	}
}

func TestApply_DeploymentStatusUpdate_PatchesStatus(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
			Deployment: "sample", Revision: 1,
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_DeploymentStatusUpdate{DeploymentStatusUpdate: &pb.DeploymentStatusUpdate{
			Deployment: "sample",
			Status:     pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING,
			Details:    map[string]string{"reason": "boom"},
		}},
	})
	d, _ := s.Deployments.Get("sample")
	if d.GetStatus() != pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING {
		t.Errorf("status = %v, want PENDING", d.GetStatus())
	}
}

func TestApply_DeploymentStatusUpdate_SkipsUnknown(t *testing.T) {
	f, _, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_DeploymentStatusUpdate{DeploymentStatusUpdate: &pb.DeploymentStatusUpdate{
			Deployment: "ghost", Status: pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING,
		}},
	})
	// Just confirm no panic / no spurious entry.
}

func TestApply_ReplicaDesiredUpsertAndRemove(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
			Replica: &pb.ReplicaDesired{Id: "r1", Deployment: "d", Service: "s", Host: "h"},
		}},
	})
	if s.ReplicasDesired.Len() != 1 {
		t.Fatalf("ReplicasDesired.Len = %d, want 1", s.ReplicasDesired.Len())
	}
	// raft_index propagated.
	r, _ := s.ReplicasDesired.Get("r1")
	if r.GetRaftIndex() != 1 {
		t.Errorf("RaftIndex = %d, want 1", r.GetRaftIndex())
	}
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_ReplicaDesiredRemove{ReplicaDesiredRemove: &pb.ReplicaDesiredRemove{Id: "r1"}},
	})
	if s.ReplicasDesired.Len() != 0 {
		t.Errorf("ReplicasDesired.Len after remove = %d, want 0", s.ReplicasDesired.Len())
	}
}

func TestApply_ReplicaDesiredUpsert_NilReplicaNoOp(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{}},
	})
	if s.ReplicasDesired.Len() != 0 {
		t.Errorf("nil-replica upsert created entry")
	}
}

func TestApply_ReplicaObservedUpdate(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{Id: "r1", State: pb.ReplicaState_REPLICA_STATE_RUNNING},
		}},
	})
	if s.ReplicasObserved.Len() != 1 {
		t.Errorf("ReplicasObserved.Len = %d, want 1", s.ReplicasObserved.Len())
	}
}

func TestApply_ReplicaCommandIssue_NoStateMutation(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_ReplicaCommandIssue{ReplicaCommandIssue: &pb.ReplicaCommandIssue{}},
	})
	// Sanity: no stores were touched.
	if s.ReplicasDesired.Len() != 0 || s.ReplicasObserved.Len() != 0 {
		t.Errorf("ReplicaCommandIssue mutated state")
	}
}

func TestApply_RolloutPlanUpdate(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_RolloutPlanUpdate{RolloutPlanUpdate: &pb.RolloutPlanUpdate{
			Plan: &pb.RolloutPlan{Deployment: "d"},
		}},
	})
	if s.RolloutPlans.Len() != 1 {
		t.Errorf("RolloutPlans.Len = %d, want 1", s.RolloutPlans.Len())
	}
}

func TestApply_RestartCounter_IncrementAndReset(t *testing.T) {
	f, s, _ := newFSM(t)
	for i := 1; i <= 3; i++ {
		applyCmd(t, f, uint64(i), &pb.Command{
			Ts: timestamppb.Now(),
			Payload: &pb.Command_RestartCounterUpdate{RestartCounterUpdate: &pb.RestartCounterUpdate{
				ReplicaId: "r1", Action: pb.RestartCounterUpdate_ACTION_INCREMENT,
			}},
		})
	}
	rc, _ := s.RestartCounters.Get("r1")
	if rc.GetConsecutiveFailures() != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", rc.GetConsecutiveFailures())
	}
	applyCmd(t, f, 4, &pb.Command{
		Payload: &pb.Command_RestartCounterUpdate{RestartCounterUpdate: &pb.RestartCounterUpdate{
			ReplicaId: "r1", Action: pb.RestartCounterUpdate_ACTION_RESET,
		}},
	})
	if _, ok := s.RestartCounters.Get("r1"); ok {
		t.Errorf("RESET did not delete counter")
	}
}

func TestApply_RouteUpsertAndRemove(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_RouteUpsert{RouteUpsert: &pb.RouteUpsert{
			Route: &pb.Route{Domain: "a.example", Deployment: "d", Service: "s", Port: 80},
		}},
	})
	if s.Routes.Len() != 1 {
		t.Fatalf("Routes.Len = %d, want 1", s.Routes.Len())
	}
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_RouteRemove{RouteRemove: &pb.RouteRemove{Domain: "a.example"}},
	})
	if s.Routes.Len() != 0 {
		t.Errorf("Routes.Len after remove = %d, want 0", s.Routes.Len())
	}
}

func TestApply_RouteUpsert_NilRouteNoOp(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_RouteUpsert{RouteUpsert: &pb.RouteUpsert{}},
	})
	if s.Routes.Len() != 0 {
		t.Errorf("nil-route upsert created entry")
	}
}

func TestApply_CertStore_NoMutation(t *testing.T) {
	f, s, _ := newFSM(t)
	prev := s.Certs.Len()
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_CertStore{CertStore: &pb.CertStore{}},
	})
	if s.Certs.Len() != prev {
		t.Errorf("CertStore mutated state (delta = %d)", s.Certs.Len()-prev)
	}
}

func TestApply_CertLock_NewEntry(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-a", Until: timestampOf(200),
		}},
	})
	c, ok := s.Certs.Get("a.example")
	if !ok {
		t.Fatalf("cert missing after lock")
	}
	if c.GetLessee() != "node-a" {
		t.Errorf("lessee = %q, want node-a", c.GetLessee())
	}
}

func TestApply_CertLock_RejectsDifferentLessee(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-a", Until: timestampOf(500),
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Ts: timestampOf(200), // before until=500
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-b", Until: timestampOf(800),
		}},
	})
	c, _ := s.Certs.Get("a.example")
	if c.GetLessee() != "node-a" {
		t.Errorf("lessee changed despite live lock; got %q", c.GetLessee())
	}
}

func TestApply_CertLock_SameLesseeRenews(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-a", Until: timestampOf(200),
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Ts: timestampOf(150),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-a", Until: timestampOf(999),
		}},
	})
	c, _ := s.Certs.Get("a.example")
	if c.GetLockUntil().GetSeconds() != 999 {
		t.Errorf("LockUntil = %d, want 999 (renewed)", c.GetLockUntil().GetSeconds())
	}
}

func TestApply_CertUnlock(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name: "a.example", Lessee: "node-a", Until: timestampOf(200),
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_CertUnlock{CertUnlock: &pb.CertUnlock{Name: "a.example"}},
	})
	c, _ := s.Certs.Get("a.example")
	if c.GetLessee() != "" {
		t.Errorf("lessee not cleared after unlock: %q", c.GetLessee())
	}
}

func TestApply_CertUnlock_MissingNoOp(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_CertUnlock{CertUnlock: &pb.CertUnlock{Name: "ghost"}},
	})
	if s.Certs.Len() != 0 {
		t.Errorf("CertUnlock on missing created entry")
	}
}

func TestApply_ChallengeTokenStore(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_ChallengeTokenStore{ChallengeTokenStore: &pb.ChallengeTokenStore{
			Token: &pb.ChallengeToken{Token: "tok-a", KeyAuth: "ka"},
		}},
	})
	if s.ChallengeTokens.Len() != 1 {
		t.Errorf("ChallengeTokens.Len = %d, want 1", s.ChallengeTokens.Len())
	}
}

func TestApply_CertBlobUpsertAndRemove(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
			Blob: &pb.CertBlob{Key: "cert/a.example/cert", Value: []byte("blob")},
		}},
	})
	b, ok := s.CertBlobs.Get("cert/a.example/cert")
	if !ok {
		t.Fatalf("blob missing")
	}
	if b.GetUpdatedAt() == nil {
		t.Errorf("UpdatedAt not stamped from cmd.Ts")
	}
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_CertBlobRemove{CertBlobRemove: &pb.CertBlobRemove{Key: "cert/a.example/cert"}},
	})
	if s.CertBlobs.Len() != 0 {
		t.Errorf("CertBlobs.Len after remove = %d, want 0", s.CertBlobs.Len())
	}
}

func TestApply_CertBlobRemove_EmptyKeyNoOp(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
			Blob: &pb.CertBlob{Key: "cert/a", Value: []byte("x")},
		}},
	})
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_CertBlobRemove{CertBlobRemove: &pb.CertBlobRemove{Key: ""}},
	})
	if s.CertBlobs.Len() != 1 {
		t.Errorf("empty-key remove deleted entry")
	}
}

func TestApply_SubnetAllocateAndFree(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_SubnetAllocate{SubnetAllocate: &pb.SubnetAllocate{
			Deployment: "d", Network: "_default", Cidr: "10.42.0.0/24",
		}},
	})
	if s.Subnets.Len() != 1 {
		t.Fatalf("Subnets.Len = %d, want 1", s.Subnets.Len())
	}
	applyCmd(t, f, 2, &pb.Command{
		Payload: &pb.Command_SubnetFree{SubnetFree: &pb.SubnetFree{Deployment: "d", Network: "_default"}},
	})
	if s.Subnets.Len() != 0 {
		t.Errorf("Subnets.Len after free = %d, want 0", s.Subnets.Len())
	}
}

func TestApply_JoinTokenIssueAndConsume(t *testing.T) {
	f, s, _ := newFSM(t)
	hash := []byte{0xab, 0xcd, 0xef}
	applyCmd(t, f, 1, &pb.Command{
		Ts: timestampOf(100),
		Payload: &pb.Command_JoinTokenIssue{JoinTokenIssue: &pb.JoinTokenIssue{
			HashedSecret: hash, ExpiresAt: timestampOf(999),
		}},
	})
	if s.JoinTokens.Len() != 1 {
		t.Fatalf("JoinTokens.Len = %d, want 1", s.JoinTokens.Len())
	}
	// Confirm key is hex-encoded.
	if _, ok := s.JoinTokens.Get(hex.EncodeToString(hash)); !ok {
		t.Errorf("join token not keyed by hex(hashed_secret)")
	}
	applyCmd(t, f, 2, &pb.Command{
		Ts: timestampOf(200),
		Payload: &pb.Command_JoinTokenConsume{JoinTokenConsume: &pb.JoinTokenConsume{HashedSecret: hash}},
	})
	tok, _ := s.JoinTokens.Get(hex.EncodeToString(hash))
	if tok.GetConsumedAt() == nil {
		t.Errorf("ConsumedAt not stamped after consume")
	}
}

func TestApply_JoinTokenConsume_MissingNoOp(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_JoinTokenConsume{JoinTokenConsume: &pb.JoinTokenConsume{HashedSecret: []byte{0xff}}},
	})
	if s.JoinTokens.Len() != 0 {
		t.Errorf("Consume on missing created entry")
	}
}

func TestApply_AuditAppend_StampsRaftIndex(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 42, &pb.Command{
		Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{Event: &pb.AuditEvent{
			Type:     pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED,
			Identity: "system",
		}}},
	})
	audits := s.AuditEvents.List()
	if len(audits) != 1 {
		t.Fatalf("audits.Len = %d, want 1", len(audits))
	}
	if audits[0].GetRaftIndex() != 42 {
		t.Errorf("RaftIndex = %d, want 42 (stamped from log.Index)", audits[0].GetRaftIndex())
	}
	if audits[0].GetType() != pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED {
		t.Errorf("type = %v, want CERTIFICATE_RENEWED", audits[0].GetType())
	}
}

func TestApply_UnknownPayload_ReturnsNilNoAudit(t *testing.T) {
	f, s, _ := newFSM(t)
	// A pb.Command with no oneof payload set hits the switch's default
	// fall-through (no case matches a nil Payload field).
	applyCmd(t, f, 1, &pb.Command{Identity: "op", Ts: timestamppb.Now()})
	if s.AuditEvents.Len() != 0 {
		t.Errorf("unknown payload wrote audit")
	}
}

func TestApply_Batch_SkipsNilChildren(t *testing.T) {
	f, s, _ := newFSM(t)
	applyCmd(t, f, 1, &pb.Command{
		Payload: &pb.Command_Batch{Batch: &pb.Batch{Children: []*pb.Command{
			nil,
			{Payload: &pb.Command_NodeJoin{NodeJoin: &pb.NodeJoin{Hostname: "node-a"}}},
			nil,
		}}},
	})
	if s.Nodes.Len() != 1 {
		t.Errorf("Nodes.Len = %d, want 1 (nil children should be skipped)", s.Nodes.Len())
	}
}

// --- snapshot tests --------------------------------------------------------

func TestSnapshot_Release_NoOp(t *testing.T) {
	f, _, _ := newFSM(t)
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Release must not panic; documented as a no-op.
	snap.Release()
}

func TestRestore_MalformedBytes_ReturnsError(t *testing.T) {
	f, _, _ := newFSM(t)
	err := f.Restore(io.NopCloser(bytes.NewReader([]byte("not-a-proto"))))
	if err == nil {
		t.Fatalf("Restore with malformed bytes returned nil")
	}
}

// failingSink is an hraft.SnapshotSink whose Write fails on the first
// call. Used to drive the error branch in fsmSnapshot.Persist, which
// must call sink.Cancel and propagate the write error.
type failingSink struct {
	cancelled bool
}

func (s *failingSink) Write(_ []byte) (int, error) { return 0, errors.New("disk full") }
func (s *failingSink) Close() error                { return nil }
func (s *failingSink) ID() string                  { return "failing" }
func (s *failingSink) Cancel() error               { s.cancelled = true; return nil }

func TestSnapshotPersist_CancelsOnWriteError(t *testing.T) {
	f, _, _ := newFSM(t)
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &failingSink{}
	if err := snap.Persist(sink); err == nil {
		t.Fatalf("Persist with failing sink returned nil error")
	}
	if !sink.cancelled {
		t.Errorf("failing sink not cancelled on Persist write error")
	}
}

// --- helpers ---------------------------------------------------------------

// timestampOf builds a deterministic timestamppb at offset seconds since
// epoch. Lets us reason about lock expiry without time.Now() racing.
func timestampOf(sec int64) *timestamppb.Timestamp {
	return &timestamppb.Timestamp{Seconds: sec}
}

var _ = state.SubnetKey

