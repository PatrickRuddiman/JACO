package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// withOutput sets the global --output flag for the duration of a test and
// restores it afterwards so cases don't leak into one another.
func withOutput(t *testing.T, v string) {
	t.Helper()
	prev := flagOutput
	flagOutput = v
	t.Cleanup(func() { flagOutput = prev })
}

func TestEnumString_StripsPrefixAndLowercases(t *testing.T) {
	if got := enumString("REPLICA_STATE_RUNNING", "REPLICA_STATE_"); got != "running" {
		t.Errorf("got %q, want running", got)
	}
	if got := enumString("DEPLOYMENT_STATUS_ACTIVE", "DEPLOYMENT_STATUS_"); got != "active" {
		t.Errorf("got %q, want active", got)
	}
	if got := enumString("NODE_STATUS_ISOLATION_UNAVAILABLE", "NODE_STATUS_"); got != "isolation_unavailable" {
		t.Errorf("got %q, want isolation_unavailable", got)
	}
}

func TestStatusToView_LowercaseEnumsAndTimes(t *testing.T) {
	last := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	resp := &pb.DeployStatusResponse{
		Deployments: []*pb.Deployment{{
			Name: "sample", AppliedRevision: 8, PreviousRevision: 7,
			Status: pb.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE,
		}},
		Replicas: []*pb.ReplicaObserved{{
			Id: "sample-web-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
			Host: "node-a", ContainerId: "c-1", LastHealthAt: timestamppb.New(last),
		}},
		Routes: []*pb.Route{{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TlsAuto: true, Path: "/api", StripPath: true}},
	}
	v := statusToView(resp)
	if v.Deployments[0].Status != "active" {
		t.Errorf("deployment status = %q, want active", v.Deployments[0].Status)
	}
	if v.Replicas[0].State != "running" {
		t.Errorf("replica state = %q, want running", v.Replicas[0].State)
	}
	if v.Replicas[0].LastHealthAt != "2026-06-01T12:00:00Z" {
		t.Errorf("last_health_at = %q, want RFC3339", v.Replicas[0].LastHealthAt)
	}
	if v.Routes[0].TLS != "auto" {
		t.Errorf("route tls = %q, want auto", v.Routes[0].TLS)
	}
	if v.Routes[0].Path != "/api" {
		t.Errorf("route path = %q, want /api", v.Routes[0].Path)
	}
	if !v.Routes[0].StripPath {
		t.Errorf("route strip_path = false, want true")
	}
	if v.Certs != nil {
		t.Errorf("certs should be nil when empty, got %#v", v.Certs)
	}
}

func TestRenderStatusOut_JSONParsesAndIsLowercase(t *testing.T) {
	withOutput(t, "json")
	resp := &pb.DeployStatusResponse{
		Deployments: []*pb.Deployment{{Name: "mydeploy", AppliedRevision: 8, PreviousRevision: 7, Status: pb.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE}},
		Replicas:    []*pb.ReplicaObserved{{Id: "r0", State: pb.ReplicaState_REPLICA_STATE_RUNNING, Host: "h"}},
	}
	var out bytes.Buffer
	if err := renderStatusOut(&out, resp); err != nil {
		t.Fatalf("renderStatusOut: %v", err)
	}
	var parsed statusView
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if parsed.Deployments[0].Status != "active" || parsed.Replicas[0].State != "running" {
		t.Errorf("enum casing wrong: %#v", parsed)
	}
	// Table-only header text must not appear in JSON.
	if strings.Contains(out.String(), "Deployments:") {
		t.Errorf("JSON output contains table header:\n%s", out.String())
	}
}

func TestRenderStatusOut_YAMLParses(t *testing.T) {
	withOutput(t, "yaml")
	resp := &pb.DeployStatusResponse{
		Replicas: []*pb.ReplicaObserved{{Id: "r0", State: pb.ReplicaState_REPLICA_STATE_FAILED, Host: "h"}},
	}
	var out bytes.Buffer
	if err := renderStatusOut(&out, resp); err != nil {
		t.Fatalf("renderStatusOut: %v", err)
	}
	var parsed statusView
	if err := yaml.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not valid YAML: %v\n%s", err, out.String())
	}
	if parsed.Replicas[0].State != "failed" {
		t.Errorf("replica state = %q, want failed", parsed.Replicas[0].State)
	}
}

func TestClusterStatusToView_Uninitialized(t *testing.T) {
	v := clusterStatusToView(&pb.ClusterStatusResponse{Initialized: false})
	if v.Initialized {
		t.Errorf("expected initialized=false")
	}
	if v.Nodes == nil {
		t.Errorf("nodes should be an empty slice, not nil")
	}
}

func TestClusterStatusToView_SuffrageJoin(t *testing.T) {
	resp := &pb.ClusterStatusResponse{
		Initialized: true,
		Leader:      "node-a",
		RaftIndex:   42,
		Nodes: []*pb.Node{
			{Hostname: "node-a", Address: "10.0.0.1", Status: pb.NodeStatus_NODE_STATUS_READY},
			{Hostname: "node-b", Address: "10.0.0.2", Status: pb.NodeStatus_NODE_STATUS_READY},
		},
		Suffrages: []*pb.NodeSuffrage{
			{Hostname: "node-a", Kind: pb.NodeSuffrage_KIND_VOTER},
		},
	}
	v := clusterStatusToView(resp)
	if v.Nodes[0].Suffrage != "voter" {
		t.Errorf("node-a suffrage = %q, want voter", v.Nodes[0].Suffrage)
	}
	if v.Nodes[1].Suffrage != "unknown" {
		t.Errorf("node-b suffrage = %q, want unknown", v.Nodes[1].Suffrage)
	}
	if v.Nodes[0].Status != "ready" {
		t.Errorf("node status = %q, want ready", v.Nodes[0].Status)
	}
}

func TestRunClusterStatus_JSONParses(t *testing.T) {
	withOutput(t, "json")
	client := &fakeClusterClient{
		statusFn: func(_ context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
			return &pb.ClusterStatusResponse{Initialized: true, Leader: "node-a", RaftIndex: 1}, nil
		},
	}
	var out bytes.Buffer
	if err := runClusterStatus(context.Background(), client, &out); err != nil {
		t.Fatalf("runClusterStatus: %v", err)
	}
	var parsed clusterStatusView
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out.String())
	}
	if !parsed.Initialized || parsed.Leader != "node-a" {
		t.Errorf("parsed = %#v", parsed)
	}
}

func TestNodeListToView_LowercaseStatus(t *testing.T) {
	resp := &pb.NodeListResponse{Nodes: []*pb.Node{
		{Hostname: "node-a", Address: "10.0.0.1", Status: pb.NodeStatus_NODE_STATUS_READY},
	}}
	v := nodeListToView(resp)
	if v.Nodes[0].Status != "ready" {
		t.Errorf("status = %q, want ready", v.Nodes[0].Status)
	}
}

func TestRenderNodeList_JSONParses(t *testing.T) {
	withOutput(t, "json")
	resp := &pb.NodeListResponse{Nodes: []*pb.Node{
		{Hostname: "node-a", Address: "10.0.0.1", Status: pb.NodeStatus_NODE_STATUS_JOINING},
	}}
	var out bytes.Buffer
	if err := renderNodeList(&out, resp); err != nil {
		t.Fatalf("renderNodeList: %v", err)
	}
	var parsed nodeListView
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out.String())
	}
	if parsed.Nodes[0].Status != "joining" {
		t.Errorf("status = %q, want joining", parsed.Nodes[0].Status)
	}
}
