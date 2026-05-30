package fsm

import (
	"fmt"
	"io"

	hraft "github.com/hashicorp/raft"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

// Snapshot captures the full control-plane state as a single proto-encoded
// blob, then writes it to the snapshot sink in Persist.
func (f *FSM) Snapshot() (hraft.FSMSnapshot, error) {
	snap := &pb.FSMSnapshot{
		Cluster:             f.State.Cluster.Get(),
		Nodes:               f.State.Nodes.List(),
		Deployments:         f.State.Deployments.List(),
		ReplicasDesired:     f.State.ReplicasDesired.List(),
		ReplicasObserved:    f.State.ReplicasObserved.List(),
		Routes:              f.State.Routes.List(),
		TcpRoutes:           f.State.TCPRoutes.List(),
		Certs:               f.State.Certs.List(),
		CertBlobs:           f.State.CertBlobs.List(),
		ChallengeTokens:     f.State.ChallengeTokens.List(),
		Tokens:              f.State.Tokens.List(),
		JoinTokens:          f.State.JoinTokens.List(),
		Subnets:             f.State.Subnets.List(),
		RolloutPlans:        f.State.RolloutPlans.List(),
		ReplicaCounters:     f.State.ReplicaCounters.List(),
		RestartCounters:     f.State.RestartCounters.List(),
		AuditEvents:         f.State.AuditEvents.List(),
		RegistryCredentials: f.State.RegistryCredentials.List(),
	}
	data, err := proto.Marshal(snap)
	if err != nil {
		f.log().Error("snapshot marshal failed", "error", err)
		return nil, fmt.Errorf("fsm: marshal snapshot: %w", err)
	}
	f.log().Info("snapshot taken", "bytes", len(data), "nodes", len(snap.GetNodes()), "deployments", len(snap.GetDeployments()))
	return &fsmSnapshot{data: data}, nil
}

// Restore replaces the entire State by re-applying every entity from the
// snapshot blob at raft index 0. Subscribers (if any are attached at this
// point — typically there are none during cold restore) will see Added events
// per entity or Resync on overflow; both are correct.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("fsm: read snapshot: %w", err)
	}
	snap := &pb.FSMSnapshot{}
	if err := proto.Unmarshal(data, snap); err != nil {
		// Corruption / wire-incompatible snapshot — ERROR (state divergence).
		f.log().Error("snapshot unmarshal failed (state may be corrupt)", "bytes", len(data), "error", err)
		return fmt.Errorf("fsm: unmarshal snapshot: %w", err)
	}
	f.log().Info("restoring state from snapshot", "bytes", len(data))

	if c := snap.GetCluster(); c != nil {
		f.State.Cluster.Set(c, 0)
	}
	for _, v := range snap.GetNodes() {
		f.State.Nodes.Apply(v, 0)
	}
	for _, v := range snap.GetDeployments() {
		f.State.Deployments.Apply(v, 0)
	}
	for _, v := range snap.GetReplicasDesired() {
		f.State.ReplicasDesired.Apply(v, 0)
	}
	for _, v := range snap.GetReplicasObserved() {
		f.State.ReplicasObserved.Apply(v, 0)
	}
	for _, v := range snap.GetRoutes() {
		f.State.Routes.Apply(v, 0)
	}
	for _, v := range snap.GetTcpRoutes() {
		f.State.TCPRoutes.Apply(v, 0)
	}
	for _, v := range snap.GetCerts() {
		f.State.Certs.Apply(v, 0)
	}
	for _, v := range snap.GetCertBlobs() {
		f.State.CertBlobs.Apply(v, 0)
	}
	for _, v := range snap.GetChallengeTokens() {
		f.State.ChallengeTokens.Apply(v, 0)
	}
	for _, v := range snap.GetTokens() {
		f.State.Tokens.Apply(v, 0)
	}
	for _, v := range snap.GetJoinTokens() {
		f.State.JoinTokens.Apply(v, 0)
	}
	for _, v := range snap.GetSubnets() {
		f.State.Subnets.Apply(v, 0)
	}
	for _, v := range snap.GetRolloutPlans() {
		f.State.RolloutPlans.Apply(v, 0)
	}
	for _, v := range snap.GetReplicaCounters() {
		f.State.ReplicaCounters.Apply(v, 0)
	}
	for _, v := range snap.GetRestartCounters() {
		f.State.RestartCounters.Apply(v, 0)
	}
	for _, v := range snap.GetAuditEvents() {
		f.State.AuditEvents.Append(v)
	}
	for _, v := range snap.GetRegistryCredentials() {
		f.State.RegistryCredentials.Apply(v, 0)
	}
	return nil
}

type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink hraft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
