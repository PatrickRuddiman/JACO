package config_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
)

func pinnedNow() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }

func opts() config.BuildOpts {
	return config.BuildOpts{ACMEEmail: "ops@example.com", Now: pinnedNow}
}

// loadGolden reads a golden fixture; if REGEN_GOLDEN=1, writes the actual
// bytes back as the new golden first.
func loadGolden(t *testing.T, name string, got []byte) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("REGEN_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", path)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return want
}

func TestBuildCaddyConfig_HealthyTwoOfThree(t *testing.T) {
	// Three replicas: two RUNNING with fresh health, one DEGRADED.
	// The AC: exactly 2 entries in the route's upstreams list.
	routes := []config.Route{
		{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 8080, TLSAuto: true},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "sample-web-0", Deployment: "sample", Service: "web", State: "running", LastHealthAt: pinnedNow().Add(-1 * time.Second)},
		{ID: "sample-web-1", Deployment: "sample", Service: "web", State: "running", LastHealthAt: pinnedNow().Add(-2 * time.Second)},
		{ID: "sample-web-2", Deployment: "sample", Service: "web", State: "degraded", LastHealthAt: pinnedNow().Add(-1 * time.Second)},
	}
	services := map[string]config.ServiceMeta{
		config.MetaKey("sample", "web"): {
			Deployment: "sample", Service: "web",
			ReplicaIPs: map[string]string{
				"sample-web-0": "10.244.5.2",
				"sample-web-1": "10.244.5.3",
				"sample-web-2": "10.244.5.4",
			},
		},
	}
	got, err := config.BuildCaddyConfig(routes, replicas, services, opts())
	if err != nil {
		t.Fatalf("BuildCaddyConfig: %v", err)
	}

	// AC: exactly 2 upstream entries in this route.
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	servers := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
	jaco := servers["jaco"].(map[string]any)
	rts := jaco["routes"].([]any)
	first := rts[0].(map[string]any)
	handle := first["handle"].([]any)[0].(map[string]any)
	upstreams := handle["upstreams"].([]any)
	if len(upstreams) != 2 {
		t.Errorf("upstreams len = %d, want 2 (degraded replica excluded)", len(upstreams))
	}

	want := loadGolden(t, "healthy-2of3.golden.json", got)
	if !bytes.Equal(got, want) {
		t.Errorf("output diverges from golden\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func TestBuildCaddyConfig_TLSOffOmitsACME(t *testing.T) {
	routes := []config.Route{
		{Domain: "internal.example.com", Deployment: "sample", Service: "web", Port: 80, TLSAuto: false},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "sample-web-0", Deployment: "sample", Service: "web", State: "running", LastHealthAt: pinnedNow()},
	}
	services := map[string]config.ServiceMeta{
		config.MetaKey("sample", "web"): {Deployment: "sample", Service: "web", ReplicaIPs: map[string]string{"sample-web-0": "10.244.5.2"}},
	}
	got, err := config.BuildCaddyConfig(routes, replicas, services, opts())
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	apps := parsed["apps"].(map[string]any)
	if _, hasTLS := apps["tls"]; hasTLS {
		t.Errorf("tls automation appears for tls=off route: %v", apps["tls"])
	}

	want := loadGolden(t, "tls-off.golden.json", got)
	if !bytes.Equal(got, want) {
		t.Errorf("output diverges from golden\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func TestBuildCaddyConfig_ZeroRoutesProducesFallbackOnly(t *testing.T) {
	got, err := config.BuildCaddyConfig(nil, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	jaco := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)
	routes := jaco["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %d, want 1 (just fallback)", len(routes))
	}
	handle := routes[0].(map[string]any)["handle"].([]any)[0].(map[string]any)
	if got := handle["handler"]; got != "static_response" {
		t.Errorf("fallback handler = %v, want static_response", got)
	}
	if got := handle["status_code"]; got != float64(404) {
		t.Errorf("fallback status_code = %v, want 404", got)
	}

	want := loadGolden(t, "zero-routes.golden.json", got)
	if !bytes.Equal(got, want) {
		t.Errorf("output diverges from golden\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func TestBuildCaddyConfig_StaleHealthExcludesUpstream(t *testing.T) {
	routes := []config.Route{
		{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TLSAuto: true},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "sample-web-0", Deployment: "sample", Service: "web", State: "running", LastHealthAt: pinnedNow().Add(-1 * time.Second)},
		{ID: "sample-web-1", Deployment: "sample", Service: "web", State: "running", LastHealthAt: pinnedNow().Add(-30 * time.Second)},
	}
	services := map[string]config.ServiceMeta{
		config.MetaKey("sample", "web"): {Deployment: "sample", Service: "web", ReplicaIPs: map[string]string{
			"sample-web-0": "10.244.5.2", "sample-web-1": "10.244.5.3",
		}},
	}
	got, err := config.BuildCaddyConfig(routes, replicas, services, opts())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(got, &parsed)
	servers := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
	jaco := servers["jaco"].(map[string]any)
	rts := jaco["routes"].([]any)
	upstreams := rts[0].(map[string]any)["handle"].([]any)[0].(map[string]any)["upstreams"].([]any)
	if len(upstreams) != 1 {
		t.Errorf("upstreams = %d, want 1 (stale-health replica excluded)", len(upstreams))
	}
	if upstreams[0].(map[string]any)["dial"] != "10.244.5.2:80" {
		t.Errorf("upstream dial = %v", upstreams[0])
	}
}

func TestBuildCaddyConfig_MissingServiceMetaProducesEmptyUpstreams(t *testing.T) {
	routes := []config.Route{
		{Domain: "web.example.com", Deployment: "sample", Service: "ghost", Port: 80, TLSAuto: true},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(got, &parsed)
	rts := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)["routes"].([]any)
	// Route exists; upstreams empty.
	first := rts[0].(map[string]any)
	upstreams, _ := first["handle"].([]any)[0].(map[string]any)["upstreams"].([]any)
	if len(upstreams) != 0 {
		t.Errorf("unknown service should produce empty upstreams; got %v", upstreams)
	}
}

func TestBuildCaddyConfig_RoutesSortedByDomain(t *testing.T) {
	routes := []config.Route{
		{Domain: "z.example.com", Deployment: "z", Service: "z", Port: 80, TLSAuto: true},
		{Domain: "a.example.com", Deployment: "a", Service: "a", Port: 80, TLSAuto: true},
		{Domain: "m.example.com", Deployment: "m", Service: "m", Port: 80, TLSAuto: true},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(got, &parsed)
	rts := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)["routes"].([]any)
	hosts := []string{}
	for _, r := range rts[:3] { // skip fallback
		match := r.(map[string]any)["match"].([]any)[0].(map[string]any)
		hosts = append(hosts, match["host"].([]any)[0].(string))
	}
	want := []string{"a.example.com", "m.example.com", "z.example.com"}
	for i := range want {
		if hosts[i] != want[i] {
			t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], want[i])
		}
	}
}

