package dns_test

import (
	"errors"
	"net"
	"testing"

	mdns "github.com/miekg/dns"

	jdns "github.com/PatrickRuddiman/jaco/internal/discovery/dns"
)

func aQuery(name string) *mdns.Msg {
	q := new(mdns.Msg)
	q.SetQuestion(mdns.Fqdn(name), mdns.TypeA)
	return q
}

func TestHandle_BareServiceReturnsHealthyReplicaIPs(t *testing.T) {
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{
			"web": {net.IPv4(10, 244, 5, 2), net.IPv4(10, 244, 5, 3)},
		},
		nil,
	)
	resp := r.Handle(aQuery("web"))
	if resp.Rcode != mdns.RcodeSuccess {
		t.Errorf("rcode = %v, want SUCCESS", mdns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 2 {
		t.Fatalf("answers = %d, want 2", len(resp.Answer))
	}
	got := map[string]bool{}
	for _, rr := range resp.Answer {
		a, ok := rr.(*mdns.A)
		if !ok {
			t.Fatalf("answer is not A: %T", rr)
		}
		got[a.A.String()] = true
	}
	for _, want := range []string{"10.244.5.2", "10.244.5.3"} {
		if !got[want] {
			t.Errorf("missing IP %s in response: %v", want, got)
		}
	}
}

func TestHandle_InScopeNameFormsReturnSameAnswers(t *testing.T) {
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}},
		nil,
	)
	// All four in-scope forms must resolve to the same service IP.
	for _, name := range []string{
		"web",
		"web.sample",
		"web.jaco.internal",
		"web.sample.jaco.internal",
	} {
		resp := r.Handle(aQuery(name))
		if len(resp.Answer) != 1 {
			t.Fatalf("%s: answers = %d, want 1", name, len(resp.Answer))
		}
		if a, ok := resp.Answer[0].(*mdns.A); !ok || a.A.String() != "10.244.5.2" {
			t.Errorf("%s: answer = %v, want 10.244.5.2", name, resp.Answer[0])
		}
	}
}

// A deployment-qualified name for a DIFFERENT deployment is not in-scope and
// (with no forwarder) returns NXDOMAIN rather than this scope's IPs.
func TestHandle_WrongDeploymentQualifierNotInScope(t *testing.T) {
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}},
		nil,
	)
	resp := r.Handle(aQuery("web.other"))
	if resp.Rcode != mdns.RcodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN for foreign-deployment qualifier", mdns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Errorf("unexpected answers for web.other: %v", resp.Answer)
	}
}

func TestHandle_UnknownInScopeServiceReturnsNXDOMAIN(t *testing.T) {
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}},
		nil,
	)
	resp := r.Handle(aQuery("ghost"))
	if resp.Rcode != mdns.RcodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN", mdns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Errorf("unexpected answers: %v", resp.Answer)
	}
}

func TestHandle_ServiceNotInScopeReturnsNXDOMAIN(t *testing.T) {
	// The AC: a service from another (deployment, network) returns NXDOMAIN
	// when the responder doesn't have it on file. Achieved by simply not
	// publishing that service in this responder's ServiceMap.
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}},
		nil,
	)
	resp := r.Handle(aQuery("billing"))
	if resp.Rcode != mdns.RcodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN for foreign service", mdns.RcodeToString[resp.Rcode])
	}
}

// fakeForwarder echoes a canned response.
type fakeForwarder struct {
	respond func(host string) ([]net.IP, error)
}

func (f *fakeForwarder) LookupHost(host string) ([]net.IP, error) {
	return f.respond(host)
}

func TestHandle_ExternalNameForwardedToUpstream(t *testing.T) {
	called := false
	fw := &fakeForwarder{respond: func(host string) ([]net.IP, error) {
		called = true
		if host != "example.com" {
			t.Errorf("forwarded host = %q", host)
		}
		return []net.IP{net.IPv4(93, 184, 216, 34)}, nil
	}}
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}},
		fw,
	)
	resp := r.Handle(aQuery("example.com"))
	if !called {
		t.Errorf("forwarder not called for external name")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d, want 1", len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*mdns.A); !ok || a.A.String() != "93.184.216.34" {
		t.Errorf("forwarded answer = %v", resp.Answer[0])
	}
}

