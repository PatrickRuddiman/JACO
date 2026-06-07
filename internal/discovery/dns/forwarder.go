package dns

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/PatrickRuddiman/jaco/internal/logging"
)

// DefaultForwarderTimeout is the per-upstream query deadline. Two seconds
// is long enough for a slow CDN nameserver and short enough that even a
// full-chain failure across two upstreams completes well inside libc's
// 5 s nameserver timeout — so a downstream resolver retries us, not
// times out (issue #165).
const DefaultForwarderTimeout = 2 * time.Second

// ErrNoUpstreams is returned by Forwarder.Lookup when the forwarder was
// constructed with an empty upstream list. Distinct from a real network
// failure so the caller (Responder.answerA) can map it to SERVFAIL
// without spamming "DNS failed" — the operator already saw the startup
// warning that no upstreams are configured.
var ErrNoUpstreams = errors.New("dns: no upstream resolvers configured")

// Forwarder is the production LookupHostFn for the per-bridge Responder.
// Owned by the daemon; one instance shared across every per-bridge
// Responder so the upstream list and dns.Client are reused.
//
// Why we don't just use net.LookupHost (the v0.3.5 behavior): that path
// goes through Go's default resolver which reads /etc/resolv.conf at the
// daemon's process scope, may fall back through NSS via cgo, and inside
// a daemon binding multiple bridge gateway IPs has surprisingly long
// failure tails (issue #165). The Responder needs a deterministic
// per-query deadline that's well under libc's 5 s nameserver timeout —
// the only way to get that without ripping into Go's resolver is to
// drive miekg/dns directly against an explicit upstream list.
type Forwarder struct {
	upstreams []string      // host:port; read-only after construction
	timeout   time.Duration // per-upstream query deadline
	client    *dns.Client   // miekg/dns; reusing one client across queries reuses sockets
	log       *slog.Logger
}

// NewForwarder builds a Forwarder. upstreams MAY be empty — Lookup then
// returns ErrNoUpstreams for every call, which the caller (Responder)
// maps to a SERVFAIL. The startup wiring logs a one-line warning in
// that case so the operator isn't surprised.
//
// Each upstream is normalized to host:port (default port 53) on
// construction; subsequent Lookup calls don't re-parse.
func NewForwarder(upstreams []string, timeout time.Duration, log *slog.Logger) *Forwarder {
	if log == nil {
		log = logging.Discard()
	}
	if timeout <= 0 {
		timeout = DefaultForwarderTimeout
	}
	normalized := make([]string, 0, len(upstreams))
	for _, u := range upstreams {
		normalized = append(normalized, ensurePort(u, "53"))
	}
	return &Forwarder{
		upstreams: normalized,
		timeout:   timeout,
		client:    &dns.Client{Net: "udp", Timeout: timeout},
		log:       log,
	}
}

// Upstreams returns the normalized upstream list for diagnostics. The
// slice is owned by the Forwarder; callers MUST NOT mutate.
func (f *Forwarder) Upstreams() []string {
	return f.upstreams
}

// Lookup matches LookupHostFn. Sends an A query to each upstream in
// order, returning the first response that yields IPs. AAAA is queried
// in parallel — Responder.answerAAAA also reaches us, so making one
// trip per type halves latency for dual-stack-capable callers.
//
// Returns:
//   - (ips, nil)            — at least one A or AAAA answer landed.
//   - (nil, ErrNoUpstreams) — no upstreams configured (operator startup error).
//   - (nil, err)            — every upstream failed; err is the last upstream's error.
//
// The Responder maps every error to SERVFAIL (NOT NXDOMAIN) so
// downstream resolvers retry rather than negative-cache the name.
func (f *Forwarder) Lookup(host string) ([]net.IP, error) {
	if len(f.upstreams) == 0 {
		return nil, ErrNoUpstreams
	}

	// Per-type queries in parallel, fanout across upstreams sequentially
	// per type (first answering upstream wins per type).
	type result struct {
		ips []net.IP
		err error
	}
	out := make(chan result, 2)
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		go func(qt uint16) {
			ips, err := f.queryAcrossUpstreams(host, qt)
			out <- result{ips: ips, err: err}
		}(qtype)
	}

	var (
		all     []net.IP
		lastErr error
	)
	for i := 0; i < 2; i++ {
		r := <-out
		if r.err != nil {
			lastErr = r.err
			continue
		}
		all = append(all, r.ips...)
	}
	if len(all) > 0 {
		return all, nil
	}
	return nil, lastErr
}

