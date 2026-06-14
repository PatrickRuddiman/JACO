package grpcsrv_test

import (
	"context"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	grpcsrv "github.com/PatrickRuddiman/jaco/internal/controlplane/grpc"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// applier returns an fsm-backed command applier over a fresh state, plus the
// state itself, for the GetRoute tests.
func newGetRouteFixture(t *testing.T) (*state.State, func(*pb.Command)) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var idx uint64
	apply := func(cmd *pb.Command) {
		idx++
		data, err := proto.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		f.Apply(&hraft.Log{Index: idx, Data: data})
	}
	return st, apply
}

func desired(id, dep, svc string) *pb.Command {
	return &pb.Command{Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
		Replica: &pb.ReplicaDesired{Id: id, Deployment: dep, Service: svc},
	}}}
}

func observed(id string, st pb.ReplicaState, lastHealth time.Time) *pb.Command {
	r := &pb.ReplicaObserved{Id: id, State: st}
	if !lastHealth.IsZero() {
		r.LastHealthAt = timestamppb.New(lastHealth)
	}
	return &pb.Command{Payload: &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{
		Replica: r,
	}}}
}

// TestGetRoute_OrdersAndCountsUpstreams covers the issue #174 verification
// surface: routes come back longest-prefix-first with the catch-all last, the
// catch_all flag is set, and ready/total upstream counts reflect replica
// health — including a fallback whose service has zero healthy replicas (the
// silent-503 case).
func TestGetRoute_OrdersAndCountsUpstreams(t *testing.T) {
	st, apply := newGetRouteFixture(t)
	now := time.Now()

	apply(&pb.Command{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
		Deployment: "app", Revision: 1,
		Routes: []*pb.Route{
			{Domain: "example.com", Deployment: "app", Service: "oauth2", Port: 4180, TlsAuto: true, Path: "/oauth2"},
			{Domain: "example.com", Deployment: "app", Service: "oauth2", Port: 4180, TlsAuto: true, Path: "/api"},
			{Domain: "example.com", Deployment: "app", Service: "website", Port: 8080, TlsAuto: true},
		},
	}}})

	// oauth2: two healthy upstreams.
	apply(desired("app-oauth2-0", "app", "oauth2"))
	apply(desired("app-oauth2-1", "app", "oauth2"))
	apply(observed("app-oauth2-0", pb.ReplicaState_REPLICA_STATE_RUNNING, now))
	apply(observed("app-oauth2-1", pb.ReplicaState_REPLICA_STATE_RUNNING, now))
	// website: one observed but stale (last health well outside the freshness
	// window) → 0 ready of 1 total, so the catch-all 503s.
	apply(desired("app-website-0", "app", "website"))
	apply(observed("app-website-0", pb.ReplicaState_REPLICA_STATE_RUNNING, now.Add(-time.Hour)))

	srv := grpcsrv.NewDeployServer(st, nil)
	resp, err := srv.GetRoute(context.Background(), &pb.GetRouteRequest{Domain: "example.com"})
	if err != nil {
		t.Fatalf("GetRoute: %v", err)
	}
	got := resp.GetRoutes()
	if len(got) != 3 {
		t.Fatalf("routes = %d, want 3", len(got))
	}

	// Order: /oauth2 (len 7), /api (len 4), catch-all last.
	if got[0].GetPath() != "/oauth2" || got[1].GetPath() != "/api" || got[2].GetPath() != "" {
		t.Errorf("order = [%q, %q, %q], want [/oauth2, /api, <catch-all>]",
			got[0].GetPath(), got[1].GetPath(), got[2].GetPath())
	}
	if got[2].GetCatchAll() != true || got[0].GetCatchAll() {
		t.Errorf("catch_all flags wrong: %v / %v", got[0].GetCatchAll(), got[2].GetCatchAll())
	}
	// oauth2 routes: 2 ready / 2 total.
	if got[0].GetReadyReplicas() != 2 || got[0].GetTotalReplicas() != 2 {
		t.Errorf("oauth2 readiness = %d/%d, want 2/2", got[0].GetReadyReplicas(), got[0].GetTotalReplicas())
	}
	// website catch-all: 0 ready / 1 total — the silent-503 fallback.
	if got[2].GetReadyReplicas() != 0 || got[2].GetTotalReplicas() != 1 {
		t.Errorf("website readiness = %d/%d, want 0/1", got[2].GetReadyReplicas(), got[2].GetTotalReplicas())
	}
	if got[2].GetService() != "website" || got[2].GetPort() != 8080 {
		t.Errorf("catch-all upstream = %s:%d, want website:8080", got[2].GetService(), got[2].GetPort())
	}
}

func TestGetRoute_UnknownDomainNotFound(t *testing.T) {
	st, _ := newGetRouteFixture(t)
	srv := grpcsrv.NewDeployServer(st, nil)
	_, err := srv.GetRoute(context.Background(), &pb.GetRouteRequest{Domain: "missing.example"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGetRoute_EmptyDomainInvalid(t *testing.T) {
	st, _ := newGetRouteFixture(t)
	srv := grpcsrv.NewDeployServer(st, nil)
	_, err := srv.GetRoute(context.Background(), &pb.GetRouteRequest{Domain: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}
