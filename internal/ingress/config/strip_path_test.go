package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
)

// findRewriteStripPrefix walks a parsed Caddy config and returns the
// strip_path_prefix value of the first rewrite handler it finds, plus whether
// one exists.
func findRewriteStripPrefix(v any) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if t["handler"] == "rewrite" {
			if p, ok := t["strip_path_prefix"].(string); ok {
				return p, true
			}
		}
		for _, child := range t {
			if p, ok := findRewriteStripPrefix(child); ok {
				return p, true
			}
		}
	case []any:
		for _, child := range t {
			if p, ok := findRewriteStripPrefix(child); ok {
				return p, true
			}
		}
	}
	return "", false
}

func TestBuildCaddyConfig_PathStripPrefix(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	services := map[string]config.ServiceMeta{
		config.MetaKey("dep", "api"): {
			Deployment: "dep", Service: "api",
			ReplicaIPs: map[string]string{"dep-api-0": "10.0.0.1"},
		},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "dep-api-0", Deployment: "dep", Service: "api", State: "running", LastHealthAt: now().Add(-time.Second)},
	}
	o := config.BuildOpts{ACMEEmail: "ops@example.com", ACMEEnabled: true, Now: now}

	build := func(t *testing.T, routes []config.Route) map[string]any {
		t.Helper()
		raw, err := config.BuildCaddyConfig(routes, nil, replicas, services, o)
		if err != nil {
			t.Fatalf("BuildCaddyConfig: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return parsed
	}

	t.Run("strip true emits rewrite handler with the matched prefix", func(t *testing.T) {
		cfg := build(t, []config.Route{
			{Domain: "example.com", Deployment: "dep", Service: "api", Port: 8080, Path: "/api", StripPath: true},
		})
		prefix, ok := findRewriteStripPrefix(cfg)
		if !ok {
			t.Fatalf("expected a rewrite handler with strip_path_prefix, got none")
		}
		if prefix != "/api" {
			t.Fatalf("strip_path_prefix = %q, want %q", prefix, "/api")
		}
	})

	t.Run("strip false emits no rewrite handler", func(t *testing.T) {
		cfg := build(t, []config.Route{
			{Domain: "example.com", Deployment: "dep", Service: "api", Port: 8080, Path: "/api", StripPath: false},
		})
		if _, ok := findRewriteStripPrefix(cfg); ok {
			t.Fatalf("expected no rewrite handler when StripPath is false")
		}
	})

	t.Run("strip true with empty path emits no rewrite handler", func(t *testing.T) {
		cfg := build(t, []config.Route{
			{Domain: "example.com", Deployment: "dep", Service: "api", Port: 8080, Path: "", StripPath: true},
		})
		if _, ok := findRewriteStripPrefix(cfg); ok {
			t.Fatalf("expected no rewrite handler when Path is empty")
		}
	})
}
