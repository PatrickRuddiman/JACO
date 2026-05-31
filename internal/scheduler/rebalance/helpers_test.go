package rebalance_test

import (
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeLeader satisfies scheduler.LeaderStatus and lets tests flip
// leadership on demand. Defaults to leader=true so the common case
// (drive a cycle, observe) needs no setup.
type fakeLeader struct{ leader bool }

func (f *fakeLeader) IsLeader() bool { return f.leader }

// fakeSource is an in-memory PressureSource. Tests configure node
// snapshots + per-replica footprints; the rebalancer reads them
// verbatim. Concurrent-safe so tests that drive multiple cycles in a
// row from one goroutine don't race the Run-loop's reads.
type fakeSource struct {
	mu         sync.RWMutex
	snapshots  map[string]rebalance.Snapshot
	footprints map[string]rebalance.Footprint
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		snapshots:  map[string]rebalance.Snapshot{},
		footprints: map[string]rebalance.Footprint{},
	}
}

func (s *fakeSource) NodePressure(host string) (rebalance.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[host]
	return snap, ok
}

func (s *fakeSource) ReplicaFootprint(id string) rebalance.Footprint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.footprints[id]
}

func (s *fakeSource) setNode(host string, snap rebalance.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[host] = snap
}

func (s *fakeSource) setReplica(id string, fp rebalance.Footprint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.footprints[id] = fp
}

// rig wires state + FSM + a leader=true rebalancer with a fake clock
// and fake source. Tests drive Cycle() directly; the captured applier
// records every Apply for assertions on commits + audits.
type rig struct {
	t       *testing.T
	state   *state.State
	fsm     *fsm.FSM
	leader  *fakeLeader
	source  *fakeSource
	rebal   *rebalance.Rebalancer
	now     time.Time
	applies [][]byte
	raftIdx uint64
	applyMu sync.Mutex
}

// newRig builds the standard test rig. cfg is the rebalancer config
// (use rebalance.DefaultConfig() then override what you need).
func newRig(t *testing.T, cfg rebalance.Config) *rig {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	leader := &fakeLeader{leader: true}
	src := newFakeSource()
	r := &rig{
		t:      t,
		state:  st,
		fsm:    f,
		leader: leader,
		source: src,
		now:    time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
	}
	apply := func(data []byte) error {
		r.applyMu.Lock()
		r.applies = append(r.applies, append([]byte(nil), data...))
		r.applyMu.Unlock()
		r.raftIdx++
		f.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
		return nil
	}
	r.rebal = rebalance.New(st, leader, apply, src, cfg)
	r.rebal.SetClock(func() time.Time { return r.now })
	return r
}

// advance moves the rig's clock forward by d. The fakeSource is
// unaffected — callers update node snapshots explicitly when they
// want to model a changing pressure landscape.
func (r *rig) advance(d time.Duration) {
	r.now = r.now.Add(d)
}

// seedNode inserts a NODE_STATUS_READY node directly into state.
func (r *rig) seedNode(host string) {
	r.t.Helper()
	r.raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeJoin{
		NodeJoin: &pb.NodeJoin{Hostname: host, Address: host + ":7000"},
	}}
	data, _ := proto.Marshal(cmd)
	r.fsm.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
	r.raftIdx++
	upd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_NodeStatusUpdate{
		NodeStatusUpdate: &pb.NodeStatusUpdate{
			Hostname: host, Status: pb.NodeStatus_NODE_STATUS_READY,
		},
	}}
	data, _ = proto.Marshal(upd)
	r.fsm.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
}

// seedDeployment inserts a deployment with one SPREAD service ("web")
// and the given number of replicas. Tests that need a different
// placement mode call seedDeploymentMode instead.
func (r *rig) seedDeployment(name string, replicas int32) {
	r.t.Helper()
	r.seedDeploymentMode(name, replicas, pb.ServiceSpec_PLACEMENT_MODE_SPREAD)
}

// seedDeploymentMode is the explicit form: caller picks the placement
// mode. Used by tests that exercise the rebalancer with PACK so the
// anti-affinity gate isn't an accidental blocker.
func (r *rig) seedDeploymentMode(name string, replicas int32, mode pb.ServiceSpec_PlacementMode) {
	r.t.Helper()
	r.raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_DeploymentApply{
		DeploymentApply: &pb.DeploymentApply{
			Deployment: name, Revision: 1,
			ComposeYaml: []byte("services:\n  web:\n    image: nginx:1.27\n"),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: replicas,
				Placement: mode,
			}},
		},
	}}
	data, _ := proto.Marshal(cmd)
	r.fsm.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
}

// seedReplica inserts a ReplicaDesired directly via the FSM.
func (r *rig) seedReplica(id, deployment, service, host string, index int32) {
	r.t.Helper()
	r.raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaDesiredUpsert{
		ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
			Replica: &pb.ReplicaDesired{
				Id: id, Deployment: deployment, Service: service,
				Host: host, Index: index, Image: "nginx:1.27",
			},
		},
	}}
	data, _ := proto.Marshal(cmd)
	r.fsm.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
}

// seedObserved marks a replica as RUNNING from the observed side.
// Kept on the rig because some tests still want to populate the
// observed table for completeness even though the rebalancer no
// longer cross-references it (the v0 quorum check is gone).
func (r *rig) seedObserved(id, host string) {
	r.t.Helper()
	r.raftIdx++
	cmd := &pb.Command{Ts: timestamppb.Now(), Payload: &pb.Command_ReplicaObservedUpdate{
		ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
			Replica: &pb.ReplicaObserved{
				Id: id, Host: host,
				State: pb.ReplicaState_REPLICA_STATE_RUNNING,
			},
		},
	}}
	data, _ := proto.Marshal(cmd)
	r.fsm.Apply(&hraft.Log{Index: r.raftIdx, Data: data})
}

// auditEvents returns every AuditEvent in state.AuditEvents (most
// commits go through the rig's applier so they land in state).
func (r *rig) auditEvents() []*pb.AuditEvent {
	return r.state.AuditEvents.List()
}

// rebalanceAuditsByType filters auditEvents() to those whose Type
// matches one of the given REBALANCE_* events.
func (r *rig) rebalanceAuditsByType(types ...pb.AuditEventType) []*pb.AuditEvent {
	wanted := map[pb.AuditEventType]bool{}
	for _, t := range types {
		wanted[t] = true
	}
	var out []*pb.AuditEvent
	for _, ev := range r.auditEvents() {
		if wanted[ev.GetType()] {
			out = append(out, ev)
		}
	}
	return out
}

// replicaHost returns the current Host of a ReplicaDesired by id, or
// "" if the replica was removed (the rebalancer never removes; this
// is just a guard against tests asking about a typoed id).
func (r *rig) replicaHost(id string) string {
	rep, ok := r.state.ReplicasDesired.Get(id)
	if !ok {
		return ""
	}
	return rep.GetHost()
}

// pbAuditSkipped is a short alias for the audit event enum value so test rows
// stay readable.
func pbAuditSkipped() pb.AuditEventType { return pb.AuditEventType_AUDIT_EVENT_TYPE_REBALANCE_SKIPPED }
