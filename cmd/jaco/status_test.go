package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// fakeDeployStatusClient implements just the Status method.
type fakeDeployStatusClient struct {
	pb.DeployClient
	statusFn func(ctx context.Context, req *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error)
}

func (f *fakeDeployStatusClient) Status(ctx context.Context, req *pb.DeployStatusRequest, _ ...grpc.CallOption) (*pb.DeployStatusResponse, error) {
	return f.statusFn(ctx, req)
}

func TestRunStatus_RendersThreeTables(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Deployments: []*pb.Deployment{{Name: "sample", AppliedRevision: 2, PreviousRevision: 1, Status: pb.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE}},
				Replicas: []*pb.ReplicaObserved{{
					Id: "sample-web-0", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
					Host: "node-a", ContainerId: "c-1", LastHealthAt: timestamppb.Now(),
				}},
				Routes: []*pb.Route{{
					Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TlsAuto: true,
				}},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Deployments:", "DEPLOYMENT", "sample", "ACTIVE",
		"Replicas:", "REPLICA_ID", "sample-web-0", "RUNNING",
		"Routes:", "DOMAIN", "PATH", "web.example.com", "auto",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunStatus_RendersCertsTable(t *testing.T) {
	notAfter := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Routes: []*pb.Route{{Domain: "web.example.com", Deployment: "s", Service: "web", Port: 80, TlsAuto: true}},
				Certs: []*pb.CertState{{
					Domain:        "web.example.com",
					Environment:   "prod",
					NotAfter:      timestamppb.New(notAfter),
					LastRenewalAt: timestamppb.New(notAfter.Add(-60 * 24 * time.Hour)),
				}},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Certs:", "ENVIRONMENT", "NOT_AFTER", "LAST_RENEWAL_AT", "web.example.com", "prod", "2026-08-01"} {
		if !strings.Contains(got, want) {
			t.Errorf("certs output missing %q:\n%s", want, got)
		}
	}
}

func TestRunStatus_OmitsCertsTableWhenEmpty(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Routes: []*pb.Route{{Domain: "x", Deployment: "s", Service: "w", Port: 80}},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if strings.Contains(out.String(), "Certs:") {
		t.Errorf("Certs table rendered with no cert state:\n%s", out.String())
	}
}

// TestRunStatus_RendersPathColumn verifies that same-domain routes differing
// only by path render as distinct rows with the PATH column populated (the
// catch-all route shows an empty path). Regression for issue #174.
func TestRunStatus_RendersPathColumn(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Routes: []*pb.Route{
					{Domain: "example.com", Deployment: "app", Service: "oauth2", Port: 4180, TlsAuto: true, Path: "/oauth2"},
					{Domain: "example.com", Deployment: "app", Service: "website", Port: 8080, TlsAuto: true},
				},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"PATH", "/oauth2", "oauth2", "website"} {
		if !strings.Contains(got, want) {
			t.Errorf("path output missing %q:\n%s", want, got)
		}
	}
}

func TestRunStatus_PropagatesServerError(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return nil, errors.New("server down")
		},
	}
	if err := runStatus(context.Background(), client, "", "", io.Discard); err == nil {
		t.Errorf("expected error from server")
	}
}

// fakeWatchStream replays pre-canned SubscribeEvents.
type fakeWatchStream struct {
	grpc.ClientStream
	events []*pb.SubscribeEvent
	idx    int
	done   chan struct{}
}

func (s *fakeWatchStream) Recv() (*pb.SubscribeEvent, error) {
	if s.idx >= len(s.events) {
		<-s.done
		return nil, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *fakeWatchStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeWatchStream) Trailer() metadata.MD         { return nil }
func (s *fakeWatchStream) CloseSend() error             { return nil }
func (s *fakeWatchStream) Context() context.Context     { return context.Background() }
func (s *fakeWatchStream) SendMsg(any) error            { return nil }
func (s *fakeWatchStream) RecvMsg(any) error            { return nil }

type fakeWatchClient struct {
	pb.WatchClient
	subscribeFn func(ctx context.Context, req *pb.SubscribeRequest) (pb.Watch_SubscribeClient, error)
}

func (f *fakeWatchClient) Subscribe(ctx context.Context, req *pb.SubscribeRequest, _ ...grpc.CallOption) (pb.Watch_SubscribeClient, error) {
	return f.subscribeFn(ctx, req)
}

func TestRunStatusWatch_ReRendersOnEveryEvent(t *testing.T) {
	var statusCalls atomic.Int64
	deploy := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			statusCalls.Add(1)
			return &pb.DeployStatusResponse{
				Deployments: []*pb.Deployment{{Name: "sample"}},
			}, nil
		},
	}
	// Three events → 1 initial + 3 re-renders = 4 total snapshots.
	streamDone := make(chan struct{})
	stream := &fakeWatchStream{
		events: []*pb.SubscribeEvent{
			{Payload: &pb.SubscribeEvent_Deployment{Deployment: &pb.DeploymentEvent{Kind: pb.EventKind_EVENT_KIND_UPDATED}}},
			{Payload: &pb.SubscribeEvent_ReplicaObserved{ReplicaObserved: &pb.ReplicaObservedEvent{Kind: pb.EventKind_EVENT_KIND_UPDATED}}},
			{Payload: &pb.SubscribeEvent_Route{Route: &pb.RouteEvent{Kind: pb.EventKind_EVENT_KIND_UPDATED}}},
		},
		done: streamDone,
	}
	watchC := &fakeWatchClient{
		subscribeFn: func(_ context.Context, _ *pb.SubscribeRequest) (pb.Watch_SubscribeClient, error) {
			return stream, nil
		},
	}

	var (
		out   bytes.Buffer
		outMu sync.Mutex
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = runStatusWatch(ctx, deploy, watchC, "sample", "", &lockedWriter{w: &out, mu: &outMu})
	}()

	// Wait for the 4th snapshot (1 initial + 3 events).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if statusCalls.Load() >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(streamDone) // unblock the fake stream's Recv after events exhausted
	if got := statusCalls.Load(); got < 4 {
		t.Fatalf("statusCalls = %d, want >= 4 (1 initial + 3 events)", got)
	}
	outMu.Lock()
	defer outMu.Unlock()
	if got := strings.Count(out.String(), "---"); got < 3 {
		t.Errorf("snapshot separators = %d, want >= 3", got)
	}
}

