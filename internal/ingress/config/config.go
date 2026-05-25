// Package config builds the Caddy v2 JSON configuration JACO loads at the
// ingress edge. Pure-Go — the actual `caddy.Load(...)` call lives in the
// daemon-side ingress.Ingress. Inputs come from state:
//   - Routes (state.Routes)
//   - Per-route ReplicaObserved entries filtered to healthy (state RUNNING +
//     last_health_at < HealthFreshness)
//   - ServiceMeta: per-(deployment, service) lookup of replica id → IP
//     populated by the discovery slice once container IPs are known.
//
// The output is deterministic JSON keyed alphabetically — golden-file tests
// assert byte equality.
package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// HealthFreshness is the eligibility window for upstreams. Matches the
// scheduler/health watcher's emit cadence and the ingress slice §4 rule.
const HealthFreshness = 10 * time.Second

// Route is the per-domain ingress entry the caller pulls from state.Routes.
type Route struct {
	Domain     string
	Deployment string
	Service    string
	Port       int
	TLSAuto    bool
	// Path is an optional URL path prefix (e.g. "/api/"). Default "" means
	// catch-all. Multiple routes for the same domain are emitted into one
	// Caddy host block ordered longest-prefix-first.
	Path string
}

// ReplicaObservedView is the subset of pb.ReplicaObserved BuildCaddyConfig
// needs. ID matches the corresponding ReplicaDesired.Id.
type ReplicaObservedView struct {
	ID           string
	Deployment   string
	Service      string
	State        string // "running", "degraded", etc.
	LastHealthAt time.Time
}

// ServiceMeta carries per-(deployment, service) data the runtime publishes
// once container IPs are known. ReplicaIPs maps replica ID → IP string
// (without port — the route's Port is appended).
type ServiceMeta struct {
	Deployment string
	Service    string
	ReplicaIPs map[string]string
}

// MetaKey builds the lookup key BuildCaddyConfig uses to find a service's
// meta entry. Same shape as state.ReplicasDesired's join.
func MetaKey(deployment, service string) string {
	return deployment + "/" + service
}

// BuildOpts holds the daemon-side configuration BuildCaddyConfig needs.
type BuildOpts struct {
	// ACMEEmail is the contact address ACME registers with. Empty allowed
	// (lets ACME work without a contact; some CAs may reject).
	ACMEEmail string
	// ACMECA is the ACME directory URL. Default is Let's Encrypt prod.
	ACMECA string
	// ACMEEnabled is the cluster-wide ACME switch (jacod.yaml.acme_enabled).
	// When false, BuildCaddyConfig omits the tls.automation block entirely no
	// matter how many `tls: auto` routes exist — verifiable without any
	// outbound ACME call (issue #41). The zero value is false, so the daemon
	// MUST set this true to get automation; ingressBuilder does exactly that
	// from Config.ACMEEnabledOrDefault().
	ACMEEnabled bool
	// ACMEStagingCA is the staging directory URL used for stage-first
	// issuance. Domains in StagingDomains get a separate automation policy
	// pointed here instead of ACMECA; once a domain's staging dry-run
	// succeeds the daemon drops it from StagingDomains and the next rebuild
	// re-issues it against ACMECA (prod). Empty when stage-first is off.
	ACMEStagingCA string
	// StagingDomains is the set of `tls: auto` domains currently in their
	// staging dry-run. nil/empty means every domain issues directly against
	// ACMECA (no staging in flight).
	StagingDomains map[string]bool
	// Now is the clock used for last_health_at staleness checks. Tests pin
	// it; production passes time.Now.
	Now func() time.Time
}

