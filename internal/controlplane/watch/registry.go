package watch

import (
	"log/slog"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Registry owns one Broker per entity type in the control plane. Constructed
// once at daemon start and shared with state.Store.
type Registry struct {
	Nodes               *Broker[*pb.Node]
	Deployments         *Broker[*pb.Deployment]
	ReplicasDesired     *Broker[*pb.ReplicaDesired]
	ReplicasObserved    *Broker[*pb.ReplicaObserved]
	Routes              *Broker[*pb.Route]
	TCPRoutes           *Broker[*pb.TCPRoute]
	Certs               *Broker[*pb.Cert]
	CertBlobs           *Broker[*pb.CertBlob]
	ChallengeTokens     *Broker[*pb.ChallengeToken]
	Tokens              *Broker[*pb.Token]
	JoinTokens          *Broker[*pb.JoinToken]
	Subnets             *Broker[*pb.Subnet]
	RolloutPlans        *Broker[*pb.RolloutPlan]
	ReplicaCounters     *Broker[*pb.ReplicaCounter]
	RestartCounters     *Broker[*pb.RestartCounter]
	AuditEvents         *Broker[*pb.AuditEvent]
	Cluster             *Broker[*pb.ClusterMeta]
	RegistryCredentials *Broker[*pb.RegistryCredential]

	// Logger is the watch-subsystem logger. Set it via SetLogger so it
	// propagates to every broker (subscriber join/leave DEBUG, fanout-drop
	// WARN). nil → brokers discard.
	Logger *slog.Logger
}

// SetLogger tags the registry and every broker with logger so watch events
// (subscriber join/leave, fanout drops) carry subsystem=watch + a broker name.
func (r *Registry) SetLogger(logger *slog.Logger) {
	r.Logger = logger
	r.Nodes.logger, r.Nodes.name = logger, "Nodes"
	r.Deployments.logger, r.Deployments.name = logger, "Deployments"
	r.ReplicasDesired.logger, r.ReplicasDesired.name = logger, "ReplicasDesired"
	r.ReplicasObserved.logger, r.ReplicasObserved.name = logger, "ReplicasObserved"
	r.Routes.logger, r.Routes.name = logger, "Routes"
	r.Certs.logger, r.Certs.name = logger, "Certs"
	r.CertBlobs.logger, r.CertBlobs.name = logger, "CertBlobs"
	r.ChallengeTokens.logger, r.ChallengeTokens.name = logger, "ChallengeTokens"
	r.Tokens.logger, r.Tokens.name = logger, "Tokens"
	r.JoinTokens.logger, r.JoinTokens.name = logger, "JoinTokens"
	r.Subnets.logger, r.Subnets.name = logger, "Subnets"
	r.RolloutPlans.logger, r.RolloutPlans.name = logger, "RolloutPlans"
	r.ReplicaCounters.logger, r.ReplicaCounters.name = logger, "ReplicaCounters"
	r.RestartCounters.logger, r.RestartCounters.name = logger, "RestartCounters"
	r.AuditEvents.logger, r.AuditEvents.name = logger, "AuditEvents"
	r.Cluster.logger, r.Cluster.name = logger, "Cluster"
	r.RegistryCredentials.logger, r.RegistryCredentials.name = logger, "RegistryCredentials"
}

// NewRegistry builds a Registry with each broker sized to DefaultBuffer.
func NewRegistry() *Registry {
	return &Registry{
		Nodes:               NewBroker[*pb.Node](DefaultBuffer),
		Deployments:         NewBroker[*pb.Deployment](DefaultBuffer),
		ReplicasDesired:     NewBroker[*pb.ReplicaDesired](DefaultBuffer),
		ReplicasObserved:    NewBroker[*pb.ReplicaObserved](DefaultBuffer),
		Routes:              NewBroker[*pb.Route](DefaultBuffer),
		TCPRoutes:           NewBroker[*pb.TCPRoute](DefaultBuffer),
		Certs:               NewBroker[*pb.Cert](DefaultBuffer),
		CertBlobs:           NewBroker[*pb.CertBlob](DefaultBuffer),
		ChallengeTokens:     NewBroker[*pb.ChallengeToken](DefaultBuffer),
		Tokens:              NewBroker[*pb.Token](DefaultBuffer),
		JoinTokens:          NewBroker[*pb.JoinToken](DefaultBuffer),
		Subnets:             NewBroker[*pb.Subnet](DefaultBuffer),
		RolloutPlans:        NewBroker[*pb.RolloutPlan](DefaultBuffer),
		ReplicaCounters:     NewBroker[*pb.ReplicaCounter](DefaultBuffer),
		RestartCounters:     NewBroker[*pb.RestartCounter](DefaultBuffer),
		AuditEvents:         NewBroker[*pb.AuditEvent](DefaultBuffer),
		Cluster:             NewBroker[*pb.ClusterMeta](DefaultBuffer),
		RegistryCredentials: NewBroker[*pb.RegistryCredential](DefaultBuffer),
	}
}
