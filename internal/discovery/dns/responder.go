// Package dns is the per-bridge DNS responder used by the discovery slice.
// One Responder runs per bridge on a node, listens on the bridge's gateway
// IP (port 53 UDP+TCP), and resolves `<service>` / `<service>.<deployment>` /
// `<service>.jaco.internal` to the healthy ReplicaObserved IPs for the (deployment,
// network) scope. Foreign services in the scope are NXDOMAIN; external
// hostnames (anything else) get forwarded to the upstream resolver.
//
// v1 ships the pure-Go query handler `Handle`; the per-bridge network
// listeners + the watch-driven ServiceMap reconciler land alongside the
// daemon entry.
package dns

import (
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"

	"github.com/miekg/dns"

	"github.com/PatrickRuddiman/jaco/internal/logging"
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

	// Logger logs each query at DEBUG and upstream-resolver fallback failures
	// at WARN. nil → discard. Set by the Manager when it builds the responder.
	Logger *slog.Logger
}

func (r *Responder) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
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

	if len(req.Question) > 0 {
		q := req.Question[0]
		r.log().Debug("dns query received",
			"name", strings.TrimSuffix(q.Name, "."), "qtype", dns.TypeToString[q.Qtype],
			logging.KeyDeployment, r.scope.Deployment, "network", r.scope.Network)
	}

	// nameExists guards the NXDOMAIN fallback. A name that resolves (an
	// in-scope service with records, or an external name the forwarder knows)
	// but has no record of the *queried* type must answer NODATA — NOERROR
	// with an empty answer — NOT NXDOMAIN. The overlay is IPv4-only, so every
	// in-scope name has A but never AAAA; returning NXDOMAIN on the AAAA leg
	// makes dual-stack getaddrinfo (Node/musl, glibc, Go) treat the name as
	// nonexistent and fail with ENOTFOUND even though A resolves — silently
	// breaking cross-host service discovery for real apps (issue #28).
	//
	// forwarderFailed signals that an external lookup hit a transport / SERVFAIL
	// chain across every upstream. We emit SERVFAIL (not NXDOMAIN) so downstream
	// resolvers retry instead of negative-caching the name (issue #165).
	var (
		nameExists      bool
		forwarderFailed bool
	)
	for _, q := range req.Question {
		name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		switch q.Qtype {
		case dns.TypeA:
			exists, failed := r.answerA(resp, name, q.Name)
			if exists {
				nameExists = true
			}
			if failed {
				forwarderFailed = true
			}
		case dns.TypeAAAA:
			exists, failed := r.answerAAAA(resp, name, q.Name)
			if exists {
				nameExists = true
			}
			if failed {
				forwarderFailed = true
			}
		default:
			// Other types (MX, TXT, …): NODATA for existing names, NXDOMAIN
			// otherwise — never invent records.
			if r.nameResolvable(name) {
				nameExists = true
			}
		}
	}
	if len(resp.Answer) == 0 && resp.Rcode == dns.RcodeSuccess && !nameExists {
		if forwarderFailed {
			// Upstream chain failed — SERVFAIL so the downstream resolver
			// retries instead of negative-caching this name (issue #165).
			resp.Rcode = dns.RcodeServerFailure
		} else {
			// Nothing answered and the name does not exist → NXDOMAIN.
			// (An existing name with no record of the queried type stays
			// NOERROR-empty, i.e. NODATA — see nameExists above.)
			resp.Rcode = dns.RcodeNameError
		}
	}
	return resp
}

// answerA appends A records for one in-scope service lookup, OR forwards to
// the upstream when the name is clearly external (contains a dot). It returns
// whether the name exists (resolves) — so an existing name with no A answer
// becomes NODATA rather than NXDOMAIN.
func (r *Responder) answerA(resp *dns.Msg, name, originalName string) (exists, forwarderFailed bool) {
	service, inScope := r.parseInScopeName(name)
	if inScope {
		ips := r.lookup(service)
		if len(ips) == 0 {
			return false, false // in-scope but unknown service → NXDOMAIN
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
		return true, false
	}
	// External name — forward.
	if r.forwarder == nil {
		// No forwarder wired (test scaffolding); treat as NXDOMAIN, not
		// SERVFAIL — there is nothing transiently failing to retry against.
		return false, false
	}
	ips, err := r.forwarder(name)
	if err != nil {
		r.log().Warn("upstream resolver fallback failed", "name", name, "error", err)
		// Upstream chain failed (every configured upstream returned a
		// transport/SERVFAIL error). Surface as SERVFAIL via Handle's
		// forwarderFailed flag so downstream resolvers retry instead of
		// negative-caching the name. Issue #165.
		return false, true
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: originalName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
				A:   v4,
			})
		}
	}
	// name resolved upstream, even if it had no IPv4 → exists=true, NODATA
	// for AAAA-only names.
	return len(ips) > 0, false
}

// answerAAAA handles AAAA queries. In-scope names are IPv4-only, so an existing
// in-scope service yields NODATA (exists=true, no answer). External names are
// forwarded — any IPv6 addresses are returned, and a resolvable external name
// reports exists even when it has no AAAA. Returning existence here is what
// keeps dual-stack getaddrinfo working: the AAAA leg becomes NODATA, not the
// NXDOMAIN that would otherwise sink the whole lookup (issue #28).
func (r *Responder) answerAAAA(resp *dns.Msg, name, originalName string) (exists, forwarderFailed bool) {
	if _, inScope := r.parseInScopeName(name); inScope {
		// IPv4-only overlay: the name exists iff it has A records; it never has
		// AAAA, so report existence (→ NODATA) without adding answers.
		return r.nameResolvable(name), false
	}
	if r.forwarder == nil {
		return false, false
	}
	ips, err := r.forwarder(name)
	if err != nil {
		return false, true
	}
	for _, ip := range ips {
		if ip.To4() == nil { // an IPv6 address
			resp.Answer = append(resp.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: originalName, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 5},
				AAAA: ip.To16(),
			})
		}
	}
	return len(ips) > 0, false
}

// nameResolvable reports whether name resolves at all (in-scope service with
// records, or an external name the forwarder knows) — without emitting any
// records. Used to choose NODATA vs NXDOMAIN for non-A queries.
func (r *Responder) nameResolvable(name string) bool {
	if service, inScope := r.parseInScopeName(name); inScope {
		return len(r.lookup(service)) > 0
	}
	if r.forwarder == nil {
		return false
	}
	ips, err := r.forwarder(name)
	return err == nil && len(ips) > 0
}

// parseInScopeName returns (service, true) when name resolves a service in
// this responder's scope. In-scope forms: `<service>` (bare),
// `<service>.<deployment>` (when the deployment matches this scope),
// `<service>.jaco.internal`, and `<service>.<deployment>.jaco.internal`. Any
// other dotted name is external and left to the forwarder. (`.jaco.internal`
// is used rather than `.local`, which is reserved for mDNS.)
func (r *Responder) parseInScopeName(name string) (string, bool) {
	// Strip the FQDN suffix, leaving either `<service>` or
	// `<service>.<deployment>`.
	name = strings.TrimSuffix(name, ".jaco.internal")

	if !strings.ContainsRune(name, '.') {
		// Bare label — in-scope service.
		return name, true
	}
	// `<service>.<deployment>` is in-scope only when the deployment matches.
	if svc, dep, found := strings.Cut(name, "."); found && dep == r.scope.Deployment {
		return svc, true
	}
	// Any other dotted name → external. Forwarder handles it.
	return "", false
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
