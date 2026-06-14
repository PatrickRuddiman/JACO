// Package fsm implements raft.FSM on top of the typed entity stores in
// internal/controlplane/state. Apply decodes a pb.Command from each raft log
// entry, dispatches to the matching state mutation, and lets the store fan
// the typed watch event out to subscribers. AuditEvents are appended for the
// closed set of user-visible mutations.
package fsm

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/proto"
)

// FSM is the raft.FSM implementation backed by *state.State and the broker
// registry. Construct once at daemon start; pass to raftnode.Config.FSM.
type FSM struct {
	State   *state.State
	Brokers *watch.Registry

	// Logger logs each Apply at DEBUG (op + raft_index) and unmarshal/dispatch
	// failures at ERROR. nil → discard. Set by the daemon after construction;
	// the FSM never logs audit-event payloads (those are user-facing, separate).
	Logger *slog.Logger
}

func (f *FSM) log() *slog.Logger {
	if f.Logger == nil {
		return logging.Discard()
	}
	return f.Logger
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
		f.log().Error("fsm unmarshal command failed", "raft_index", log.Index, "error", err)
		return fmt.Errorf("fsm: unmarshal command at index %d: %w", log.Index, err)
	}
	idx := log.Index
	f.log().Debug("fsm apply", "op", commandOp(cmd), "raft_index", idx)

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
			GrpcAddress:           nj.GetGrpcAddress(),
			Status:                pb.NodeStatus_NODE_STATUS_JOINING,
			JoinedAt:              cmd.GetTs(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_JOIN, map[string]string{
			"hostname": nj.GetHostname(),
		}

	case *pb.Command_NodeRemove:
		hostname := p.NodeRemove.GetHostname()
		f.State.Nodes.Remove(hostname, idx)
		// Free every /24 the departing node owned so the pool slots return.
		for _, sn := range f.State.Subnets.List() {
			if sn.GetHost() == hostname {
				f.State.Subnets.Remove(state.SubnetKey(sn.GetDeployment(), sn.GetNetwork(), sn.GetHost()), idx)
			}
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_NODE_REMOVE, map[string]string{
			"hostname": hostname,
		}

	case *pb.Command_NodeUpdateSelf:
		us := p.NodeUpdateSelf
		if node, ok := f.State.Nodes.Get(us.GetHostname()); ok {
			if len(us.GetWireguardPubkey()) > 0 {
				node.WireguardPubkey = us.GetWireguardPubkey()
			}
			if addr := us.GetGrpcAddress(); addr != "" {
				node.GrpcAddress = addr
			}
			f.State.Nodes.Apply(node, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_NodeStatusUpdate:
		u := p.NodeStatusUpdate
		if node, ok := f.State.Nodes.Get(u.GetHostname()); ok {
			// A pressure-only heartbeat sets Status=UNSPECIFIED; in
			// that case keep the existing status + details rather
			// than clobbering them with zero values. Explicit status
			// transitions (firewall reconciler, membership) always
			// set a concrete enum and reach the patch path.
			if u.GetStatus() != pb.NodeStatus_NODE_STATUS_UNSPECIFIED {
				node.Status = u.GetStatus()
				node.StatusDetails = u.GetDetails()
			}
			// Pressure fields only patch when the heartbeater
			// flagged include_pressure=true — otherwise a status-only
			// command (firewall reconciler, membership) would clobber
			// the latest pressure sample with zeros. Absence here
			// becomes !ok at read time via LastPressureAt freshness.
			if u.GetIncludePressure() {
				node.CpuPressure = u.GetCpuPressure()
				node.MemoryPressure = u.GetMemoryPressure()
				node.LastPressureAt = cmd.GetTs()
			}
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
			AcmeEmail:        da.GetAcmeEmail(),
			UpdatedAt:        cmd.GetTs(),
		}, idx)
		// Replace-set the deployment's routes: prune any the new revision no
		// longer declares, then apply the desired set. Upsert-only would leave a
		// route (and its listener) serving after it was dropped from the manifest.
		desiredRoutes := make(map[string]bool, len(da.GetRoutes()))
		for _, r := range da.GetRoutes() {
			desiredRoutes[state.RouteKey(r.GetDomain(), r.GetPath())] = true
		}
		for _, r := range f.State.Routes.List() {
			if r.GetDeployment() != da.GetDeployment() {
				continue
			}
			if k := state.RouteKey(r.GetDomain(), r.GetPath()); !desiredRoutes[k] {
				f.State.Routes.Remove(k, idx)
			}
		}
		for _, r := range da.GetRoutes() {
			f.State.Routes.Apply(r, idx)
		}
		// Same replace-set for TCP routes (derived from compose ports).
		desiredTCP := make(map[string]bool, len(da.GetTcpRoutes()))
		for _, r := range da.GetTcpRoutes() {
			desiredTCP[state.TCPRouteKey(r.GetPublishedPort())] = true
		}
		for _, r := range f.State.TCPRoutes.List() {
			if r.GetDeployment() != da.GetDeployment() {
				continue
			}
			if k := state.TCPRouteKey(r.GetPublishedPort()); !desiredTCP[k] {
				f.State.TCPRoutes.Remove(k, idx)
			}
		}
		for _, r := range da.GetTcpRoutes() {
			f.State.TCPRoutes.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_APPLY, map[string]string{
			"deployment": da.GetDeployment(),
			"revision":   strconv.FormatUint(da.GetRevision(), 10),
		}

	case *pb.Command_DeploymentRollback:
		// Swap applied_revision and previous_revision so the marker reflects
		// the rollback target. Full state restore — re-deriving the previous
		// revision's services and routes from stored YAML — requires
		// revision history which is a follow-up; v1 just flips the markers
		// and audits ROLLBACK.
		dr := p.DeploymentRollback
		if existing, ok := f.State.Deployments.Get(dr.GetDeployment()); ok {
			wasApplied := existing.GetAppliedRevision()
			existing.AppliedRevision = dr.GetRevision()
			existing.PreviousRevision = wasApplied
			existing.UpdatedAt = cmd.GetTs()
			f.State.Deployments.Apply(existing, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_ROLLBACK, map[string]string{
			"deployment": p.DeploymentRollback.GetDeployment(),
			"revision":   strconv.FormatUint(p.DeploymentRollback.GetRevision(), 10),
		}

	case *pb.Command_DeploymentDelete:
		name := p.DeploymentDelete.GetDeployment()
		for _, r := range f.State.Routes.List() {
			if r.GetDeployment() == name {
				f.State.Routes.Remove(state.RouteKey(r.GetDomain(), r.GetPath()), idx)
			}
		}
		for _, r := range f.State.TCPRoutes.List() {
			if r.GetDeployment() == name {
				f.State.TCPRoutes.Remove(state.TCPRouteKey(r.GetPublishedPort()), idx)
			}
		}
		for _, rd := range f.State.ReplicasDesired.List() {
			if rd.GetDeployment() == name {
				f.State.ReplicasDesired.Remove(rd.GetId(), idx)
				// Cascade observed records too. Without this, a later
				// re-apply of the same deployment name (which reuses
				// the same `<dep>-<svc>-<idx>` replica ids) reads stale
				// observations from the prior incarnation — broke the
				// depends_on gate (issue #130) when a redeploy
				// satisfied the gate against a previous run's RUNNING
				// records.
				f.State.ReplicasObserved.Remove(rd.GetId(), idx)
			}
		}
		// Free every per-host /24 allocated for this deployment.
		for _, sn := range f.State.Subnets.List() {
			if sn.GetDeployment() == name {
				f.State.Subnets.Remove(state.SubnetKey(sn.GetDeployment(), sn.GetNetwork(), sn.GetHost()), idx)
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

	case *pb.Command_RestartCounterUpdate:
		u := p.RestartCounterUpdate
		switch u.GetAction() {
		case pb.RestartCounterUpdate_ACTION_INCREMENT:
			var failures int32 = 1
			if existing, ok := f.State.RestartCounters.Get(u.GetReplicaId()); ok {
				failures = existing.GetConsecutiveFailures() + 1
			}
			f.State.RestartCounters.Apply(&pb.RestartCounter{
				ReplicaId:           u.GetReplicaId(),
				ConsecutiveFailures: failures,
				LastAttemptAt:       cmd.GetTs(),
			}, idx)
		case pb.RestartCounterUpdate_ACTION_RESET:
			f.State.RestartCounters.Remove(u.GetReplicaId(), idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_RouteUpsert:
		if r := p.RouteUpsert.GetRoute(); r != nil {
			f.State.Routes.Apply(r, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_RouteRemove:
		// RouteRemove targets a whole domain (no path field), so drop every
		// route under it — the store keys on (domain, path) now, so a single
		// Remove(domain) would no longer match.
		for _, r := range f.State.Routes.List() {
			if r.GetDomain() == p.RouteRemove.GetDomain() {
				f.State.Routes.Remove(state.RouteKey(r.GetDomain(), r.GetPath()), idx)
			}
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertStore:
		// Key namespace (`cert/<domain>/...` vs `challenge/<token>`) is
		// interpreted by the storage layer in task 33. The FSM holds it as an
		// opaque blob until that task decodes by prefix.
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertLock:
		l := p.CertLock
		now := cmd.GetTs().AsTime()
		if c, ok := f.State.Certs.Get(l.GetName()); ok {
			// Reject the lock when it's held by a different lessee and the
			// existing LockUntil hasn't passed yet (cluster-wide single-
			// flight semantics for CertMagic). Same-lessee reapplies are
			// the auto-renew path and always accepted.
			if c.GetLessee() != "" && c.GetLessee() != l.GetLessee() &&
				c.GetLockUntil() != nil && c.GetLockUntil().AsTime().After(now) {
				return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
			}
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

	case *pb.Command_CertBlobUpsert:
		if b := p.CertBlobUpsert.GetBlob(); b != nil {
			if b.GetUpdatedAt() == nil {
				b.UpdatedAt = cmd.GetTs()
			}
			f.State.CertBlobs.Apply(b, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_CertBlobRemove:
		if key := p.CertBlobRemove.GetKey(); key != "" {
			f.State.CertBlobs.Remove(key, idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_SubnetAllocate:
		sa := p.SubnetAllocate
		f.State.Subnets.Apply(&pb.Subnet{
			Deployment:  sa.GetDeployment(),
			Network:     sa.GetNetwork(),
			Cidr:        sa.GetCidr(),
			AllocatedAt: cmd.GetTs(),
			Host:        sa.GetHost(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_SubnetFree:
		sf := p.SubnetFree
		// Remove by the most-specific key when all three fields are set;
		// otherwise cascade-remove every entry matching the set fields
		// (empty network and/or host act as wildcards).
		if sf.GetDeployment() != "" && sf.GetNetwork() != "" && sf.GetHost() != "" {
			f.State.Subnets.Remove(state.SubnetKey(sf.GetDeployment(), sf.GetNetwork(), sf.GetHost()), idx)
			return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
		}
		for _, sn := range f.State.Subnets.List() {
			if sf.GetDeployment() != "" && sn.GetDeployment() != sf.GetDeployment() {
				continue
			}
			if sf.GetNetwork() != "" && sn.GetNetwork() != sf.GetNetwork() {
				continue
			}
			if sf.GetHost() != "" && sn.GetHost() != sf.GetHost() {
				continue
			}
			f.State.Subnets.Remove(state.SubnetKey(sn.GetDeployment(), sn.GetNetwork(), sn.GetHost()), idx)
		}
		return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil

	case *pb.Command_TokenIssue:
		ti := p.TokenIssue
		f.State.Tokens.Apply(&pb.Token{
			Identity:         ti.GetIdentity(),
			HashedSecret:     ti.GetHashedSecret(),
			IssuedAt:         cmd.GetTs(),
			AllowsPrivileged: ti.GetAllowsPrivileged(),
		}, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_TOKEN_ISSUE, map[string]string{
			"identity":          ti.GetIdentity(),
			"allows_privileged": strconv.FormatBool(ti.GetAllowsPrivileged()),
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

	case *pb.Command_RegistryCredentialUpsert:
		// Canonicalize the host so a credential added as "index.docker.io" by
		// one operator is found by the reconciler that just normalized the
		// image ref to "docker.io". Empty/garbage hosts are dropped silently
		// (the gRPC handler validates up-front; this guard catches Apply
		// reaching the FSM via an unvalidated path, e.g. a future test
		// helper).
		cred := p.RegistryCredentialUpsert.GetCredential()
		if cred == nil {
			return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
		}
		host := canonicalRegistryKey(cred.GetRegistry())
		if host == "" {
			return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
		}
		stored := proto.Clone(cred).(*pb.RegistryCredential)
		stored.Registry = host
		if stored.GetUpdatedAt() == nil {
			stored.UpdatedAt = cmd.GetTs()
		}
		f.State.RegistryCredentials.Apply(stored, idx)
		// Audit MUST NOT carry the secret (or even its length / hash — the
		// posture is symmetric with TOKEN_ISSUE which records only identity).
		return pb.AuditEventType_AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_UPSERT, map[string]string{
			"registry": host,
			"username": cred.GetUsername(),
		}

	case *pb.Command_RegistryCredentialRemove:
		host := canonicalRegistryKey(p.RegistryCredentialRemove.GetRegistry())
		if host == "" {
			return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
		}
		f.State.RegistryCredentials.Remove(host, idx)
		return pb.AuditEventType_AUDIT_EVENT_TYPE_REGISTRY_CREDENTIAL_REMOVE, map[string]string{
			"registry": host,
		}
	}

	// Unknown / missing payload — recorded by returning UNSPECIFIED; no audit
	// event but Apply returns nil so raft doesn't surface this as a hard
	// error. Future Command variants must be added explicitly above.
	return pb.AuditEventType_AUDIT_EVENT_TYPE_UNSPECIFIED, nil
}

// commandOp returns a short operation label for the command's oneof variant,
// derived from the Go type name (e.g. *pb.Command_NodeJoin → "NodeJoin").
// It deliberately exposes only the variant NAME, never the payload contents,
// so DEBUG fsm-apply logging stays free of sensitive fields.
func commandOp(cmd *pb.Command) string {
	if cmd == nil || cmd.Payload == nil {
		return "unknown"
	}
	name := fmt.Sprintf("%T", cmd.Payload) // e.g. "*pb.Command_NodeJoin"
	if i := strings.LastIndex(name, "_"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// canonicalRegistryKey normalizes the credential key used by
// state.RegistryCredentials to the same form the reconciler's per-pull
// resolver matches against (pull.CanonicalRepo + pull.MatchCredentialKey).
// Both sides MUST agree or the lookup misses every credential.
//
// The key is "host[:port]" or, when the operator scopes the credential to a
// registry namespace, "host[:port]/namespace" (e.g. "ghcr.io/owner"). Empty /
// whitespace-only input returns "" so the caller can drop the command;
// otherwise the host is lower-cased with the legacy Docker Hub aliases
// ("index.docker.io" and friends) folded to "docker.io", any scheme / query /
// fragment is stripped, and the namespace path is lower-cased and trimmed of
// surrounding slashes. Self-hosted hosts with a non-default port keep the
// port verbatim.
func canonicalRegistryKey(ref string) string {
	h := strings.ToLower(strings.TrimSpace(ref))
	if h == "" {
		return ""
	}
	// Some clients emit "https://registry.example.com/v2/" — strip the scheme
	// first so the path split below sees the bare host.
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "?#"); i >= 0 {
		h = h[:i]
	}
	host, path := h, ""
	if i := strings.IndexByte(h, '/'); i >= 0 {
		host, path = h[:i], h[i+1:]
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.docker.io":
		host = "docker.io"
	}
	if host == "" {
		return ""
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return host
	}
	return host + "/" + path
}