func TestBuildCaddyConfig_PathMatcherWithCatchAll(t *testing.T) {
	// Two routes for the same domain: /api/ → api service, "" → web service.
	// Expected: one host block with subroutes, /api/* first then web catch-all.
	routes := []config.Route{
		{Domain: "jaco.sh", Deployment: "app", Service: "api", Port: 8080, TLSAuto: true, Path: "/api/"},
		{Domain: "jaco.sh", Deployment: "app", Service: "web", Port: 80, TLSAuto: true, Path: ""},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "app-api-0", Deployment: "app", Service: "api", State: "running", LastHealthAt: pinnedNow().Add(-1 * time.Second)},
		{ID: "app-web-0", Deployment: "app", Service: "web", State: "running", LastHealthAt: pinnedNow().Add(-1 * time.Second)},
	}
	services := map[string]config.ServiceMeta{
		config.MetaKey("app", "api"): {Deployment: "app", Service: "api", ReplicaIPs: map[string]string{"app-api-0": "10.0.0.1"}},
		config.MetaKey("app", "web"): {Deployment: "app", Service: "web", ReplicaIPs: map[string]string{"app-web-0": "10.0.0.2"}},
	}
	got, err := config.BuildCaddyConfig(routes, replicas, services, opts())
	if err != nil {
		t.Fatalf("BuildCaddyConfig: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rts := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)["routes"].([]any)

	// First entry should be the jaco.sh host block; last is fallback.
	if len(rts) != 2 { // 1 domain block + fallback
		t.Fatalf("routes len = %d, want 2 (1 domain + fallback)", len(rts))
	}

	domainBlock := rts[0].(map[string]any)
	// Match should be host=jaco.sh
	matches := domainBlock["match"].([]any)
	host := matches[0].(map[string]any)["host"].([]any)
	if host[0] != "jaco.sh" {
		t.Errorf("host = %v, want jaco.sh", host[0])
	}

	// Handle should be a subroute with 2 sub-routes.
	handle := domainBlock["handle"].([]any)[0].(map[string]any)
	if handle["handler"] != "subroute" {
		t.Errorf("handler = %v, want subroute", handle["handler"])
	}
	subRoutes := handle["routes"].([]any)
	if len(subRoutes) != 2 {
		t.Fatalf("subroutes len = %d, want 2 (/api/ + catch-all)", len(subRoutes))
	}

	// First subroute: path=[/api, /api/*] → api service upstream 10.0.0.1:8080.
	// Both patterns are emitted so a bare /api request also matches.
	firstSub := subRoutes[0].(map[string]any)
	firstMatch := firstSub["match"].([]any)[0].(map[string]any)
	pathMatcher := firstMatch["path"].([]any)
	if len(pathMatcher) != 2 || pathMatcher[0] != "/api" || pathMatcher[1] != "/api/*" {
		t.Errorf("first sub-route path = %v, want [/api /api/*]", pathMatcher)
	}
	firstUpstreams := firstSub["handle"].([]any)[0].(map[string]any)["upstreams"].([]any)
	if firstUpstreams[0].(map[string]any)["dial"] != "10.0.0.1:8080" {
		t.Errorf("api upstream dial = %v, want 10.0.0.1:8080", firstUpstreams[0])
	}

	// Second subroute: catch-all → web service upstream 10.0.0.2:80
	secondSub := subRoutes[1].(map[string]any)
	// Catch-all has no match key.
	if _, hasMatch := secondSub["match"]; hasMatch {
		t.Error("catch-all subroute should have no match key")
	}
	secondUpstreams := secondSub["handle"].([]any)[0].(map[string]any)["upstreams"].([]any)
	if secondUpstreams[0].(map[string]any)["dial"] != "10.0.0.2:80" {
		t.Errorf("web upstream dial = %v, want 10.0.0.2:80", secondUpstreams[0])
	}
}

func TestBuildCaddyConfig_LongestPrefixFirst(t *testing.T) {
	// Three routes for jaco.sh: /api/v2/, /api/, and catch-all.
	// Expected order in subroutes: /api/v2/* first, /api/* second, catch-all last.
	routes := []config.Route{
		{Domain: "jaco.sh", Deployment: "app", Service: "web", Port: 80, TLSAuto: true, Path: ""},
		{Domain: "jaco.sh", Deployment: "app", Service: "apiv1", Port: 8080, TLSAuto: true, Path: "/api/"},
		{Domain: "jaco.sh", Deployment: "app", Service: "apiv2", Port: 9090, TLSAuto: true, Path: "/api/v2/"},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, opts())
	if err != nil {
		t.Fatalf("BuildCaddyConfig: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(got, &parsed)
	rts := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)["routes"].([]any)
	domainBlock := rts[0].(map[string]any)
	handle := domainBlock["handle"].([]any)[0].(map[string]any)
	subRoutes := handle["routes"].([]any)
	if len(subRoutes) != 3 {
		t.Fatalf("subroutes = %d, want 3", len(subRoutes))
	}
	// First should match [/api/v2, /api/v2/*] (longest prefix first).
	firstPath := subRoutes[0].(map[string]any)["match"].([]any)[0].(map[string]any)["path"].([]any)
	if len(firstPath) != 2 || firstPath[0] != "/api/v2" || firstPath[1] != "/api/v2/*" {
		t.Errorf("first subroute path = %v, want [/api/v2 /api/v2/*]", firstPath)
	}
	// Second should match [/api, /api/*].
	secondPath := subRoutes[1].(map[string]any)["match"].([]any)[0].(map[string]any)["path"].([]any)
	if len(secondPath) != 2 || secondPath[0] != "/api" || secondPath[1] != "/api/*" {
		t.Errorf("second subroute path = %v, want [/api /api/*]", secondPath)
	}
	// Third should be catch-all (no match)
	if _, hasMatch := subRoutes[2].(map[string]any)["match"]; hasMatch {
		t.Error("third subroute (catch-all) should have no match key")
	}
}

func TestBuildCaddyConfig_ACMEPolicyShape(t *testing.T) {
	routes := []config.Route{
		{Domain: "web.example.com", Deployment: "sample", Service: "web", Port: 80, TLSAuto: true},
	}
	got, _ := config.BuildCaddyConfig(routes, nil, nil, opts())
	var parsed map[string]any
	_ = json.Unmarshal(got, &parsed)
	tls := parsed["apps"].(map[string]any)["tls"].(map[string]any)
	policies := tls["automation"].(map[string]any)["policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1", len(policies))
	}
	p := policies[0].(map[string]any)
	if got := p["key_type"]; got != "p256" {
		t.Errorf("key_type = %v, want p256", got)
	}
	if got := p["storage"].(map[string]any)["module"]; got != "jaco" {
		t.Errorf("storage.module = %v, want jaco", got)
	}
	issuer := p["issuers"].([]any)[0].(map[string]any)
	if got := issuer["module"]; got != "acme" {
		t.Errorf("issuer module = %v", got)
	}
	if got := issuer["email"]; got != "ops@example.com" {
		t.Errorf("issuer email = %v", got)
	}
	if got := issuer["ca"]; got != "https://acme-v02.api.letsencrypt.org/directory" {
		t.Errorf("issuer ca = %v", got)
	}
}

// TestBuildCaddyConfig_PathMatchesExactPrefix — a route with path "/api"
// must produce a match block that covers both the bare "/api" request and
// any "/api/..." sub-path, and must NOT match unrelated paths like
// "/other". This is the regression guard for the original pathGlob bug
// where "/api" → "/api/*" silently dropped the bare-prefix request.
func TestBuildCaddyConfig_PathMatchesExactPrefix(t *testing.T) {
	routes := []config.Route{
		{Domain: "jaco.sh", Deployment: "app", Service: "api", Port: 8080, TLSAuto: true, Path: "/api"},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, opts())
	if err != nil {
		t.Fatalf("BuildCaddyConfig: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rts := parsed["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["jaco"].(map[string]any)["routes"].([]any)

	// One path route (no catch-all) lands in a subroute block under the
	// host-matched outer route. Walk in to the inner path matcher.
	outer := rts[0].(map[string]any)
	hostMatch := outer["match"].([]any)[0].(map[string]any)
	if hostMatch["host"].([]any)[0] != "jaco.sh" {
		t.Fatalf("outer host = %v, want jaco.sh", hostMatch["host"])
	}
	sub := outer["handle"].([]any)[0].(map[string]any)["routes"].([]any)[0].(map[string]any)
	patterns := sub["match"].([]any)[0].(map[string]any)["path"].([]any)
	if len(patterns) != 2 {
		t.Fatalf("path patterns = %d, want 2 (exact + sub-path)", len(patterns))
	}

	// Build a tiny matcher harness that mirrors how Caddy treats the array:
	// match if any pattern is the exact request path, or a glob ending in
	// "/*" whose prefix matches the request path.
	matchesAny := func(reqPath string) bool {
		for _, p := range patterns {
			s := p.(string)
			if strings.HasSuffix(s, "/*") {
				prefix := strings.TrimSuffix(s, "/*")
				if strings.HasPrefix(reqPath, prefix+"/") {
					return true
				}
				continue
			}
			if s == reqPath {
				return true
			}
		}
		return false
	}

	if !matchesAny("/api") {
		t.Errorf("expected /api to match, patterns = %v", patterns)
	}
	if !matchesAny("/api/foo") {
		t.Errorf("expected /api/foo to match, patterns = %v", patterns)
	}
	if matchesAny("/other") {
		t.Errorf("expected /other NOT to match, patterns = %v", patterns)
	}
}
