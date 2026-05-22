package lifecycle

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

// Reconcile is the daemon-startup orphan sweep. Lists every container labeled
// with our cluster_id and stop+removes any whose jaco.replica_id is absent
// from expectedReplicaIDs. Returns the list of removed container ids so the
// caller can audit (or surface them in tests).
//
// Called from runtime sub-init when `jaco serve` boots; the typical caller
// populates expectedReplicaIDs from state.ReplicasDesired.List() filtered to
// host=self.
func Reconcile(ctx context.Context, d dockerx.Docker, clusterID string, expectedReplicaIDs map[string]bool) ([]string, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("Reconcile: clusterID is required")
	}
	args := filters.NewArgs()
	args.Add("label", labelClusterID+"="+clusterID)
	list, err := d.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var removed []string
	for _, c := range list {
		rid := c.Labels[labelReplicaID]
		if rid == "" {
			// Defensive: a JACO-cluster-labeled container without a replica id
			// shouldn't exist, but if it does, leave it alone (could be a
			// manual debug container; orphan logic only acts on labeled
			// replicas).
			continue
		}
		if expectedReplicaIDs[rid] {
			continue
		}
		if err := stopAndRemove(ctx, d, c.ID); err != nil {
			return removed, fmt.Errorf("stop+remove orphan %s (replica %s): %w", c.ID, rid, err)
		}
		removed = append(removed, rid)
	}
	return removed, nil
}