// BuildCaddyConfig emits the Caddy JSON for the given (routes, replicas,
// services). Returned bytes are indented JSON with alphabetically-sorted
// keys (deterministic for golden-file tests).
//
// When multiple routes share the same domain, they are grouped into a single
// Caddy host block. Within that block the path-specific routes are emitted
// longest-prefix-first (so Caddy's first-match rule picks the most specific
// path), followed by the catch-all route (empty path) as the unconditional
// final handler.
func BuildCaddyConfig(routes []Route, replicas []ReplicaObservedView, services map[string]ServiceMeta, opts BuildOpts) ([]byte, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ACMECA == "" {
		opts.ACMECA = "https://acme-v02.api.letsencrypt.org/directory"
	}

	// Index replicas by (deployment, service).
	healthyByService := map[string][]ReplicaObservedView{}
	for _, r := range replicas {
		if !isEligible(r, opts.Now()) {
			continue
		}
		key := MetaKey(r.Deployment, r.Service)
		healthyByService[key] = append(healthyByService[key], r)
	}
	// Sort replicas by id within each service.
	for k := range healthyByService {
		v := healthyByService[k]
		sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
		healthyByService[k] = v
	}

	// Group routes by domain; collect unique domains in sorted order.
	domainMap := map[string][]Route{}
	for _, r := range routes {
		domainMap[r.Domain] = append(domainMap[r.Domain], r)
	}
	domains := make([]string, 0, len(domainMap))
	for d := range domainMap {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	// sortedRoutes is used for TLS policy building.
	var sortedRoutes []Route
	for _, d := range domains {
		sortedRoutes = append(sortedRoutes, domainMap[d]...)
	}

	cfgRoutes := make([]any, 0, len(domains)+1)
	for _, domain := range domains {
		domRoutes := domainMap[domain]

		// Within this domain: separate path routes from catch-all (empty path).
		var pathRoutes []Route
		var catchAll *Route
		for i := range domRoutes {
			if domRoutes[i].Path == "" {
				r := domRoutes[i]
				catchAll = &r
			} else {
				pathRoutes = append(pathRoutes, domRoutes[i])
			}
		}
		// Sort path routes longest-prefix-first for deterministic Caddy ordering.
		sort.Slice(pathRoutes, func(i, j int) bool {
			li, lj := len(pathRoutes[i].Path), len(pathRoutes[j].Path)
			if li != lj {
				return li > lj // longer first
			}
			return pathRoutes[i].Path < pathRoutes[j].Path
		})

		if len(pathRoutes) == 0 {
			// Single route for this domain (catch-all or path-only).
			r := domRoutes[0]
			cfgRoutes = append(cfgRoutes, buildSingleRoute(domain, r, healthyByService, services))
			continue
		}

		// Multiple routes: emit a subroutes block inside a host-matched route.
		// Caddy v2: outer match=host, handle=[{handler:"subroute", routes:[...]}]
		var subRoutes []any
		for _, r := range pathRoutes {
			subRoutes = append(subRoutes, buildPathRoute(r, healthyByService, services))
		}
		if catchAll != nil {
			subRoutes = append(subRoutes, buildProxyHandle(*catchAll, healthyByService, services))
		}
		cfgRoutes = append(cfgRoutes, map[string]any{
			"match": []any{map[string]any{"host": []any{domain}}},
			"handle": []any{map[string]any{
				"handler": "subroute",
				"routes":  subRoutes,
			}},
		})
	}
	// Fallback 404 at the tail.
	cfgRoutes = append(cfgRoutes, fallbackRoute())

	tlsPolicies := buildTLSPolicies(sortedRoutes, opts)

	root := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"jaco": map[string]any{
						"listen": []any{":80", ":443"},
						"routes": cfgRoutes,
					},
				},
			},
		},
	}
	if len(tlsPolicies) > 0 {
		root["apps"].(map[string]any)["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": tlsPolicies,
			},
		}
	}

	return json.MarshalIndent(root, "", "  ")
}

// buildUpstreams returns the upstream list for a route.
func buildUpstreams(route Route, healthyByService map[string][]ReplicaObservedView, services map[string]ServiceMeta) []any {
	meta, ok := services[MetaKey(route.Deployment, route.Service)]
	var upstreams []any
	if ok {
		for _, r := range healthyByService[MetaKey(route.Deployment, route.Service)] {
			ip, hasIP := meta.ReplicaIPs[r.ID]
			if !hasIP {
				continue
			}
			upstreams = append(upstreams, map[string]any{
				"dial": fmt.Sprintf("%s:%d", ip, route.Port),
			})
		}
	}
	return upstreams
}

// buildProxyHandle returns a reverse_proxy handler object (no match).
func buildProxyHandle(route Route, healthyByService map[string][]ReplicaObservedView, services map[string]ServiceMeta) any {
	return map[string]any{
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": buildUpstreams(route, healthyByService, services),
			"load_balancing": map[string]any{
				"selection_policy": map[string]any{"policy": "random"},
				"retries":          2,
				"try_duration":     "0s",
			},
			"health_checks": map[string]any{
				"passive": map[string]any{"fail_duration": "10s"},
			},
		}},
	}
}

// buildSingleRoute emits a top-level Caddy route for a single route entry.
func buildSingleRoute(domain string, route Route, healthyByService map[string][]ReplicaObservedView, services map[string]ServiceMeta) any {
	match := map[string]any{"host": []any{domain}}
	if route.Path != "" {
		match["path"] = pathMatchers(route.Path)
	}
	return map[string]any{
		"match": []any{match},
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": buildUpstreams(route, healthyByService, services),
			"load_balancing": map[string]any{
				"selection_policy": map[string]any{"policy": "random"},
				"retries":          2,
				"try_duration":     "0s",
			},
			"health_checks": map[string]any{
				"passive": map[string]any{"fail_duration": "10s"},
			},
		}},
	}
}

