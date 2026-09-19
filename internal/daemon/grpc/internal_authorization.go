package grpc

import (
	"context"
	"math"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func (s *Server) authorizeInternalCommand(ctx context.Context, cmd *pb.Command) error {
	if admission.IdentityFromContext(ctx) == admission.LocalIdentity {
		return nil
	}
	cert := admission.NodeCertificateFromContext(ctx)
	if cert == nil {
		return status.Error(codes.Unauthenticated, "peer_invalid")
	}
	host := cert.Subject.CommonName
	st := s.State()
	now := timestamppb.Now()
	identity := "node:" + host
	if cmd.GetClusterId() != "" && cmd.GetClusterId() != st.Cluster.Get().GetClusterId() {
		return errPeerCommandScope
	}
	switch p := cmd.Payload.(type) {
	case *pb.Command_NodeStatusUpdate:
		u := p.NodeStatusUpdate
		if u.GetHostname() != host {
			return errPeerCommandScope
		}
		if _, ok := pb.NodeStatus_name[int32(u.GetStatus())]; !ok ||
			(u.GetIncludePressure() && (!validPressure(u.GetCpuPressure()) || !validPressure(u.GetMemoryPressure()))) {
			return status.Error(codes.InvalidArgument, "invalid_node_status")
		}
		u.Details = bindNodeDetails(u.GetDetails(), host)
	case *pb.Command_NodeUpdateSelf:
		u := p.NodeUpdateSelf
		if u.GetHostname() != host {
			return errPeerCommandScope
		}
		if len(u.GetWireguardPubkey()) != 0 && len(u.GetWireguardPubkey()) != 32 {
			return status.Error(codes.InvalidArgument, "invalid_wireguard_key")
		}
		if addr := u.GetGrpcAddress(); addr != "" {
			name, port, err := net.SplitHostPort(addr)
			if err != nil || name == "" {
				return status.Error(codes.InvalidArgument, "invalid_grpc_address")
			}
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return status.Error(codes.InvalidArgument, "invalid_grpc_address")
			}
			if err := cert.VerifyHostname(name); err != nil {
				return errPeerCommandScope
			}
		}
	case *pb.Command_ReplicaObservedUpdate:
		obs := p.ReplicaObservedUpdate.GetReplica()
		if obs == nil || obs.GetId() == "" {
			return status.Error(codes.InvalidArgument, "invalid_replica_observation")
		}
		desired, ok := st.ReplicasDesired.Get(obs.GetId())
		if !ok || desired.GetHost() != host || (obs.GetHost() != "" && obs.GetHost() != host) {
			return errPeerCommandScope
		}
		if _, ok := pb.ReplicaState_name[int32(obs.GetState())]; !ok || obs.GetState() == pb.ReplicaState_REPLICA_STATE_UNSPECIFIED {
			return status.Error(codes.InvalidArgument, "invalid_replica_state")
		}
		obs.Host = host
		obs.Details = bindNodeDetails(obs.GetDetails(), host)
		obs.Details["deployment"] = desired.GetDeployment()
		if obs.GetLastHealthAt() != nil {
			obs.LastHealthAt = now
		}
	case *pb.Command_AuditAppend:
		ev := p.AuditAppend.GetEvent()
		if ev == nil {
			return status.Error(codes.InvalidArgument, "invalid_audit_event")
		}
		switch ev.GetType() {
		case pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED,
			pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE:
		case pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED,
			pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED,
			pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED:
			if !s.hasTLSRoute(ev.GetPayload()["domain"]) {
				return errPeerCommandScope
			}
		default:
			return errPeerCommandScope
		}
		ev.Identity, ev.Ts, ev.RaftIndex = identity, now, 0
		ev.Payload = bindNodeDetails(ev.GetPayload(), host)
		ev.Payload["identity"] = identity
	case *pb.Command_CertLock:
		lock := p.CertLock
		if lock.GetName() == "" {
			return status.Error(codes.InvalidArgument, "invalid_cert_lock")
		}
		if lock.GetLessee() != host {
			return errPeerCommandScope
		}
		lock.Until = timestamppb.New(now.AsTime().Add(storage.LockTTL))
	case *pb.Command_CertUnlock:
		if p.CertUnlock.GetName() == "" {
			return status.Error(codes.InvalidArgument, "invalid_cert_unlock")
		}
		if lock, ok := st.Certs.Get(p.CertUnlock.GetName()); ok &&
			lock.GetLessee() != "" && lock.GetLessee() != host {
			return errPeerCommandScope
		}
	case *pb.Command_CertBlobUpsert:
		blob := p.CertBlobUpsert.GetBlob()
		if blob == nil || blob.GetKey() == "" {
			return status.Error(codes.InvalidArgument, "invalid_cert_blob")
		}
		blob.UpdatedAt = now
	case *pb.Command_CertBlobRemove:
		if p.CertBlobRemove.GetKey() == "" {
			return status.Error(codes.InvalidArgument, "invalid_cert_blob")
		}
	case *pb.Command_ChallengeTokenStore:
		token := p.ChallengeTokenStore.GetToken()
		if token == nil || token.GetToken() == "" || token.GetKeyAuth() == "" {
			return status.Error(codes.InvalidArgument, "invalid_challenge_token")
		}
		if !s.hasTLSRoute(token.GetDomain()) {
			return errPeerCommandScope
		}
		token.ExpiresAt = timestamppb.New(now.AsTime().Add(challenge.TokenTTL))
	default:
		// Controllers apply desired state locally. No network caller, including
		// an operator bearer, can smuggle an admin mutation through a batch.
		return status.Error(codes.PermissionDenied, "peer_command_denied")
	}
	cmd.Identity, cmd.Ts, cmd.RaftIndex = identity, now, 0
	cmd.ClusterId = st.Cluster.Get().GetClusterId()
	return nil
}

