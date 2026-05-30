// Package placement is the pure scheduler decision layer: given a service
// spec, the set of healthy nodes, and the current per-host replica counts,
// it picks the host that should receive the next replica.
//
// Three modes from the scheduler slice §3:
//   - SPREAD (default): FNV-1a 32-bit hash of `<deployment>-<service>-<index>`
//     mod len(eligible). Same input → same host across reconciles → stable
//     placement under leader failover.
//   - PACK: pick the eligible host with the fewest existing replicas;
//     hostname-lex tiebreak.
//   - HOSTS: spec.Hosts ∩ healthy_nodes. If fewer than spec.Replicas
//     candidates survive, raise cannot_satisfy_host_placement.
//
// GLOBAL (daemonset) is NOT decided in this package: the scheduler reconcile
// loop handles it directly, placing exactly one replica per ready node and
// ignoring spec.Replicas. EligibleHosts still returns the ready hosts for a
// GLOBAL service (the HOSTS-intersection branch is skipped), which the
// scheduler maps one-per-host; PlaceReplica is never called for GLOBAL.
package placement

import (
	"fmt"
	"hash/fnv"
	"sort"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// PlacementError is the typed result PlaceReplica returns when it can't
// satisfy a HOSTS-mode service. Other modes never fail (so long as at least
// one eligible host exists).
type PlacementError struct {
	Code    string
	Message string
	Details map[string]string
}

// Error implements the error interface.
func (e *PlacementError) Error() string { return e.Message }

// EligibleHosts returns the set of hostnames the scheduler may place this
// service on. Always returns a sorted slice. Nodes whose status is not
// NODE_STATUS_READY are excluded. In HOSTS mode, the result is intersected
// with spec.Hosts.
func EligibleHosts(spec *pb.ServiceSpec, nodes []*pb.Node) []string {
	healthy := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.GetStatus() == pb.NodeStatus_NODE_STATUS_READY {
			healthy = append(healthy, n.GetHostname())
		}
	}

	if spec.GetPlacement() == pb.ServiceSpec_PLACEMENT_MODE_HOSTS && len(spec.GetHosts()) > 0 {
		wanted := make(map[string]bool, len(spec.GetHosts()))
		for _, h := range spec.GetHosts() {
			wanted[h] = true
		}
		filtered := healthy[:0]
		for _, h := range healthy {
			if wanted[h] {
				filtered = append(filtered, h)
			}
		}
		healthy = filtered
	}

	sort.Strings(healthy)
	return healthy
}

// PlaceReplica picks the host for replicaIndex of spec given the eligible
// host set and per-host replica counts (only used in PACK mode).
//
// HOSTS mode raises PlacementError{code:cannot_satisfy_host_placement} when
// len(eligible) < spec.Replicas.
// SPREAD / PACK never fail so long as len(eligible) > 0.
func PlaceReplica(deployment string, spec *pb.ServiceSpec, eligible []string, replicaIndex int, currentReplicaCounts map[string]int) (string, error) {
	if len(eligible) == 0 {
		return "", &PlacementError{
			Code:    "cannot_satisfy_host_placement",
			Message: fmt.Sprintf("service %q: no eligible hosts", spec.GetName()),
			Details: map[string]string{
				"service":   spec.GetName(),
				"requested": fmt.Sprintf("%d", spec.GetReplicas()),
				"eligible":  "0",
			},
		}
	}

	sortedEligible := append([]string(nil), eligible...)
	sort.Strings(sortedEligible)

	switch spec.GetPlacement() {
	case pb.ServiceSpec_PLACEMENT_MODE_HOSTS:
		if int(spec.GetReplicas()) > len(sortedEligible) {
			return "", &PlacementError{
				Code:    "cannot_satisfy_host_placement",
				Message: fmt.Sprintf("service %q: only %d eligible host(s) but %d replicas requested", spec.GetName(), len(sortedEligible), spec.GetReplicas()),
				Details: map[string]string{
					"service":   spec.GetName(),
					"requested": fmt.Sprintf("%d", spec.GetReplicas()),
					"eligible":  fmt.Sprintf("%d", len(sortedEligible)),
				},
			}
		}
		// Spread by replica index within the pinned hosts.
		return sortedEligible[replicaIndex%len(sortedEligible)], nil

	case pb.ServiceSpec_PLACEMENT_MODE_PACK:
		return pickPackHost(sortedEligible, currentReplicaCounts), nil

	default: // SPREAD (and UNSPECIFIED).
		return pickSpreadHost(deployment, spec.GetName(), sortedEligible, replicaIndex), nil
	}
}

// pickSpreadHost is deterministic — same (deployment, service, replicaIndex,
// eligible-set) → same host. FNV-1a 32-bit is cheap and uniformly
// distributed for the short strings JACO hashes.
func pickSpreadHost(deployment, service string, eligible []string, replicaIndex int) string {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s-%s-%d", deployment, service, replicaIndex)
	return eligible[int(h.Sum32())%len(eligible)]
}

// pickPackHost picks the eligible host with the fewest existing replicas.
// Tiebreak by hostname lex (matches the slice §3 decision).
func pickPackHost(eligible []string, counts map[string]int) string {
	best := eligible[0]
	bestCount := counts[best]
	for _, h := range eligible[1:] {
		c := counts[h]
		if c < bestCount || (c == bestCount && h < best) {
			best = h
			bestCount = c
		}
	}
	return best
}
