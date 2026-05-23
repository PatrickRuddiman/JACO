// Package dns is the per-bridge DNS responder used by the discovery slice.
// One Responder runs per bridge on a node, listens on the bridge's gateway
// IP (port 53 UDP+TCP), and resolves `<service>` / `<service>.jaco.local`
// to the healthy ReplicaObserved IPs for the responder's (deployment,
// network) scope. Foreign services in the scope are NXDOMAIN; external
// hostnames (anything else) get forwarded to the upstream resolver.
//
// v1 ships the pure-Go query handler `Handle`; the per-bridge network
// listeners + the watch-driven ServiceMap reconciler land alongside the
// daemon entry.
package dns

import (
	"math/rand"
	"net"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

// Scope identifies one responder's (deployment, network) world.
type Scope struct {
	Deployment string
	Network    string
}

// ServiceMap maps service name → list of A-record IPs. Only healthy replicas
// (state RUNNING + last_health_at < 10s) belong here; the reconciler is
// responsible for filtering.
type ServiceMap map[string][]net.IP

// LookupHostFn is the upstream resolver Hooks into for external hostnames.
// Production uses LookupHost (net.LookupHost-backed); tests inject a fake.
type LookupHostFn func(host string) ([]net.IP, error)

// Responder handles DNS queries within its scope. Snapshot() is called once
// per query so callers can swap the ServiceMap atomically when watch events
// fire.
type Responder struct {
	scope    Scope
	mu       sync.RWMutex
	services ServiceMap

	forwarder LookupHostFn
}

// New constructs a Responder for the given scope. forwarder=nil disables
// external-name forwarding (queries outside the scope return NXDOMAIN).
func New(scope Scope, services ServiceMap, forwarder LookupHostFn) *Responder {
	r := &Responder{scope: scope, forwarder: forwarder}
	r.SetServices(services)
	return r
}

// SetServices replaces the snapshot atomically. The watch-driven reconciler
// calls this on every ReplicaObserved change.
func (r *Responder) SetServices(m ServiceMap) {
	cp := make(ServiceMap, len(m))
	for k, v := range m {
		ips := make([]net.IP, len(v))
		for i, ip := range v {
			ips[i] = append(net.IP(nil), ip...)
		}
		cp[k] = ips
	}
	r.mu.Lock()
	r.services = cp
	r.mu.Unlock()
}

// Services returns a snapshot of the current map (defensive copy).
func (r *Responder) Services() ServiceMap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := make(ServiceMap, len(r.services))
	for k, v := range r.services {
		ips := make([]net.IP, len(v))
		copy(ips, v)
		cp[k] = ips
	}
	return cp
}

// Handle answers a single dns.Msg query and returns the response. Pure-Go,
// no network IO — the listener calls Handle on each incoming packet.
func (r *Responder) Handle(req *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	for _, q := range req.Question {
		name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		switch q.Qtype {
		case dns.TypeA:
			r.answerA(resp, name, q.Name)
		default:
			// Only A records v1; AAAA + other types → NOERROR with no answer
			// (matches the discovery slice §3 — IPv4-only mesh).
		}
	}
	if len(resp.Answer) == 0 && resp.Rcode == dns.RcodeSuccess {
		// Set NXDOMAIN when nothing answered AND no upstream takeover happened.
		resp.Rcode = dns.RcodeNameError
	}
	return resp
}

// answerA appends A records for one in-scope service lookup, OR forwards to
// the upstream when the name is clearly external (contains a dot), OR
// leaves the answer empty (which Handle later upgrades to NXDOMAIN).
func (r *Responder) answerA(resp *dns.Msg, name, originalName string) {
	service, inScope := r.parseInScopeName(name)
	if inScope {
		ips := r.lookup(service)
		if len(ips) == 0 {
			// In-scope but unknown service → NXDOMAIN (set by caller when
			// resp.Answer stays empty).
			return
		}
		// Randomize order for poor-man's load balancing.
		shuffled := append([]net.IP(nil), ips...)
		rand.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		for _, ip := range shuffled {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: originalName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
				A:   ip.To4(),
			})
		}
		return
	}
	// External name — forward.
	if r.forwarder == nil {
		return
	}
	ips, err := r.forwarder(name)
	if err != nil {
		return
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: originalName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
				A:   v4,
			})
		}
	}
}

// parseInScopeName returns (service, true) when name resolves a service in
// this responder's scope. Accepts `<service>` (bare) and
// `<service>.jaco.local` (FQDN suffix).
func (r *Responder) parseInScopeName(name string) (string, bool) {
	const suffix = ".jaco.local"
	if strings.HasSuffix(name, suffix) {
		return strings.TrimSuffix(name, suffix), true
	}
	if strings.ContainsRune(name, '.') {
		// Has a dot but not our suffix → external. Forwarder handles it.
		return "", false
	}
	// Bare label — treat as in-scope.
	return name, true
}

// lookup returns the IPs for a service in scope, or nil if unknown.
func (r *Responder) lookup(service string) []net.IP {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ips, ok := r.services[service]
	if !ok {
		return nil
	}
	out := make([]net.IP, len(ips))
	copy(out, ips)
	return out
}

// Scope returns the responder's (deployment, network) scope.
func (r *Responder) Scope() Scope { return r.scope }
