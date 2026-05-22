package watch

import (
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Registry owns one Broker per entity type in the control plane. Constructed
// once at daemon start and shared with state.Store.
type Registry struct {
	Nodes            *Broker[*pb.Node]
	Deployments      *Broker[*pb.Deployment]
	ReplicasDesired  *Broker[*pb.ReplicaDesired]
	ReplicasObserved *Broker[*pb.ReplicaObserved]
	Routes           *Broker[*pb.Route]
	Certs            *Broker[*pb.Cert]
	ChallengeTokens  *Broker[*pb.ChallengeToken]
	Tokens           *Broker[*pb.Token]
	JoinTokens       *Broker[*pb.JoinToken]
	Subnets          *Broker[*pb.Subnet]
	RolloutPlans     *Broker[*pb.RolloutPlan]
	ReplicaCounters  *Broker[*pb.ReplicaCounter]
	RestartCounters  *Broker[*pb.RestartCounter]
	AuditEvents      *Broker[*pb.AuditEvent]
	Cluster          *Broker[*pb.ClusterMeta]
}

// NewRegistry builds a Registry with each broker sized to DefaultBuffer.
func NewRegistry() *Registry {
	return &Registry{
		Nodes:            NewBroker[*pb.Node](DefaultBuffer),
		Deployments:      NewBroker[*pb.Deployment](DefaultBuffer),
		ReplicasDesired:  NewBroker[*pb.ReplicaDesired](DefaultBuffer),
		ReplicasObserved: NewBroker[*pb.ReplicaObserved](DefaultBuffer),
		Routes:           NewBroker[*pb.Route](DefaultBuffer),
		Certs:            NewBroker[*pb.Cert](DefaultBuffer),
		ChallengeTokens:  NewBroker[*pb.ChallengeToken](DefaultBuffer),
		Tokens:           NewBroker[*pb.Token](DefaultBuffer),
		JoinTokens:       NewBroker[*pb.JoinToken](DefaultBuffer),
		Subnets:          NewBroker[*pb.Subnet](DefaultBuffer),
		RolloutPlans:     NewBroker[*pb.RolloutPlan](DefaultBuffer),
		ReplicaCounters:  NewBroker[*pb.ReplicaCounter](DefaultBuffer),
		RestartCounters:  NewBroker[*pb.RestartCounter](DefaultBuffer),
		AuditEvents:      NewBroker[*pb.AuditEvent](DefaultBuffer),
		Cluster:          NewBroker[*pb.ClusterMeta](DefaultBuffer),
	}
}
