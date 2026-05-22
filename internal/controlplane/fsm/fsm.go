// Package fsm implements raft.FSM on top of the typed entity stores in
// internal/controlplane/state. Apply decodes a pb.Command from each raft log
// entry, dispatches to the matching state mutation, and lets the store fan
// the typed watch event out to subscribers. AuditEvents are appended for the
// closed set of user-visible mutations.
package fsm

import (
	"encoding/hex"
	"fmt"
	"strconv"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

// FSM is the raft.FSM implementation backed by *state.State and the broker
// registry. Construct once at daemon start; pass to raftnode.Config.FSM.
type FSM struct {
	State   *state.State
	Brokers *watch.Registry
}

// New constructs an FSM wired to existing state + brokers.
func New(s *state.State, b *watch.Registry) *FSM {
	return &FSM{State: s, Brokers: b}
}

// Apply decodes the raft log entry into a pb.Command and dispatches by oneof
// payload type. Returns a non-nil error from the switch on unmarshal failure
// or unknown variant; raft surfaces this back to the Apply caller.
func (f *FSM) Apply(log *hraft.Log) interface{} {
	cmd := &pb.Command{}
	if err := proto.Unmarshal(log.Data, cmd); err != nil {
		return fmt.Errorf("fsm: unmarshal command at index %d: %w", log.Index, err)
	}
	idx := log.Index

	auditType, auditPayload := f.applyPayload(cmd, idx)
	if auditType != pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED {
		f.State.AuditEvents.Append(&pb.AuditEvent{
			RaftIndex: idx,
			Type:      auditType,
			Identity:  cmd.GetIdentity(),
			Ts:        cmd.GetTs(),
			Payload:   auditPayload,
		})
	}
	return nil
}

// applyPayload mutates state according to cmd's oneof variant and returns the
// audit event type + payload to record for this mutation (or UNSPECIFIED if
// the variant has no user-visible audit entry).
func (f *FSM) applyPayload(cmd *pb.Command, idx uint64) (pb.AuditEventType, map[string]string) {
	switch p := cmd.Payload.(type) {

	case *pb.Command_ClusterInit:
		ci := p.ClusterInit
		f.State.Cluster.Set(&pb.ClusterMeta{
			ClusterId: ci.GetClusterId(),
			CaCert:    ci.GetCaCert(),
			CaKey:     ci.GetCaKey(),
			CreatedAt: cmd.GetTs(),
		}, idx)
		if len(ci.GetOperatorTokenHashedSecret()) > 0 {
			f.State.Tokens.Apply(&pb.Token{
				Identity:     "bootstrap",
				HashedSecret: ci.GetOperatorTokenHashedSecret(),
				IssuedAt:     cmd.GetTs(),
			}, idx)
		}
		if h := ci.GetSelfHostname(); h != "" {
			f.State.Nodes.Apply(&pb.Node{
				Hostname: h,
				Address:  ci.GetSelfAddress(),
				Status:   pb.NodeStatus_NODE_STATUS_READY,
				JoinedAt: cmd.GetTs(),
			}, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, map[string]string{
			"cluster_id": ci.GetClusterId(),
		}

	case *pb.Command_NodeJoin:
		nj := p.NodeJoin
		f.State.Nodes.Apply(&pb.Node{
			Hostname:              nj.GetHostname(),
			Address:               nj.GetAddress(),
			ServerCertFingerprint: nj.GetServerCertFingerprint(),
			WireguardPubkey:       nj.GetWireguardPubkey(),
			Status:                pb.NodeStatus_NODE_STATUS_JOINING,
			JoinedAt:              cmd.GetTs(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, map[string]string{
			"hostname": nj.GetHostname(),
		}

	case *pb.Command_NodeRemove:
		f.State.Nodes.Remove(p.NodeRemove.GetHostname(), idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_REMOVE, map[string]string{
			"hostname": p.NodeRemove.GetHostname(),
		}

	case *pb.Command_NodeStatusUpdate:
		u := p.NodeStatusUpdate
		if node, ok := f.State.Nodes.Get(u.GetHostname()); ok {
			node.Status = u.GetStatus()
			node.StatusDetails = u.GetDetails()
			f.State.Nodes.Apply(node, idx)
		}
		if u.GetStatus() == pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE {
			return pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE, map[string]string{
				"hostname": u.GetHostname(),
			}
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_DeploymentApply:
		da := p.DeploymentApply
		var prevRev uint64
		if existing, ok := f.State.Deployments.Get(da.GetDeployment()); ok {
			prevRev = existing.GetAppliedRevision()
		}
		f.State.Deployments.Apply(&pb.Deployment{
			Name:             da.GetDeployment(),
			AppliedRevision:  da.GetRevision(),
			PreviousRevision: prevRev,
			Status:           pb.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE,
			JacoYaml:         da.GetJacoYaml(),
			ComposeYaml:      da.GetComposeYaml(),
			Services:         da.GetServices(),
			UpdatedAt:        cmd.GetTs(),
		}, idx)
		for _, r := range da.GetRoutes() {
			f.State.Routes.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, map[string]string{
			"deployment": da.GetDeployment(),
			"revision":   strconv.FormatUint(da.GetRevision(), 10),
		}

	case *pb.Command_DeploymentRollback:
		// Restore semantics (re-derive ReplicaDesired/Routes from a previous
		// revision) lands in task 14; here we just record the audit event.
		return pb.AuditEventType_AUDIT_EVENT_TYPE_ROLLBACK, map[string]string{
			"deployment": p.DeploymentRollback.GetDeployment(),
			"revision":   strconv.FormatUint(p.DeploymentRollback.GetRevision(), 10),
		}

	case *pb.Command_DeploymentDelete:
		name := p.DeploymentDelete.GetDeployment()
		for _, r := range f.State.Routes.List() {
			if r.GetDeployment() == name {
				f.State.Routes.Remove(r.GetDomain(), idx)
			}
		}
		for _, rd := range f.State.ReplicasDesired.List() {
			if rd.GetDeployment() == name {
				f.State.ReplicasDesired.Remove(rd.GetId(), idx)
			}
		}
		f.State.Deployments.Remove(name, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_DELETE, map[string]string{
			"deployment": name,
		}

	case *pb.Command_DeploymentStatusUpdate:
		u := p.DeploymentStatusUpdate
		if d, ok := f.State.Deployments.Get(u.GetDeployment()); ok {
			d.Status = u.GetStatus()
			d.StatusDetails = u.GetDetails()
			f.State.Deployments.Apply(d, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ReplicaDesiredUpsert:
		r := p.ReplicaDesiredUpsert.GetReplica()
		if r != nil {
			r.RaftIndex = idx
			f.State.ReplicasDesired.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ReplicaDesiredRemove:
		f.State.ReplicasDesired.Remove(p.ReplicaDesiredRemove.GetId(), idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ReplicaObservedUpdate:
		if r := p.ReplicaObservedUpdate.GetReplica(); r != nil {
			f.State.ReplicasObserved.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ReplicaCommandIssue:
		// ReplicaCommand is a ledger entry consumed by runtime/ingress via a
		// dedicated topic; task 23 wires that consumer. No store mutation here.
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_RolloutPlanUpdate:
		if plan := p.RolloutPlanUpdate.GetPlan(); plan != nil {
			f.State.RolloutPlans.Apply(plan, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ReplicaCounterIncrement:
		rc := p.ReplicaCounterIncrement
		var next uint64 = 1
		if existing, ok := f.State.ReplicaCounters.Get(state.ReplicaCounterKey(rc.GetDeployment(), rc.GetService())); ok {
			next = existing.GetNextIndex() + 1
		}
		f.State.ReplicaCounters.Apply(&pb.ReplicaCounter{
			Deployment: rc.GetDeployment(),
			Service:    rc.GetService(),
			NextIndex:  next,
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_RouteUpsert:
		if r := p.RouteUpsert.GetRoute(); r != nil {
			f.State.Routes.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_RouteRemove:
		f.State.Routes.Remove(p.RouteRemove.GetDomain(), idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertStore:
		// Key namespace (`cert/<domain>/...` vs `challenge/<token>`) is
		// interpreted by the storage layer in task 33. The FSM holds it as an
		// opaque blob until that task decodes by prefix.
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertLock:
		l := p.CertLock
		if c, ok := f.State.Certs.Get(l.GetName()); ok {
			c.Lessee = l.GetLessee()
			c.LockUntil = l.GetUntil()
			f.State.Certs.Apply(c, idx)
		} else {
			f.State.Certs.Apply(&pb.Cert{
				Domain:    l.GetName(),
				Lessee:    l.GetLessee(),
				LockUntil: l.GetUntil(),
			}, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertUnlock:
		if c, ok := f.State.Certs.Get(p.CertUnlock.GetName()); ok {
			c.Lessee = ""
			c.LockUntil = nil
			f.State.Certs.Apply(c, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_ChallengeTokenStore:
		if t := p.ChallengeTokenStore.GetToken(); t != nil {
			f.State.ChallengeTokens.Apply(t, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_SubnetAllocate:
		sa := p.SubnetAllocate
		f.State.Subnets.Apply(&pb.Subnet{
			Deployment:  sa.GetDeployment(),
			Network:     sa.GetNetwork(),
			Cidr:        sa.GetCidr(),
			AllocatedAt: cmd.GetTs(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_SubnetFree:
		sf := p.SubnetFree
		f.State.Subnets.Remove(state.SubnetKey(sf.GetDeployment(), sf.GetNetwork()), idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_TokenIssue:
		ti := p.TokenIssue
		f.State.Tokens.Apply(&pb.Token{
			Identity:     ti.GetIdentity(),
			HashedSecret: ti.GetHashedSecret(),
			IssuedAt:     cmd.GetTs(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE, map[string]string{
			"identity": ti.GetIdentity(),
		}

	case *pb.Command_TokenRevoke:
		tr := p.TokenRevoke
		if t, ok := f.State.Tokens.Get(tr.GetIdentity()); ok {
			t.RevokedAt = cmd.GetTs()
			f.State.Tokens.Apply(t, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_REVOKE, map[string]string{
			"identity": tr.GetIdentity(),
		}

	case *pb.Command_JoinTokenIssue:
		ji := p.JoinTokenIssue
		f.State.JoinTokens.Apply(&pb.JoinToken{
			HashedSecret: ji.GetHashedSecret(),
			IssuedAt:     cmd.GetTs(),
			ExpiresAt:    ji.GetExpiresAt(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_JoinTokenConsume:
		key := hex.EncodeToString(p.JoinTokenConsume.GetHashedSecret())
		if t, ok := f.State.JoinTokens.Get(key); ok {
			t.ConsumedAt = cmd.GetTs()
			f.State.JoinTokens.Apply(t, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_AuditAppend:
		// AuditAppend carries its own typed AuditEvent (used for cert lifecycle
		// events, upgrade outcomes, isolation reconciles — emitted by ingress,
		// packaging, and discovery). Stamp the current raft index and append
		// directly; bypass the auditType return path so we don't double-write.
		if ev := p.AuditAppend.GetEvent(); ev != nil {
			ev.RaftIndex = idx
			f.State.AuditEvents.Append(ev)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_Batch:
		for _, child := range p.Batch.GetChildren() {
			if child == nil {
				continue
			}
			childBytes, err := proto.Marshal(child)
			if err != nil {
				continue
			}
			f.Apply(&hraft.Log{Index: idx, Data: childBytes})
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
	}

	// Unknown / missing payload — recorded by returning UNSPECIFIED; no audit
	// event but Apply returns nil so raft doesn't surface this as a hard
	// error. Future Command variants must be added explicitly above.
	return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
}
