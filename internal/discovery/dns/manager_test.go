package dns

import (
	"bytes"
	"context"
	"log"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestScopeKey — pure helper; just confirm the deployment/network
// concatenation.
func TestScopeKey(t *testing.T) {
	cases := []struct {
		dep, net, want string
	}{
		{"app", "frontend", "app/frontend"},
		{"", "default", "/default"},
		{"x", "", "x/"},
	}
	for _, c := range cases {
		if got := scopeKey(c.dep, c.net); got != c.want {
			t.Errorf("scopeKey(%q,%q) = %q, want %q", c.dep, c.net, got, c.want)
		}
	}
}

// TestNetworksOfService — looks up service.networks by name on the
// matching Deployment entity; returns nil for missing deployment or
// missing service.
func TestNetworksOfService(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Deployments.Apply(&pb.Deployment{
		Name: "sample",
		Services: []*pb.ServiceSpec{
			{Name: "web", Networks: []string{"frontend", "shared"}},
			{Name: "api", Networks: []string{"shared"}},
		},
	}, 1)

	if got := networksOfService(st, "sample", "web"); len(got) != 2 || got[0] != "frontend" || got[1] != "shared" {
		t.Errorf("web networks = %v, want [frontend shared]", got)
	}
	if got := networksOfService(st, "sample", "api"); len(got) != 1 || got[0] != "shared" {
		t.Errorf("api networks = %v, want [shared]", got)
	}
	if got := networksOfService(st, "sample", "ghost"); got != nil {
		t.Errorf("ghost service = %v, want nil", got)
	}
	if got := networksOfService(st, "ghost", "web"); got != nil {
		t.Errorf("ghost deployment = %v, want nil", got)
	}
}

// TestResponderScope — round-trip accessor.
func TestResponderScope(t *testing.T) {
	sc := Scope{Deployment: "app", Network: "net"}
	r := New(sc, ServiceMap{}, nil)
	if got := r.Scope(); got.Deployment != "app" || got.Network != "net" {
		t.Errorf("Scope() = %+v, want %+v", got, sc)
	}
}

// TestDefaultForwarder_LoopbackResolves — net.LookupHost("localhost")
// returns at least 127.0.0.1 on every supported platform. The forwarder
// strips non-IP results.
func TestDefaultForwarder_LoopbackResolves(t *testing.T) {
	ips, err := defaultForwarder{}.LookupHost("localhost")
	if err != nil {
		// CI containers occasionally have no resolv.conf for "localhost".
		// In that case skip rather than fail; behaviour is OS-dependent.
		t.Skipf("LookupHost(localhost) errored: %v — likely no resolver in this env", err)
	}
	if len(ips) == 0 {
		t.Errorf("LookupHost(localhost) returned no IPs")
	}
	foundV4 := false
	for _, ip := range ips {
		if ip.To4() != nil && ip.IsLoopback() {
			foundV4 = true
		}
	}
	if !foundV4 {
		t.Errorf("loopback IPv4 not in forwarder result: %v", ips)
	}
}

// TestDefaultForwarder_UnresolvableSurfacesError — junk hostname
// returns an error; the forwarder propagates it (the responder uses
// the error to drop the answer).
func TestDefaultForwarder_UnresolvableSurfacesError(t *testing.T) {
	_, err := defaultForwarder{}.LookupHost("nonexistent-host-jaco-tests.invalid")
	if err == nil {
		t.Errorf("LookupHost on .invalid hostname returned nil err")
	}
}

// TestDNSHandlerFunc_WritesResponse — dnsHandlerFunc satisfies
// dns.Handler; ServeDNS writes the handler's result through the
// ResponseWriter. We use a buffer-backed fake writer.
func TestDNSHandlerFunc_WritesResponse(t *testing.T) {
	called := false
	var captured *mdns.Msg
	h := dnsHandlerFunc(func(req *mdns.Msg) *mdns.Msg {
		called = true
		captured = req
		resp := new(mdns.Msg)
		resp.SetReply(req)
		return resp
	})

	q := new(mdns.Msg)
	q.SetQuestion("example.test.", mdns.TypeA)
	w := &fakeRespWriter{}
	h.ServeDNS(w, q)
	if !called {
		t.Errorf("handler not invoked")
	}
	if captured == nil || len(captured.Question) == 0 {
		t.Errorf("request not forwarded to handler")
	}
	if !w.wroteMsg {
		t.Errorf("ServeDNS did not write the response")
	}
}

// TestDNSHandlerFunc_NilResponseSkipsWrite — when the handler returns
// nil (e.g. dropped query), ServeDNS should not call WriteMsg.
func TestDNSHandlerFunc_NilResponseSkipsWrite(t *testing.T) {
	h := dnsHandlerFunc(func(*mdns.Msg) *mdns.Msg { return nil })
	w := &fakeRespWriter{}
	h.ServeDNS(w, &mdns.Msg{})
	if w.wroteMsg {
		t.Errorf("ServeDNS wrote a message despite nil handler response")
	}
}

// TestManager_ReconcileSubnets_BadCIDRSkipped — the manager logs and
// skips subnets whose CIDR doesn't parse; no listener is recorded.
func TestManager_ReconcileSubnets_BadCIDRSkipped(t *testing.T) {
	st := state.New(watch.NewRegistry())
	st.Subnets.Apply(&pb.Subnet{
		Deployment: "app", Network: "frontend", Cidr: "not-a-cidr",
	}, 1)

	var logBuf bytes.Buffer
	m := &Manager{
		State:     st,
		Brokers:   watch.NewRegistry(),
		Logger:    log.New(&logBuf, "", 0),
		listeners: map[string]*listenerEntry{},
	}
	m.reconcileSubnets()
	if len(m.listeners) != 0 {
		t.Errorf("bad-CIDR subnet created listener; len = %d", len(m.listeners))
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("bad CIDR")) {
		t.Errorf("expected bad-CIDR log; got %q", logBuf.String())
	}
}

// TestManager_ReconcileSubnets_RemovesStaleListeners — when a subnet
// disappears from state.Subnets the manager shuts down its listeners.
// We seed a synthetic listenerEntry whose servers have no listener
// (Shutdown returns "server not started" which we ignore) and confirm
// the map entry is dropped.
func TestManager_ReconcileSubnets_RemovesStaleListeners(t *testing.T) {
	m := &Manager{
		State:     state.New(watch.NewRegistry()),
		Brokers:   watch.NewRegistry(),
		Logger:    log.New(&bytes.Buffer{}, "", 0),
		listeners: map[string]*listenerEntry{},
	}
	// Plant a fake listener for a subnet that doesn't exist in state.
	m.listeners["dead/net"] = &listenerEntry{
		scope:     Scope{Deployment: "dead", Network: "net"},
		responder: New(Scope{Deployment: "dead", Network: "net"}, ServiceMap{}, nil),
		udp:       &mdns.Server{Addr: "127.0.0.1:0", Net: "udp"},
		tcp:       &mdns.Server{Addr: "127.0.0.1:0", Net: "tcp"},
	}
	m.reconcileSubnets()
	if _, still := m.listeners["dead/net"]; still {
		t.Errorf("stale listener not removed")
	}
}

// TestManager_RefreshServiceMaps_FiltersOnHealthAndState — only RUNNING
// replicas observed within 10s of now feed the ServiceMap. Older or
// non-running observations are filtered out.
func TestManager_RefreshServiceMaps_FiltersOnHealthAndState(t *testing.T) {
	st := state.New(watch.NewRegistry())
	now := time.Now()

	// Deployment + service spec declaring which networks each service
	// reaches.
	st.Deployments.Apply(&pb.Deployment{
		Name: "app",
		Services: []*pb.ServiceSpec{{
			Name: "web", Networks: []string{"frontend"},
		}},
	}, 1)

	// Three replicas: r1 healthy, r2 too old, r3 not running.
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "r1", Deployment: "app", Service: "web", Host: "h",
	}, 2)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "r2", Deployment: "app", Service: "web", Host: "h",
	}, 3)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "r3", Deployment: "app", Service: "web", Host: "h",
	}, 4)

	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           "r1",
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.New(now),
		Details:      map[string]string{"ip": "10.42.0.5"},
	}, 5)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           "r2",
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.New(now.Add(-time.Hour)),
		Details:      map[string]string{"ip": "10.42.0.6"},
	}, 6)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           "r3",
		State:        pb.ReplicaState_REPLICA_STATE_FAILED,
		LastHealthAt: timestamppb.New(now),
		Details:      map[string]string{"ip": "10.42.0.7"},
	}, 7)

	// Plant a listener for app/frontend; refreshServiceMaps will mutate
	// its responder.
	resp := New(Scope{Deployment: "app", Network: "frontend"}, ServiceMap{}, nil)
	m := &Manager{
		State:   st,
		Brokers: watch.NewRegistry(),
		Logger:  log.New(&bytes.Buffer{}, "", 0),
		listeners: map[string]*listenerEntry{
			"app/frontend": {
				scope:     Scope{Deployment: "app", Network: "frontend"},
				responder: resp,
				udp:       &mdns.Server{},
				tcp:       &mdns.Server{},
			},
		},
	}
	m.refreshServiceMaps()

	sm := resp.Services()
	ips := sm["web"]
	if len(ips) != 1 {
		t.Fatalf("Services()[\"web\"] = %v, want one IP (r1 only)", ips)
	}
	if ips[0].String() != "10.42.0.5" {
		t.Errorf("IP = %s, want 10.42.0.5", ips[0].String())
	}
}

