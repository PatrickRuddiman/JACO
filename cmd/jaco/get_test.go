package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeGetClient implements the Deploy methods the get subcommands use.
type fakeGetClient struct {
	pb.DeployClient
	statusFn   func(ctx context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error)
	replicasFn func(ctx context.Context, req *pb.GetReplicasRequest) (*pb.GetReplicasResponse, error)
}

func (f *fakeGetClient) Status(ctx context.Context, req *pb.DeployStatusRequest, _ ...grpc.CallOption) (*pb.DeployStatusResponse, error) {
	return f.statusFn(ctx, req)
}

func (f *fakeGetClient) GetReplicas(ctx context.Context, req *pb.GetReplicasRequest, _ ...grpc.CallOption) (*pb.GetReplicasResponse, error) {
	return f.replicasFn(ctx, req)
}

func sampleStatusResp() *pb.DeployStatusResponse {
	return &pb.DeployStatusResponse{
		Deployments: []*pb.Deployment{{
			Name: "sample", AppliedRevision: 2, PreviousRevision: 1,
			Status:      pb.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE,
			JacoYaml:    []byte("deployment: sample\n"),
			ComposeYaml: []byte("services:\n  web:\n    image: nginx:1.27\n"),
			Services: []*pb.ServiceSpec{{
				Name: "web", Replicas: 2,
				Placement: pb.ServiceSpec_PLACEMENT_MODE_SPREAD,
			}},
		}},
		Routes: []*pb.Route{
			{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TlsAuto: true},
			{Domain: "web.example.com", Deployment: "sample", Service: "api", Port: 8080, Path: "/api", StripPath: true},
		},
	}
}

func TestRunGetDeployments_Table(t *testing.T) {
	client := &fakeGetClient{statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
		return sampleStatusResp(), nil
	}}
	var out bytes.Buffer
	flagOutput = "table"
	if err := runGetDeployments(context.Background(), client, &out); err != nil {
		t.Fatalf("runGetDeployments: %v", err)
	}
	for _, want := range []string{"Deployments:", "DEPLOYMENT", "sample", "ACTIVE", "web"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunGetDeployment_YamlIncludesSpecs(t *testing.T) {
	client := &fakeGetClient{statusFn: func(_ context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
		if req.GetDeploymentFilter() != "sample" {
			t.Errorf("filter = %q, want sample", req.GetDeploymentFilter())
		}
		return sampleStatusResp(), nil
	}}
	var out bytes.Buffer
	flagOutput = "yaml"
	defer func() { flagOutput = "table" }()
	if err := runGetDeployment(context.Background(), client, "sample", &out); err != nil {
		t.Fatalf("runGetDeployment: %v", err)
	}
	for _, want := range []string{"name: sample", "applied_revision: 2", "jaco_yaml:", "compose_yaml:", "nginx:1.27"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("yaml missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunGetDeployment_NotFound(t *testing.T) {
	client := &fakeGetClient{statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
		return &pb.DeployStatusResponse{}, nil
	}}
	var out bytes.Buffer
	flagOutput = "table"
	err := runGetDeployment(context.Background(), client, "missing", &out)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestRunGetReplica_YamlWithDependsOn(t *testing.T) {
	client := &fakeGetClient{replicasFn: func(_ context.Context, req *pb.GetReplicasRequest) (*pb.GetReplicasResponse, error) {
		if req.GetReplicaId() != "sample-web-0" {
			t.Errorf("replica_id = %q", req.GetReplicaId())
		}
		return &pb.GetReplicasResponse{Replicas: []*pb.ReplicaDetail{{
			Id: "sample-web-0", Deployment: "sample", Service: "web", Index: 0,
			State: pb.ReplicaState_REPLICA_STATE_PENDING, Host: "node-a",
			Image: "nginx:1.27", Revision: 11, RestartCount: 2,
			Code: "depends_on_unmet",
			DependsOn: []*pb.DependencyState{{
				Service: "api", Condition: "service_started",
				State: pb.ReplicaState_REPLICA_STATE_PENDING, Satisfied: false,
			}},
		}}}, nil
	}}
	var out bytes.Buffer
	flagOutput = "yaml"
	defer func() { flagOutput = "table" }()
	if err := runGetReplica(context.Background(), client, "sample-web-0", &out); err != nil {
		t.Fatalf("runGetReplica: %v", err)
	}
	for _, want := range []string{"id: sample-web-0", "state: pending", "restart_count: 2", "depends_on:", "service: api", "satisfied: false"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("yaml missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunGetReplicas_Table(t *testing.T) {
	client := &fakeGetClient{replicasFn: func(_ context.Context, _ *pb.GetReplicasRequest) (*pb.GetReplicasResponse, error) {
		return &pb.GetReplicasResponse{Replicas: []*pb.ReplicaDetail{{
			Id: "sample-web-0", Deployment: "sample", Service: "web",
			State: pb.ReplicaState_REPLICA_STATE_RUNNING, Host: "node-a",
			Image: "nginx:1.27", RestartCount: 0,
		}}}, nil
	}}
	var out bytes.Buffer
	flagOutput = "table"
	if err := runGetReplicas(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runGetReplicas: %v", err)
	}
	for _, want := range []string{"Replicas:", "REPLICA_ID", "sample-web-0", "RUNNING", "nginx:1.27"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}
