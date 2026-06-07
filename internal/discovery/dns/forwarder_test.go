package dns_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	jdns "github.com/PatrickRuddiman/jaco/internal/discovery/dns"
)

// fakeUpstream stands up a real miekg/dns server on 127.0.0.1:0 (kernel
// picks a port) and answers per the handler function. Cleanup closes it.
func fakeUpstream(t *testing.T, handler func(w dns.ResponseWriter, r *dns.Msg)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{
		PacketConn: pc,
		Handler:    dns.HandlerFunc(handler),
	}
	go srv.ActivateAndServe() //nolint:errcheck // shutdown returns the close error
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = srv.ShutdownContext(ctx)
	})
	return pc.LocalAddr().String()
}

// TestForwarder_FirstUpstreamWins is the happy path: first upstream
// answers A, the forwarder returns its IPs. Second upstream never sees
// traffic.
func TestForwarder_FirstUpstreamWins(t *testing.T) {
	var secondCalls int
	first := fakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Answer = append(msg.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.IPv4(1, 2, 3, 4),
		})
		_ = w.WriteMsg(msg)
	})
	second := fakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		secondCalls++
	})

	fwd := jdns.NewForwarder([]string{first, second}, 500*time.Millisecond, nil)
	ips, err := fwd.Lookup("example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// Both A and AAAA legs hit `first`; the A leg returns one IP, the
	// AAAA leg returns NOERROR-empty. So Lookup gives one IPv4 back.
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(1, 2, 3, 4)) {
		t.Errorf("ips = %v, want [1.2.3.4]", ips)
	}
	// `second` should never have been called — both A and AAAA were
	// answered (one with records, one with empty NOERROR) by `first`.
	if secondCalls != 0 {
		t.Errorf("second upstream called %d times; want 0 (first answered)", secondCalls)
	}
}

// TestForwarder_FailsOverToSecondUpstream pins the chain semantics: when
// the first upstream returns SERVFAIL, the forwarder moves on to the
// second instead of giving up.
func TestForwarder_FailsOverToSecondUpstream(t *testing.T) {
	first := fakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Rcode = dns.RcodeServerFailure // every query: SERVFAIL
		_ = w.WriteMsg(msg)
	})
	second := fakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(r)
		if r.Question[0].Qtype == dns.TypeA {
			msg.Answer = append(msg.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   net.IPv4(9, 9, 9, 9),
			})
		}
		_ = w.WriteMsg(msg)
	})

	fwd := jdns.NewForwarder([]string{first, second}, 500*time.Millisecond, nil)
	ips, err := fwd.Lookup("example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(9, 9, 9, 9)) {
		t.Errorf("ips = %v, want [9.9.9.9] (failover to second)", ips)
	}
}

// TestForwarder_AllUpstreamsFailReturnsError pins the SERVFAIL trigger:
// every upstream returns SERVFAIL → forwarder Lookup returns a non-nil
// error so the responder maps it to SERVFAIL (per the new contract in
// responder.go's Handle).
func TestForwarder_AllUpstreamsFailReturnsError(t *testing.T) {
	servfail := func(w dns.ResponseWriter, r *dns.Msg) {
		msg := new(dns.Msg)
		msg.SetReply(r)
		msg.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(msg)
	}
	first := fakeUpstream(t, servfail)
	second := fakeUpstream(t, servfail)

	fwd := jdns.NewForwarder([]string{first, second}, 500*time.Millisecond, nil)
	ips, err := fwd.Lookup("example.com")
	if err == nil {
		t.Fatalf("Lookup: expected error, got ips=%v", ips)
	}
	if len(ips) != 0 {
		t.Errorf("ips = %v, want []", ips)
	}
	// The error should mention "rcode SERVFAIL" so operators see the cause
	// rather than just "no upstreams worked."
	if !strings.Contains(err.Error(), "SERVFAIL") {
		t.Errorf("error %q does not mention SERVFAIL", err)
	}
}

// TestForwarder_EmptyUpstreamsReturnsErrNoUpstreams pins the no-config
// startup story: a forwarder with no upstreams returns ErrNoUpstreams
// for every Lookup, so the responder can SERVFAIL without spamming
// "DNS failed" — the operator already saw the startup warning.
func TestForwarder_EmptyUpstreamsReturnsErrNoUpstreams(t *testing.T) {
	fwd := jdns.NewForwarder(nil, 0, nil)
	_, err := fwd.Lookup("example.com")
	if !errors.Is(err, jdns.ErrNoUpstreams) {
		t.Errorf("err = %v, want ErrNoUpstreams", err)
	}
}

// TestForwarder_NormalizesUpstreamPorts pins the host[:port] handling:
// bare IPs get :53 appended; IPv6 literals get bracketed; pre-ported
// addresses pass through untouched.
func TestForwarder_NormalizesUpstreamPorts(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.1.1.1", "1.1.1.1:53"},
		{"1.1.1.1:5353", "1.1.1.1:5353"},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]:5353", "[2606:4700:4700::1111]:5353"},
	}
	for _, tc := range tests {
		fwd := jdns.NewForwarder([]string{tc.in}, 0, nil)
		got := fwd.Upstreams()
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("normalize %q: got %v, want [%s]", tc.in, got, tc.want)
		}
	}
}

// TestForwarder_TimesOutPerUpstream pins the per-upstream deadline:
// an upstream that never replies is given up on within the configured
// window, so the responder doesn't blow past libc's 5 s nameserver
// timeout.
func TestForwarder_TimesOutPerUpstream(t *testing.T) {
	silent := fakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		// Never reply.
		time.Sleep(2 * time.Second)
	})

	fwd := jdns.NewForwarder([]string{silent}, 100*time.Millisecond, nil)
	start := time.Now()
	_, err := fwd.Lookup("example.com")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	// Per-leg (A + AAAA) timeout is 100ms each running in parallel; full
	// Lookup should finish well under 500ms. Generous bound to avoid CI flake.
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed=%s, want < 500ms (per-upstream timeout not enforced)", elapsed)
	}
}

// Compile-time sanity that Lookup matches the LookupHostFn signature
// (otherwise NewForwarder + Manager.Forwarder wouldn't fit at the
// production call site).
var _ jdns.LookupHostFn = (*jdns.Forwarder)(nil).Lookup

// Build-time-only check, prevents unused-import drift if the test file
// shrinks.
var _ = fmt.Sprintf