// TestManager_RefreshServiceMaps_SkipsMalformedIPAndMissingReplicaDesired —
// replicas whose details.ip is empty or unparseable are skipped, as
// are observed replicas with no matching ReplicasDesired entry.
func TestManager_RefreshServiceMaps_SkipsMalformedIPAndMissingReplicaDesired(t *testing.T) {
	st := state.New(watch.NewRegistry())
	now := time.Now()

	st.Deployments.Apply(&pb.Deployment{
		Name: "app",
		Services: []*pb.ServiceSpec{{Name: "web", Networks: []string{"frontend"}}},
	}, 1)
	st.ReplicasDesired.Apply(&pb.ReplicaDesired{
		Id: "r-known", Deployment: "app", Service: "web", Host: "h",
	}, 2)

	// r-known has malformed ip → skipped.
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           "r-known",
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.New(now),
		Details:      map[string]string{"ip": "not-an-ip"},
	}, 3)
	// r-orphan has no ReplicasDesired entry → skipped.
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{
		Id:           "r-orphan",
		State:        pb.ReplicaState_REPLICA_STATE_RUNNING,
		LastHealthAt: timestamppb.New(now),
		Details:      map[string]string{"ip": "10.0.0.5"},
	}, 4)

	resp := New(Scope{Deployment: "app", Network: "frontend"}, ServiceMap{"old": {net.IPv4(1, 2, 3, 4)}}, nil)
	m := &Manager{
		State:   st,
		Brokers: watch.NewRegistry(),
		Logger:  log.New(&bytes.Buffer{}, "", 0),
		listeners: map[string]*listenerEntry{
			"app/frontend": {responder: resp, udp: &mdns.Server{}, tcp: &mdns.Server{}},
		},
	}
	m.refreshServiceMaps()
	// Result should be empty (no valid replicas) — the responder's old
	// map is replaced with empty.
	if got := resp.Services(); len(got) != 0 {
		t.Errorf("Services() = %v, want empty (all replicas filtered)", got)
	}
}