// buildPathRoute emits a subroute entry for a path-prefixed route.
func buildPathRoute(route Route, healthyByService map[string][]ReplicaObservedView, services map[string]ServiceMeta) any {
	return map[string]any{
		"match": []any{map[string]any{"path": pathMatchers(route.Path)}},
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": buildUpstreams(route, healthyByService, services),
			"load_balancing": map[string]any{
				"selection_policy": map[string]any{"policy": "random"},
				"retries":          2,
				"try_duration":     "0s",
			},
			"health_checks": map[string]any{
				"passive": map[string]any{"fail_duration": "10s"},
			},
		}},
	}
}

// pathMatchers returns the Caddy path-matcher patterns covering both the
// exact prefix and everything below it. Caddy's `path` matcher accepts an
// array of patterns and matches if any one matches. The intent of
// `path: /api` is "match /api itself plus /api/anything"; a single glob
// like `/api/*` would silently miss the bare `/api` request.
//
// Trailing "*" → use as-is (operator already wrote a glob).
// Trailing "/" (e.g. "/api/") → ["/api", "/api/*"].
// Otherwise (e.g. "/api")    → ["/api", "/api/*"].
func pathMatchers(path string) []any {
	if strings.HasSuffix(path, "*") {
		return []any{path}
	}
	exact := strings.TrimSuffix(path, "/")
	return []any{exact, exact + "/*"}
}

// fallbackRoute matches anything not matched above and returns 404 with the
// Server: jaco header.
func fallbackRoute() any {
	return map[string]any{
		"handle": []any{map[string]any{
			"handler": "static_response",
			"status_code": 404,
			"headers": map[string]any{
				"Server": []any{"jaco"},
			},
		}},
	}
}

// buildTLSPolicies emits the apps.tls.automation.policies array.
// One policy per `tls: auto` route; `tls: off` routes are omitted (the
// HTTP-only listener serves them).
//
// A single domain may appear on multiple routes (path-based routing), so
// the subjects list is deduped — ACME would otherwise be asked to issue
// the same cert several times.
func buildTLSPolicies(routes []Route, opts BuildOpts) []any {
	// Cluster-wide ACME opt-out: no automation block at all, regardless of
	// per-route tls_auto. The HTTP-only listener serves :80; :443 routes get
	// no managed cert (the operator runs their own cert pipeline). This is the
	// safety net that lets `acme_enabled: false` be verified offline.
	if !opts.ACMEEnabled {
		return nil
	}
	seen := map[string]bool{}
	var prodDomains, stagingDomains []string
	for _, r := range routes {
		if !r.TLSAuto {
			continue
		}
		if seen[r.Domain] {
			continue
		}
		seen[r.Domain] = true
		// Stage-first: a domain in its staging dry-run gets a policy pointing
		// at the staging directory. All others issue against the prod CA.
		if opts.StagingDomains[r.Domain] && opts.ACMEStagingCA != "" {
			stagingDomains = append(stagingDomains, r.Domain)
		} else {
			prodDomains = append(prodDomains, r.Domain)
		}
	}
	if len(prodDomains) == 0 && len(stagingDomains) == 0 {
		return nil
	}
	sort.Strings(prodDomains)
	sort.Strings(stagingDomains)

	var policies []any
	// Staging policy first (deterministic order: staging before prod) so the
	// rendered JSON is golden-stable.
	if len(stagingDomains) > 0 {
		policies = append(policies, tlsPolicy(stagingDomains, opts.ACMEStagingCA, opts.ACMEEmail))
	}
	if len(prodDomains) > 0 {
		policies = append(policies, tlsPolicy(prodDomains, opts.ACMECA, opts.ACMEEmail))
	}
	return policies
}

// tlsPolicy builds one automation policy for the given subjects + CA.
func tlsPolicy(domains []string, ca, email string) any {
	subjects := make([]any, 0, len(domains))
	for _, d := range domains {
		subjects = append(subjects, d)
	}
	issuer := map[string]any{
		"module": "acme",
		"ca":     ca,
		"challenges": map[string]any{
			"http": map[string]any{},
		},
	}
	if email != "" {
		issuer["email"] = email
	}
	return map[string]any{
		"subjects": subjects,
		"issuers":  []any{issuer},
		"key_type": "p256",
		"storage":  map[string]any{"module": "jaco"},
	}
}

// isEligible reports whether the observed replica is admissible as an
// upstream. Matches scheduler/health's emit shape: state=running and
// last_health_at within HealthFreshness.
func isEligible(r ReplicaObservedView, now time.Time) bool {
	if r.State != "running" {
		return false
	}
	if r.LastHealthAt.IsZero() {
		return false
	}
	if now.Sub(r.LastHealthAt) >= HealthFreshness {
		return false
	}
	return true
}
