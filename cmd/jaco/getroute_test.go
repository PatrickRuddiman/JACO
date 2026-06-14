package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

type fakeGetRouteClient struct {
	pb.DeployClient
	fn func(ctx context.Context, req *pb.GetRouteRequest) (*pb.GetRouteResponse, error)
}

func (f *fakeGetRouteClient) GetRoute(ctx context.Context, req *pb.GetRouteRequest, _ ...grpc.CallOption) (*pb.GetRouteResponse, error) {
	return f.fn(ctx, req)
}

func sampleGetRouteResp() *pb.GetRouteResponse {
	return &pb.GetRouteResponse{
		Domain: "example.com",
		Routes: []*pb.RealizedRoute{
			{Path: "/oauth2", Deployment: "app", Service: "oauth2", Port: 4180, TlsAuto: true, ReadyReplicas: 2, TotalReplicas: 2},
			{Path: "", CatchAll: true, Deployment: "app", Service: "website", Port: 8080, TlsAuto: true, ReadyReplicas: 0, TotalReplicas: 1},
		},
	}
}

func TestRunGetRoute_RendersTable(t *testing.T) {
	client := &fakeGetRouteClient{fn: func(_ context.Context, req *pb.GetRouteRequest) (*pb.GetRouteResponse, error) {
		if req.GetDomain() != "example.com" {
			t.Errorf("domain = %q, want example.com", req.GetDomain())
		}
		return sampleGetRouteResp(), nil
	}}
	var out bytes.Buffer
	if err := runGetRoute(context.Background(), client, "example.com", &out); err != nil {
		t.Fatalf("runGetRoute: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Routes for example.com:", "PATH", "SERVICE", "FALLBACK", "READY",
		"/oauth2", "oauth2", "website", "2/2", "0/1", "yes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestGetRouteToView_MapsFields(t *testing.T) {
	v := getRouteToView(sampleGetRouteResp())
	if v.Domain != "example.com" {
		t.Errorf("domain = %q", v.Domain)
	}
	if len(v.Routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(v.Routes))
	}
	if v.Routes[0].Path != "/oauth2" || v.Routes[0].TLS != "auto" || v.Routes[0].ReadyReplicas != 2 {
		t.Errorf("route[0] = %+v", v.Routes[0])
	}
	if !v.Routes[1].CatchAll || v.Routes[1].TotalReplicas != 1 || v.Routes[1].ReadyReplicas != 0 {
		t.Errorf("route[1] = %+v", v.Routes[1])
	}
}

func TestRunGetRoute_PropagatesError(t *testing.T) {
	client := &fakeGetRouteClient{fn: func(_ context.Context, _ *pb.GetRouteRequest) (*pb.GetRouteResponse, error) {
		return nil, errors.New("not found")
	}}
	if err := runGetRoute(context.Background(), client, "missing.example", io.Discard); err == nil {
		t.Error("expected error from server")
	}
}
