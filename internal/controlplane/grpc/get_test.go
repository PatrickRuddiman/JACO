package grpcsrv

import (
	"context"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

const getComposeYAML = `services:
  web:
    image: nginx:1.27
    depends_on: [api]
  api:
    image: ghcr.io/example/api:v1
`

// seedGetState builds a two-service deployment (web depends_on api) with one
// replica each, an observation per replica, and a restart counter on web.
func seedGetState(t *testing.T, apiState pb.ReplicaState) *state.State {
	t.Helper()
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{
		Name:            "app",
		AppliedRevision: 7,
		ComposeYaml:     []byte(getComposeYAML),
	}, 1)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "app-web-0", Deployment: "app", Service: "web", Index: 0,
		Host: "node-a", Image: "nginx:1.27", RaftIndex: 11,
	}, 1)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "app-api-0", Deployment: "app", Service: "api", Index: 0,
		Host: "node-b", Image: "ghcr.io/example/api:v1", RaftIndex: 11,
	}, 1)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id: "app-web-0", State: pb.ReplicaState_REPLICA_STATE_PENDING,
		Host: "node-a", Code: "depends_on_unmet", Message: "waiting on api",
	}, 1)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id: "app-api-0", State: apiState, Host: "node-b", ContainerId: "c-api",
	}, 1)
	st.RestartCounters.Apply(&pb.RestartCounter{ReplicaId: "app-web-0", ConsecutiveFailures: 3}, 1)
	return st
}

func TestGetReplicas_JoinsDesiredObservedAndRestart(t *testing.T) {
	st := seedGetState(t, pb.ReplicaState_REPLICA_STATE_RUNNING)
	d := &deployServer{state: st}

	resp, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{ReplicaId: "app-web-0"})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	if len(resp.GetReplicas()) != 1 {
		t.Fatalf("replicas = %d, want 1", len(resp.GetReplicas()))
	}
	r := resp.GetReplicas()[0]
	if r.GetDeployment() != "app" || r.GetService() != "web" {
		t.Errorf("deployment/service = %q/%q", r.GetDeployment(), r.GetService())
	}
	if r.GetImage() != "nginx:1.27" {
		t.Errorf("image = %q, want nginx:1.27", r.GetImage())
	}
	if r.GetRevision() != 11 {
		t.Errorf("revision = %d, want 11", r.GetRevision())
	}
	if r.GetRestartCount() != 3 {
		t.Errorf("restart_count = %d, want 3", r.GetRestartCount())
	}
	if r.GetState() != pb.ReplicaState_REPLICA_STATE_PENDING {
		t.Errorf("state = %v, want PENDING", r.GetState())
	}
	if r.GetCode() != "depends_on_unmet" {
		t.Errorf("code = %q", r.GetCode())
	}
}

func TestGetReplicas_ResolvesDependsOnSatisfied(t *testing.T) {
	st := seedGetState(t, pb.ReplicaState_REPLICA_STATE_RUNNING)
	d := &deployServer{state: st}

	resp, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{ReplicaId: "app-web-0"})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	deps := resp.GetReplicas()[0].GetDependsOn()
	if len(deps) != 1 {
		t.Fatalf("depends_on = %d, want 1", len(deps))
	}
	if deps[0].GetService() != "api" {
		t.Errorf("dep service = %q, want api", deps[0].GetService())
	}
	if deps[0].GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
		t.Errorf("dep state = %v, want RUNNING", deps[0].GetState())
	}
	if !deps[0].GetSatisfied() {
		t.Errorf("dep satisfied = false, want true (api running)")
	}
}

func TestGetReplicas_DependsOnUnsatisfiedWhenDepPending(t *testing.T) {
	st := seedGetState(t, pb.ReplicaState_REPLICA_STATE_PENDING)
	d := &deployServer{state: st}

	resp, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{ReplicaId: "app-web-0"})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	deps := resp.GetReplicas()[0].GetDependsOn()
	if len(deps) != 1 {
		t.Fatalf("depends_on = %d, want 1", len(deps))
	}
	if deps[0].GetSatisfied() {
		t.Errorf("dep satisfied = true, want false (api pending)")
	}
}

func TestGetReplicas_FiltersAndSort(t *testing.T) {
	st := seedGetState(t, pb.ReplicaState_REPLICA_STATE_RUNNING)
	// A second deployment that must be excluded by the deployment filter.
	st.Deployments.Apply(&pb.Deployment{Name: "other"}, 2)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "other-web-0", Deployment: "other", Service: "web", Host: "h",
	}, 2)
	d := &deployServer{state: st}

	// No filter → all three replicas, sorted by deployment/service/index.
	all, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	if len(all.GetReplicas()) != 3 {
		t.Fatalf("replicas = %d, want 3", len(all.GetReplicas()))
	}
	if all.GetReplicas()[0].GetId() != "app-api-0" {
		t.Errorf("first replica = %q, want app-api-0 (sorted)", all.GetReplicas()[0].GetId())
	}

	// Deployment filter scopes to one deployment.
	scoped, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{DeploymentFilter: "app"})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	if len(scoped.GetReplicas()) != 2 {
		t.Errorf("scoped replicas = %d, want 2", len(scoped.GetReplicas()))
	}

	// Service filter scopes further.
	svc, err := d.GetReplicas(context.Background(), &pb.GetReplicasRequest{DeploymentFilter: "app", ServiceFilter: "api"})
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}
	if len(svc.GetReplicas()) != 1 || svc.GetReplicas()[0].GetService() != "api" {
		t.Errorf("service-filtered = %+v, want [api]", svc.GetReplicas())
	}
}
