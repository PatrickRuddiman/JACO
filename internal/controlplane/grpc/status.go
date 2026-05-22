package grpcsrv

import (
	"context"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Status returns a snapshot of the (filtered) deployment(s), their observed
// replicas, and their routes. Reads from local state; no leader required so
// the CLI can hit any node.
func (d *deployServer) Status(_ context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
	depFilter := req.GetDeploymentFilter()
	svcFilter := req.GetServiceFilter()

	resp := &pb.DeployStatusResponse{}

	for _, dep := range d.state.Deployments.List() {
		if depFilter != "" && dep.GetName() != depFilter {
			continue
		}
		resp.Deployments = append(resp.Deployments, dep)
	}

	// Replica filtering needs the per-replica deployment/service which lives
	// on ReplicaDesired (ReplicaObserved doesn't carry it). Index desired by
	// id so the per-observed lookup stays O(1).
	desiredByID := map[string]*pb.ReplicaDesired{}
	for _, r := range d.state.ReplicasDesired.List() {
		desiredByID[r.GetId()] = r
	}
	for _, r := range d.state.ReplicasObserved.List() {
		if depFilter != "" || svcFilter != "" {
			rd, ok := desiredByID[r.GetId()]
			if !ok {
				continue
			}
			if depFilter != "" && rd.GetDeployment() != depFilter {
				continue
			}
			if svcFilter != "" && rd.GetService() != svcFilter {
				continue
			}
		}
		resp.Replicas = append(resp.Replicas, r)
	}

	for _, rt := range d.state.Routes.List() {
		if depFilter != "" && rt.GetDeployment() != depFilter {
			continue
		}
		if svcFilter != "" && rt.GetService() != svcFilter {
			continue
		}
		resp.Routes = append(resp.Routes, rt)
	}
	return resp, nil
}