var errPeerCommandScope = status.Error(codes.PermissionDenied, "peer_command_scope")

func (s *Server) authorizeInternalLogs(ctx context.Context) error {
	ctx, err := admission.AuthenticateNode(ctx, s.State())
	if err != nil {
		return err
	}
	if admission.IdentityFromContext(ctx) == admission.LocalIdentity {
		return nil
	}
	cert := admission.NodeCertificateFromContext(ctx)
	if cert == nil {
		return status.Error(codes.Unauthenticated, "peer_invalid")
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("authorization")) != 0 {
		_, err := admission.BearerIdentity(ctx, s.State())
		return err
	}
	if r := s.Raft(); r != nil {
		_, leader := r.Raft.LeaderWithID()
		if string(leader) == cert.Subject.CommonName {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "peer_logs_denied: operator bearer or current leader required")
}

func validPressure(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

func (s *Server) hasTLSRoute(domain string) bool {
	if domain == "" {
		return false
	}
	for _, route := range s.State().Routes.List() {
		if route.GetTlsAuto() && strings.EqualFold(strings.TrimSuffix(route.GetDomain(), "."), strings.TrimSuffix(domain, ".")) {
			return true
		}
	}
	return false
}

func bindNodeDetails(details map[string]string, host string) map[string]string {
	if details == nil {
		details = make(map[string]string)
	}
	details["host"], details["hostname"] = host, host
	return details
}

func (s *Server) authorizeInternalSubnet(ctx context.Context, req *pb.EnsureSubnetRequest) error {
	if admission.IdentityFromContext(ctx) == admission.LocalIdentity {
		return nil
	}
	cert := admission.NodeCertificateFromContext(ctx)
	if cert == nil {
		return status.Error(codes.Unauthenticated, "peer_invalid")
	}
	if req.GetHost() != cert.Subject.CommonName {
		return errPeerCommandScope
	}

	st := s.State()
	var services []string
	for _, rep := range st.ReplicasDesired.List() {
		if rep.GetHost() == req.GetHost() && rep.GetDeployment() == req.GetDeployment() {
			services = append(services, rep.GetService())
		}
	}
	dep, ok := st.Deployments.Get(req.GetDeployment())
	if !ok || len(services) == 0 {
		return errPeerCommandScope
	}
	// Use the runtime's projection, including implicit _default networks,
	// rather than trusting a network name supplied by the requesting host.
	project, err := compose.LoadBytes(dep.GetComposeYaml(), "subnet-admission.yml")
	if err != nil {
		return status.Error(codes.FailedPrecondition, "deployment_networks_unavailable")
	}
	for _, name := range services {
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		spec := compose.ToContainerSpec(svc, compose.SpecOptions{Deployment: req.GetDeployment()})
		for _, network := range spec.Networks {
			if bridge.NetworkNameFromDockerName(network) == req.GetNetwork() {
				return nil
			}
		}
	}
	return errPeerCommandScope
}
