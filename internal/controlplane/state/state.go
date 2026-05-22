package state

import (
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// State is the top-level container holding one typed sub-store per entity
// type. Built once at daemon start; passed to the FSM and to read-only RPC
// handlers (Deploy.Status, Audit.Query, etc.).
//
// The keyed sub-stores share the generic Store[T] machinery from store.go.
// AuditEvents is append-only; Cluster is a singleton — see their own files.
type State struct {
	Nodes            *Store[*pb.Node]
	Deployments      *Store[*pb.Deployment]
	ReplicasDesired  *Store[*pb.ReplicaDesired]
	ReplicasObserved *Store[*pb.ReplicaObserved]
	Routes           *Store[*pb.Route]
	Certs            *Store[*pb.Cert]
	ChallengeTokens  *Store[*pb.ChallengeToken]
	Tokens           *Store[*pb.Token]
	JoinTokens       *Store[*pb.JoinToken]
	Subnets          *Store[*pb.Subnet]
	RolloutPlans     *Store[*pb.RolloutPlan]
	ReplicaCounters  *Store[*pb.ReplicaCounter]
	RestartCounters  *Store[*pb.RestartCounter]
	AuditEvents      *AuditEvents
	Cluster          *Cluster
}

// New constructs an empty State wired to a broker registry.
func New(brokers *watch.Registry) *State {
	return &State{
		Nodes:            newNodes(brokers.Nodes),
		Deployments:      newDeployments(brokers.Deployments),
		ReplicasDesired:  newReplicasDesired(brokers.ReplicasDesired),
		ReplicasObserved: newReplicasObserved(brokers.ReplicasObserved),
		Routes:           newRoutes(brokers.Routes),
		Certs:            newCerts(brokers.Certs),
		ChallengeTokens:  newChallengeTokens(brokers.ChallengeTokens),
		Tokens:           newTokens(brokers.Tokens),
		JoinTokens:       newJoinTokens(brokers.JoinTokens),
		Subnets:          newSubnets(brokers.Subnets),
		RolloutPlans:     newRolloutPlans(brokers.RolloutPlans),
		ReplicaCounters:  newReplicaCounters(brokers.ReplicaCounters),
		RestartCounters:  newRestartCounters(brokers.RestartCounters),
		AuditEvents:      newAuditEvents(brokers.AuditEvents),
		Cluster:          newCluster(brokers.Cluster),
	}
}
