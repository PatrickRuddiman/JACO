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