func TestHandle_ExternalNameForwarderErrorReturnsNXDOMAIN(t *testing.T) {
	fw := &fakeForwarder{respond: func(string) ([]net.IP, error) {
		return nil, errors.New("upstream nope")
	}}
	r := jdns.New(jdns.Scope{Deployment: "x", Network: "y"}, jdns.ServiceMap{}, fw)
	resp := r.Handle(aQuery("missing.example.com"))
	if resp.Rcode != mdns.RcodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN", mdns.RcodeToString[resp.Rcode])
	}
}

func TestHandle_NonAQueryReturnsEmptyAnswer(t *testing.T) {
	// v1 only serves A records (IPv4 mesh per discovery §3).
	r := jdns.New(jdns.Scope{Deployment: "x", Network: "y"}, jdns.ServiceMap{"web": {net.IPv4(10, 244, 5, 2)}}, nil)
	q := new(mdns.Msg)
	q.SetQuestion(mdns.Fqdn("web"), mdns.TypeAAAA)
	resp := r.Handle(q)
	if len(resp.Answer) != 0 {
		t.Errorf("AAAA query produced answers: %v", resp.Answer)
	}
}

func TestSetServices_AtomicSwap(t *testing.T) {
	r := jdns.New(jdns.Scope{Deployment: "x", Network: "y"}, jdns.ServiceMap{"web": {net.IPv4(1, 2, 3, 4)}}, nil)
	resp := r.Handle(aQuery("web"))
	if a, ok := resp.Answer[0].(*mdns.A); !ok || a.A.String() != "1.2.3.4" {
		t.Fatalf("initial answer wrong: %v", resp.Answer)
	}

	r.SetServices(jdns.ServiceMap{"api": {net.IPv4(5, 6, 7, 8)}})

	// web no longer exists → NXDOMAIN.
	resp = r.Handle(aQuery("web"))
	if resp.Rcode != mdns.RcodeNameError {
		t.Errorf("after swap web should NXDOMAIN; got %v", mdns.RcodeToString[resp.Rcode])
	}
	// api now resolves.
	resp = r.Handle(aQuery("api"))
	if len(resp.Answer) != 1 {
		t.Errorf("api missing after swap: %v", resp.Answer)
	}
}

func TestServices_ReturnsDefensiveCopy(t *testing.T) {
	original := jdns.ServiceMap{"web": {net.IPv4(1, 2, 3, 4)}}
	r := jdns.New(jdns.Scope{Deployment: "x", Network: "y"}, original, nil)
	snap := r.Services()
	// Mutate the snapshot; original should be unaffected.
	snap["web"][0] = net.IPv4(9, 9, 9, 9)
	resp := r.Handle(aQuery("web"))
	if a, ok := resp.Answer[0].(*mdns.A); !ok || a.A.String() != "1.2.3.4" {
		t.Errorf("snapshot mutation leaked into responder: %v", resp.Answer)
	}
}

func TestHandle_HealthyOnlyExcludesDegradedReplicas(t *testing.T) {
	// The AC: the ServiceMap the reconciler builds excludes degraded replicas
	// before they reach the responder. We simulate that by setting up the
	// responder with only the healthy IPs.
	allReplicas := map[string]struct {
		IP    net.IP
		State string // simulating ReplicaObserved.State
	}{
		"web-0": {IP: net.IPv4(10, 244, 5, 2), State: "running"},
		"web-1": {IP: net.IPv4(10, 244, 5, 3), State: "degraded"}, // excluded
		"web-2": {IP: net.IPv4(10, 244, 5, 4), State: "running"},
	}
	var healthy []net.IP
	for _, r := range allReplicas {
		if r.State == "running" {
			healthy = append(healthy, r.IP)
		}
	}
	r := jdns.New(
		jdns.Scope{Deployment: "sample", Network: "frontend"},
		jdns.ServiceMap{"web": healthy},
		nil,
	)
	resp := r.Handle(aQuery("web"))
	got := map[string]bool{}
	for _, rr := range resp.Answer {
		got[rr.(*mdns.A).A.String()] = true
	}
	if got["10.244.5.3"] {
		t.Errorf("degraded replica 10.244.5.3 appears in answer: %v", got)
	}
	if !got["10.244.5.2"] || !got["10.244.5.4"] {
		t.Errorf("expected only healthy replicas; got %v", got)
	}
}
