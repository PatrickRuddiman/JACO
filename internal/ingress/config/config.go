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
	type domainEntry struct {
		domain string
		routes []Route
	}
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
		match["path"] = []any{pathGlob(route.Path)}
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
		"match": []any{map[string]any{"path": []any{pathGlob(route.Path)}}},
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

// pathGlob converts a path prefix like "/api/" into the Caddy path glob
// "/api/*". A path that already ends in "*" or "/" is handled correctly:
// if it ends in "/" we append "*"; otherwise we use it as-is.
func pathGlob(path string) string {
	if strings.HasSuffix(path, "*") {
		return path
	}
	if strings.HasSuffix(path, "/") {
		return path + "*"
	}
	return path + "/*"
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
func buildTLSPolicies(routes []Route, opts BuildOpts) []any {
	var domains []string
	for _, r := range routes {
		if r.TLSAuto {
			domains = append(domains, r.Domain)
		}
	}
	if len(domains) == 0 {
		return nil
	}
	sort.Strings(domains)
	subjects := make([]any, 0, len(domains))
	for _, d := range domains {
		subjects = append(subjects, d)
	}
	issuer := map[string]any{
		"module": "acme",
		"ca":     opts.ACMECA,
		"challenges": map[string]any{
			"http": map[string]any{},
		},
	}
	if opts.ACMEEmail != "" {
		issuer["email"] = opts.ACMEEmail
	}
	return []any{
		map[string]any{
			"subjects": subjects,
			"issuers":  []any{issuer},
			"key_type": "p256",
			"storage":  map[string]any{"module": "jaco"},
		},
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
