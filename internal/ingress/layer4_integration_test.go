package ingress_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/mholt/caddy-l4/layer4"
	_ "github.com/mholt/caddy-l4/modules/l4proxy"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
)

// TestIntegration_Layer4ConfigLoadsInCaddy proves the apps.layer4 block
// BuildCaddyConfig emits is schema-valid and provisions in caddy with the
// caddy-l4 modules registered — the golden test alone can't catch a wrong
// json key or an unregistered module. Only the layer4 app is loaded (the
// HTTP server would bind privileged :80/:443) on a high port, with the admin
// endpoint disabled so the test doesn't contend for :2019.
func TestIntegration_Layer4ConfigLoadsInCaddy(t *testing.T) {
	tcpRoutes := []config.TCPRoute{
		{PublishedPort: 14573, Deployment: "data", Service: "db", ContainerPort: 5432},
	}
	replicas := []config.ReplicaObservedView{
		{ID: "data-db-0", Deployment: "data", Service: "db", State: "running", LastHealthAt: time.Now()},
	}
	services := map[string]config.ServiceMeta{
		config.MetaKey("data", "db"): {
			Deployment: "data", Service: "db",
			ReplicaIPs: map[string]string{"data-db-0": "127.0.0.1"},
		},
	}
	cfg, err := config.BuildCaddyConfig(nil, tcpRoutes, replicas, services, config.BuildOpts{Now: time.Now})
	if err != nil {
		t.Fatalf("BuildCaddyConfig: %v", err)
	}

	// Keep only the (verbatim) layer4 app; drop the :80/:443 HTTP server and
	// disable admin so the load needs no privileged ports nor :2019.
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	layer4 := m["apps"].(map[string]any)["layer4"]
	if layer4 == nil {
		t.Fatalf("config has no apps.layer4: %s", cfg)
	}
	m["apps"] = map[string]any{"layer4": layer4}
	m["admin"] = map[string]any{"disabled": true}
	loadable, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := caddy.Load(loadable, true); err != nil {
		t.Fatalf("caddy.Load layer4 config: %v", err)
	}
	t.Cleanup(func() { _ = caddy.Stop() })
}
