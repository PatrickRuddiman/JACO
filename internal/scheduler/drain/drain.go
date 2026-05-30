// Package drain orchestrates the migrate-replicas-off-a-host workflow that
// precedes a graceful Cluster.NodeRemove. v1 surfaces a Plan helper —
// "given the current state, what replicas need to move and where"; the full
// drain step machine (await replacements healthy → stop evictees →
// raft.RemoveServer) lands alongside the daemon entry.
package drain

import (
	"fmt"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/placement"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Migration describes one replica that needs to move during a drain.
type Migration struct {
	ReplicaID  string
	Deployment string
	Service    string
	FromHost   string
	ToHost     string
	Image      string
}

// Plan returns the ordered list of Migrations required to drain hostname.
// Empty result means the host has no replicas to drain. Returns an error if
// any replica's service can't be placed on the remaining eligible set —
// e.g. a hosts-pinned service where the only host being drained leaves
// nothing eligible.
//
// The plan reflects an instantaneous snapshot of state.ReplicasDesired and
// state.Nodes; the actual drain step machine (waiting for replacements to
// be healthy before stopping evictees) lands when the daemon entry wires
// drain into Cluster.NodeRemove(force=false).
func Plan(st *state.State, hostname string) ([]Migration, error) {
	if hostname == "" {
		return nil, fmt.Errorf("Plan: hostname is required")
	}

	// Build the eligible-host set excluding the draining node.
	nodes := st.Nodes.List()
	remaining := make([]*pb.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.GetHostname() == hostname {
			continue
		}
		remaining = append(remaining, n)
	}

	// Group desired replicas by service so we honor PACK's per-host counts
	// without inflating them by the about-to-evict replicas.
	type svcKey struct{ deployment, service string }
	bySvc := map[svcKey][]*pb.ReplicaDesired{}
	for _, r := range st.ReplicasDesired.List() {
		k := svcKey{r.GetDeployment(), r.GetService()}
		bySvc[k] = append(bySvc[k], r)
	}

	// Look up ServiceSpec for placement decisions. ServiceSpec lives on
	// Deployment.services; build a quick lookup.
	specByDep := map[string]*pb.Deployment{}
	for _, d := range st.Deployments.List() {
		specByDep[d.GetName()] = d
	}
	specOf := func(dep, svc string) *pb.ServiceSpec {
		d, ok := specByDep[dep]
		if !ok {
			return nil
		}
		for _, s := range d.GetServices() {
			if s.GetName() == svc {
				return s
			}
		}
		return nil
	}

	var migrations []Migration
	for k, replicas := range bySvc {
		spec := specOf(k.deployment, k.service)
		if spec == nil {
			// Service no longer declared in the Deployment — leave it alone.
			continue
		}
		// GLOBAL (daemonset) replicas are not migrated: the service runs
		// exactly one replica per node, so the draining host's replica is
		// simply dropped (the scheduler will not recreate it elsewhere — a
		// node that already runs the service must not get a second copy).
		// Migrating it would double-place the daemonset on the target host.
		if spec.GetPlacement() == pb.ServiceSpec_PLACEMENT_MODE_GLOBAL {
			continue
		}

		eligible := placement.EligibleHosts(spec, remaining)

		// Current per-host counts for PACK, excluding the draining host.
		counts := map[string]int{}
		for _, r := range replicas {
			if r.GetHost() == hostname {
				continue
			}
			counts[r.GetHost()]++
		}

		for _, r := range replicas {
			if r.GetHost() != hostname {
				continue
			}
			newHost, err := placement.PlaceReplica(k.deployment, spec, eligible, int(r.GetIndex()), counts)
			if err != nil {
				return nil, fmt.Errorf("drain %s: service %s/%s has no remaining eligible host: %w",
					hostname, k.deployment, k.service, err)
			}
			migrations = append(migrations, Migration{
				ReplicaID:  r.GetId(),
				Deployment: k.deployment,
				Service:    k.service,
				FromHost:   hostname,
				ToHost:     newHost,
				Image:      r.GetImage(),
			})
			counts[newHost]++
		}
	}
	return migrations, nil
}