// lockedWriter serializes writes from the runStatusWatch goroutine vs. the
// test goroutine reading via String().
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestRunStatus_RendersDeploymentDetailsReason — a PENDING deployment carries
// the scheduler's reason in status_details; it must surface in the DETAILS
// column so the operator sees why nothing scheduled (the original report:
// status read ACTIVE/empty and "told me nothing"). The reason text here is the
// real string the placement layer emits when a pin matches no ready server
// (internal/scheduler/placement.PlaceReplica), not an invented placeholder.
func TestRunStatus_RendersDeploymentDetailsReason(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Deployments: []*pb.Deployment{{
					Name: "website", AppliedRevision: 6, PreviousRevision: 5,
					Status:        pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING,
					StatusDetails: map[string]string{"reason": `service "mirror": no eligible hosts`},
				}},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"DETAILS", "PENDING", "mirror", "no eligible hosts"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestRunStatus_RendersReplicaReason — failed/pending replicas must surface
// their observed failure code+message in the REASON column, while a healthy
// RUNNING replica stays blank (classify returns no code, so no noise).
func TestRunStatus_RendersReplicaReason(t *testing.T) {
	client := &fakeDeployStatusClient{
		statusFn: func(_ context.Context, _ *pb.DeployStatusRequest) (*pb.DeployStatusResponse, error) {
			return &pb.DeployStatusResponse{
				Replicas: []*pb.ReplicaObserved{
					{
						Id: "website-web-0", State: pb.ReplicaState_REPLICA_STATE_FAILED,
						ContainerId: "c-1", Code: "container_exited",
						Details: map[string]string{"exit_code": "1"},
					},
					{
						Id: "website-mirror-0", State: pb.ReplicaState_REPLICA_STATE_PENDING,
						Code: "image_pull_failed", Details: map[string]string{"reason": "manifest unknown"},
					},
					{
						Id: "website-web-1", State: pb.ReplicaState_REPLICA_STATE_RUNNING,
						Host: "node-a", ContainerId: "c-2",
					},
				},
			}, nil
		},
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), client, "", "", &out); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"REASON", "container_exited", "exit 1", "image_pull_failed", "manifest unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestStatusToView_IncludesReasonFields — the json/yaml view must carry the
// new reason fields (deployment status_details; replica code/message/details)
// and omit them for a healthy replica (omitempty), so `-o json` is as
// informative as the table.
func TestStatusToView_IncludesReasonFields(t *testing.T) {
	resp := &pb.DeployStatusResponse{
		Deployments: []*pb.Deployment{{
			Name: "website", Status: pb.DeploymentStatus_DEPLOYMENT_STATUS_PENDING,
			StatusDetails: map[string]string{"reason": `service "mirror": no eligible hosts`},
		}},
		Replicas: []*pb.ReplicaObserved{
			{
				Id: "website-web-0", State: pb.ReplicaState_REPLICA_STATE_FAILED,
				Code: "container_exited", Message: "boom", Details: map[string]string{"exit_code": "1"},
			},
			{Id: "website-web-1", State: pb.ReplicaState_REPLICA_STATE_RUNNING, Host: "node-a"},
		},
	}
	v := statusToView(resp)
	if len(v.Deployments) != 1 || v.Deployments[0].StatusDetails["reason"] != `service "mirror": no eligible hosts` {
		t.Errorf("deployment status_details not carried: %+v", v.Deployments)
	}
	if len(v.Replicas) != 2 {
		t.Fatalf("replicas = %d, want 2", len(v.Replicas))
	}
	if r := v.Replicas[0]; r.Code != "container_exited" || r.Message != "boom" || r.Details["exit_code"] != "1" {
		t.Errorf("failed replica view missing reason fields: %+v", r)
	}
	if r := v.Replicas[1]; r.Code != "" || r.Message != "" || len(r.Details) != 0 {
		t.Errorf("healthy replica should carry no reason fields: %+v", r)
	}
}
