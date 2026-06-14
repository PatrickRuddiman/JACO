package grpcsrv

import (
	"context"
	"sort"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// GetRoute returns the realized ingress view for a single domain: the routes
// Caddy serves, ordered exactly as it evaluates them (path-scoped routes
// longest-prefix-first, the catch-all last), each joined with how many of its
// upstream (deployment, service) replicas are currently eligible as upstreams.
// Reads from local raft state; no leader required, and the answer is
// node-agnostic because BuildCaddyConfig is a deterministic function of the
// same replicated state. Closes issue #174's "no way to verify what's actually
// serving" gap.
func (d *deployServer) GetRoute(_ context.Context, req *pb.GetRouteRequest) (*pb.GetRouteResponse, error) {
	domain := req.GetDomain()
	if domain == "" {
		return nil, errorStatus(codes.InvalidArgument, "validation_failed", "domain is required")
	}

	var domainRoutes []*pb.Route
	for _, rt := range d.state.Routes.List() {
		if rt.GetDomain() == domain {
			domainRoutes = append(domainRoutes, rt)
		}
	}
	if len(domainRoutes) == 0 {
		return nil, errorStatus(codes.NotFound, "route_not_found", "no routes for domain "+domain)
	}

	// Order to match Caddy evaluation: path-scoped routes longest-prefix-first
	// (ties broken alphabetically for determinism), catch-all (empty path)
	// last. Mirrors internal/ingress/config.buildDomainRoute.
	sort.SliceStable(domainRoutes, func(i, j int) bool {
		pi, pj := domainRoutes[i].GetPath(), domainRoutes[j].GetPath()
		if (pi == "") != (pj == "") {
			return pi != "" // non-empty paths sort before the catch-all
		}
		if len(pi) != len(pj) {
			return len(pi) > len(pj) // longer prefix first
		}
		return pi < pj
	})

	ready, total := d.upstreamReadiness(time.Now())

	resp := &pb.GetRouteResponse{Domain: domain}
	for _, rt := range domainRoutes {
		key := config.MetaKey(rt.GetDeployment(), rt.GetService())
		resp.Routes = append(resp.Routes, &pb.RealizedRoute{
			Path:          rt.GetPath(),
			CatchAll:      rt.GetPath() == "",
			Deployment:    rt.GetDeployment(),
			Service:       rt.GetService(),
			Port:          rt.GetPort(),
			TlsAuto:       rt.GetTlsAuto(),
			StripPath:     rt.GetStripPath(),
			ReadyReplicas: ready[key],
			TotalReplicas: total[key],
		})
	}
	return resp, nil
}

// upstreamReadiness counts, per (deployment, service) MetaKey, the observed
// replicas (total) and those eligible as Caddy upstreams (ready: running and
// health-fresh, the same rule BuildCaddyConfig applies). The deployment/service
// for an observed replica comes from its ReplicaDesired (ReplicaObserved does
// not carry it), joined by id.
func (d *deployServer) upstreamReadiness(now time.Time) (ready, total map[string]int32) {
	desiredByID := map[string]*pb.ReplicaDesired{}
	for _, rd := range d.state.ReplicasDesired.List() {
		desiredByID[rd.GetId()] = rd
	}
	ready = map[string]int32{}
	total = map[string]int32{}
	for _, obs := range d.state.ReplicasObserved.List() {
		rd, ok := desiredByID[obs.GetId()]
		if !ok {
			continue
		}
		key := config.MetaKey(rd.GetDeployment(), rd.GetService())
		total[key]++
		if isUpstreamEligible(obs, now) {
			ready[key]++
		}
	}
	return ready, total
}

// isUpstreamEligible mirrors internal/ingress/config.isEligible: an observed
// replica is an admissible upstream when it is running and its last health
// report is within config.HealthFreshness.
func isUpstreamEligible(obs *pb.ReplicaObserved, now time.Time) bool {
	if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
		return false
	}
	last := obs.GetLastHealthAt()
	if last == nil {
		return false
	}
	t := last.AsTime()
	if t.IsZero() {
		return false
	}
	return now.Sub(t) < config.HealthFreshness
}