// TestManager_RefreshServiceMaps_ListenerWithoutMatchingScopeClearsServices —
// when no replicas land in a scope's map but the listener exists, the
// responder's ServiceMap is set to empty (otherwise old entries would
// linger forever).
func TestManager_RefreshServiceMaps_ListenerWithoutMatchingScopeClearsServices(t *testing.T) {
	st := state.New(watch.NewRegistry())
	// No deployments / replicas at all.

	resp := New(Scope{Deployment: "app", Network: "frontend"}, ServiceMap{"stale": {net.IPv4(1, 1, 1, 1)}}, nil)
	m := &Manager{
		State:   st,
		Brokers: watch.NewRegistry(),
		Logger:  log.New(&bytes.Buffer{}, "", 0),
		listeners: map[string]*listenerEntry{
			"app/frontend": {responder: resp, udp: &mdns.Server{}, tcp: &mdns.Server{}},
		},
	}
	m.refreshServiceMaps()
	if got := resp.Services(); len(got) != 0 {
		t.Errorf("stale service not cleared: %v", got)
	}
}

// TestManager_RunReturnsOnCtxCancel — Run blocks until ctx.Done; the
// subscriptions are registered and then drained. Drives the Run
// goroutine briefly and confirms it exits cleanly.
func TestManager_RunReturnsOnCtxCancel(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	m := &Manager{
		State:   st,
		Brokers: brokers,
		Logger:  log.New(&bytes.Buffer{}, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	// Let Run hit its select.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Run did not return after ctx cancel within 2s")
	}
}

// --- helpers ---------------------------------------------------------------

// fakeRespWriter is a minimal dns.ResponseWriter for ServeDNS unit
// tests. Records whether WriteMsg was called.
type fakeRespWriter struct {
	wroteMsg bool
}

func (f *fakeRespWriter) LocalAddr() net.Addr         { return &net.UDPAddr{} }
func (f *fakeRespWriter) RemoteAddr() net.Addr        { return &net.UDPAddr{} }
func (f *fakeRespWriter) WriteMsg(*mdns.Msg) error    { f.wroteMsg = true; return nil }
func (f *fakeRespWriter) Write([]byte) (int, error)   { return 0, nil }
func (f *fakeRespWriter) Close() error                { return nil }
func (f *fakeRespWriter) TsigStatus() error           { return nil }
func (f *fakeRespWriter) TsigTimersOnly(bool)         {}
func (f *fakeRespWriter) Hijack()                     {}