// queryAcrossUpstreams walks the upstreams in order, returning the first
// NOERROR response with answers. A NOERROR-empty response (NODATA, e.g.
// no AAAA for an IPv4-only name) is treated as a successful "this name
// has nothing of that type" answer — empty slice, nil error. Any
// non-NOERROR or transport error is recorded and we move to the next
// upstream.
func (f *Forwarder) queryAcrossUpstreams(host string, qtype uint16) ([]net.IP, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(host), qtype)
	msg.RecursionDesired = true

	var lastErr error
	for _, addr := range f.upstreams {
		resp, _, err := f.client.Exchange(msg, addr)
		if err != nil {
			lastErr = fmt.Errorf("dns exchange %s @%s: %w", host, addr, err)
			f.log.Debug("forwarder upstream error", "name", host, "qtype", dns.TypeToString[qtype],
				"upstream", addr, "error", err)
			continue
		}
		// Treat SERVFAIL / REFUSED / NOTIMP as "this upstream couldn't
		// answer" → try the next. NXDOMAIN / NOERROR are authoritative
		// answers — return them (even when empty) so the responder
		// doesn't keep walking.
		switch resp.Rcode {
		case dns.RcodeServerFailure, dns.RcodeRefused, dns.RcodeNotImplemented:
			lastErr = fmt.Errorf("dns rcode %s @%s for %s", dns.RcodeToString[resp.Rcode], addr, host)
			f.log.Debug("forwarder upstream rcode", "name", host, "qtype", dns.TypeToString[qtype],
				"upstream", addr, "rcode", dns.RcodeToString[resp.Rcode])
			continue
		}
		ips := extractIPs(resp, qtype)
		return ips, nil
	}
	return nil, lastErr
}

// extractIPs pulls A or AAAA records out of a response. Skips CNAMEs and
// other auxiliary records — Go's net.LookupHost flattens these, so to
// stay shape-compatible with the existing LookupHostFn contract we do
// the same.
func extractIPs(resp *dns.Msg, qtype uint16) []net.IP {
	out := make([]net.IP, 0, len(resp.Answer))
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			if qtype == dns.TypeA && v.A != nil {
				out = append(out, v.A)
			}
		case *dns.AAAA:
			if qtype == dns.TypeAAAA && v.AAAA != nil {
				out = append(out, v.AAAA)
			}
		}
	}
	return out
}

// ensurePort returns addr with `:port` appended when addr is a bare host
// or IP. IPv6 literals are bracket-wrapped per RFC 3986. Idempotent.
func ensurePort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // already has a port
	}
	// IPv6 literal without brackets — wrap it before appending port.
	if strings.Count(addr, ":") >= 2 {
		return "[" + addr + "]:" + port
	}
	return addr + ":" + port
}

// ValidateUpstreams rejects addresses that would either cause forwarding
// loops (bridge gateways `10.244.*.1`, Docker's embedded resolver
// `127.0.0.11`) or that don't parse. Called by config.Validate on the
// operator-supplied list; ReadHostResolvers applies the same filters to
// the host resolv.conf silently. Issue #165.
func ValidateUpstreams(addrs []string) error {
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(ensurePort(a, "53"))
		if err != nil {
			return fmt.Errorf("dns.forwarders[%q]: not host[:port]: %w", a, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// Hostname is allowed; the daemon resolves it via Go's
			// startup resolver. Not a loop risk because the hostname
			// has to resolve to a real upstream's IP.
			continue
		}
		if ip.IsLoopback() && ip.To4() != nil && ip.To4()[3] == 11 {
			return fmt.Errorf("dns.forwarders[%q]: 127.0.0.11 is docker's embedded resolver; configuring it as an upstream would create a forwarding loop", a)
		}
		if isBridgeGatewayIP(ip) {
			return fmt.Errorf("dns.forwarders[%q]: 10.244.*.1 is a JACO bridge gateway; configuring it as an upstream would create a forwarding loop", a)
		}
	}
	return nil
}

// isBridgeGatewayIP reports whether ip is a /24 .1 inside JACO's default
// IPAM pool. Matches the convention from internal/discovery/bridge —
// every bridge gateway is `10.244.<n>.1`. Lives here rather than in
// internal/discovery/bridge because the bridge package's GatewayIP
// computes from a CIDR, not from a heuristic on a configured upstream.
func isBridgeGatewayIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 10 && v4[1] == 244 && v4[3] == 1
}
