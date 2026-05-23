package grpcsrv

import (
	"context"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestKindToProto_EveryVariant — the adapter between watch.Kind
// (controlplane) and pb.EventKind (wire). Every variant must map; the
// fall-through default returns UNSPECIFIED.
func TestKindToProto_EveryVariant(t *testing.T) {
	cases := []struct {
		in   watch.Kind
		want pb.EventKind
	}{
		{watch.KindAdded, pb.EventKind_EVENT_KIND_ADDED},
		{watch.KindUpdated, pb.EventKind_EVENT_KIND_UPDATED},
		{watch.KindRemoved, pb.EventKind_EVENT_KIND_REMOVED},
		{watch.KindResync, pb.EventKind_EVENT_KIND_RESYNC},
	}
	for _, c := range cases {
		if got := kindToProto(c.in); got != c.want {
			t.Errorf("kindToProto(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	// Unknown / out-of-range falls through to UNSPECIFIED.
	if got := kindToProto(watch.Kind(99)); got != pb.EventKind_EVENT_KIND_UNSPECIFIED {
		t.Errorf("kindToProto(99) = %v, want UNSPECIFIED", got)
	}
}

// TestDeployStatus_FilteringByDeploymentAndService — covers the
// filter-skip branches that the existing integration tests don't
// exercise directly.
func TestDeployStatus_FilteringByDeploymentAndService(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{Name: "app1"}, 1)
	st.Deployments.Apply(&pb.Deployment{Name: "app2"}, 2)

	// Two replicas on app1.web, one on app1.api, one on app2.web.
	for _, rd := range []*pb.ReplicaDesired{
		{Id: "app1-web-0", Deployment: "app1", Service: "web", Host: "h"},
		{Id: "app1-web-1", Deployment: "app1", Service: "web", Host: "h"},
		{Id: "app1-api-0", Deployment: "app1", Service: "api", Host: "h"},
		{Id: "app2-web-0", Deployment: "app2", Service: "web", Host: "h"},
	} {
		st.ReplicasDesired.Apply(rd, 1)
		st.ReplicasObserved.Apply(&pb.ReplicaObserved{Id: rd.Id, State: pb.ReplicaState_REPLICA_STATE_RUNNING}, 1)
	}
	st.Routes.Apply(&pb.Route{Domain: "a.example", Deployment: "app1", Service: "web", Port: 80}, 1)
	st.Routes.Apply(&pb.Route{Domain: "b.example", Deployment: "app1", Service: "api", Port: 8080}, 2)
	st.Routes.Apply(&pb.Route{Domain: "c.example", Deployment: "app2", Service: "web", Port: 80}, 3)

	d := &deployServer{state: st}

	// Filter by deployment.
	resp, err := d.Status(context.TODO(), &pb.DeployStatusRequest{DeploymentFilter: "app1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetDeployments()) != 1 {
		t.Errorf("deployments = %d, want 1 (app1)", len(resp.GetDeployments()))
	}
	if len(resp.GetReplicas()) != 3 {
		t.Errorf("replicas = %d, want 3 (app1: web*2 + api*1)", len(resp.GetReplicas()))
	}
	if len(resp.GetRoutes()) != 2 {
		t.Errorf("routes = %d, want 2 (app1: a + b)", len(resp.GetRoutes()))
	}

	// Filter by deployment + service.
	resp, err = d.Status(context.TODO(), &pb.DeployStatusRequest{DeploymentFilter: "app1", ServiceFilter: "web"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetReplicas()) != 2 {
		t.Errorf("filtered replicas = %d, want 2 (app1.web)", len(resp.GetReplicas()))
	}
	if len(resp.GetRoutes()) != 1 {
		t.Errorf("filtered routes = %d, want 1 (a.example)", len(resp.GetRoutes()))
	}

	// No filter returns everything.
	resp, _ = d.Status(context.TODO(), &pb.DeployStatusRequest{})
	if len(resp.GetDeployments()) != 2 {
		t.Errorf("no-filter deployments = %d, want 2", len(resp.GetDeployments()))
	}
	if len(resp.GetReplicas()) != 4 {
		t.Errorf("no-filter replicas = %d, want 4", len(resp.GetReplicas()))
	}
	if len(resp.GetRoutes()) != 3 {
		t.Errorf("no-filter routes = %d, want 3", len(resp.GetRoutes()))
	}
}

// TestDeployStatus_ObservedWithoutDesiredIsSkippedWhenFiltering — when
// a filter is in play, an observed replica with no matching desired
// entry is excluded (the lookup can't determine deployment/service).
func TestDeployStatus_ObservedWithoutDesiredIsSkippedWhenFiltering(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{Name: "app1"}, 1)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{Id: "orphan-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING}, 1)

	d := &deployServer{state: st}
	resp, err := d.Status(context.TODO(), &pb.DeployStatusRequest{DeploymentFilter: "app1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetReplicas()) != 0 {
		t.Errorf("orphan observed not filtered out: %v", resp.GetReplicas())
	}
}
