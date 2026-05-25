package grpc

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	// Register Caddy's standard modules (http, tls, reverse_proxy, acme,
	// static_response, …). Importing caddy/v2 alone only pulls the core, so
	// caddy.Load rejects every real config with "unknown module: http/tls".
	// Without this the embedded ingress never binds :80/:443 (issue #28).
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	// caddy-l4 registers the `layer4` app + `layer4.handlers.proxy` (and its
	// round-robin selection policy) so caddy.Load resolves the apps.layer4
	// block BuildCaddyConfig emits for TCP ingress (issue #37).
	_ "github.com/mholt/caddy-l4/layer4"
	_ "github.com/mholt/caddy-l4/modules/l4proxy"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ingressConfigPath is where the daemon writes the rendered Caddy config.
// Operators can repoint this with an env override in a follow-up iter.
const ingressConfigPath = "/etc/caddy/jaco.json"

// ingressBuilder is the rebuild.Builder concrete impl. Reads state.Routes
// + state.ReplicasObserved + state.Deployments, projects them into the
// config package's typed views, and calls BuildCaddyConfig.
func ingressBuilder(st *state.State, acmeEmail string) func() ([]byte, error) {
	return func() ([]byte, error) {
		var routes []config.Route
		for _, r := range st.Routes.List() {
			routes = append(routes, config.Route{
				Domain:     r.GetDomain(),
				Deployment: r.GetDeployment(),
				Service:    r.GetService(),
				Port:       int(r.GetPort()),
				TLSAuto:    r.GetTlsAuto(),
				Path:       r.GetPath(),
			})
		}

		var replicas []config.ReplicaObservedView
		for _, o := range st.ReplicasObserved.List() {
			replicas = append(replicas, config.ReplicaObservedView{
				ID:           o.GetId(),
				Deployment:   replicaIDDeployment(o.GetId(), st),
				Service:      replicaIDService(o.GetId(), st),
				State:        replicaStateString(o.GetState()),
				LastHealthAt: o.GetLastHealthAt().AsTime(),
			})
		}

		// Service metadata: replica id → overlay IP, read from the per-network
		// detail Details["ip.<dockerNetwork>"] the health watcher writes (same
		// source the DNS responder uses, issue #28). Every replica with a known
		// IP is an eligible upstream — including ones on other hosts: the WG
		// route src-hint (wgmesh) gives host-originated overlay traffic a pool
		// source so the destination host's pool→pool firewall exemption admits
		// the proxied connection. BuildCaddyConfig intersects these IPs with
		// the running+fresh replica set.
		services := map[string]config.ServiceMeta{}
		for _, obs := range st.ReplicasObserved.List() {
			rep, ok := st.ReplicasDesired.Get(obs.GetId())
			if !ok {
				continue
			}
			for _, network := range serviceNetworks(st, rep.GetDeployment(), rep.GetService()) {
				ip := obs.GetDetails()["ip."+bridge.DockerNetworkName(rep.GetDeployment(), network)]
				if ip == "" {
					continue
				}
				key := config.MetaKey(rep.GetDeployment(), rep.GetService())
				meta, ok := services[key]
				if !ok {
					meta = config.ServiceMeta{
						Deployment: rep.GetDeployment(),
						Service:    rep.GetService(),
						ReplicaIPs: map[string]string{},
					}
				}
				meta.ReplicaIPs[obs.GetId()] = ip
				services[key] = meta
			}
		}

		// TCP ingress listeners derived from state.TCPRoutes. Upstream IPs come
		// from the same `services` map as HTTP; BuildCaddyConfig dials each
		// replica's overlay IP on the container port over the WG mesh. caddy-l4
		// owns the listeners — re-loading a config with a port caddy already
		// binds is an idempotent graceful swap, so we emit every route and let
		// caddy manage the sockets (a pre-bind probe would always see caddy's
		// own listener as "in use" and drop the route on every rebuild).
		var tcpRoutes []config.TCPRoute
		for _, r := range st.TCPRoutes.List() {
			tcpRoutes = append(tcpRoutes, config.TCPRoute{
				PublishedPort: int(r.GetPublishedPort()),
				Deployment:    r.GetDeployment(),
				Service:       r.GetService(),
				ContainerPort: int(r.GetContainerPort()),
			})
		}

		cfg, err := config.BuildCaddyConfig(routes, tcpRoutes, replicas, services, config.BuildOpts{
			ACMEEmail: acmeEmail,
		})
		log.Printf("ingress: built caddy config (%d routes, %d tcp routes, %d observed replicas, %d bytes, err=%v)", len(routes), len(tcpRoutes), len(replicas), len(cfg), err)
		return cfg, err
	}
}

// ingressLoader is the rebuild.Loader concrete impl. Default mode is
// embedded — calls caddy.Load directly, no IPC, no exec (task 32
// deferral). JACO_INGRESS_EXEC=1 falls back to the v0 path that writes
// /etc/caddy/jaco.json + execs `caddy reload`, useful when the operator
// wants caddy crashes to stay isolated from jacod.
func ingressLoader() func(ctx context.Context, cfg []byte) error {
	if os.Getenv("JACO_INGRESS_EXEC") == "1" {
		return ingressLoaderExec()
	}
	return ingressLoaderEmbedded()
}

// configHasLoadableRoute reports whether the rendered config carries a real
// forwarding route — an HTTP reverse_proxy or a layer4 (TCP) server. With
// neither, the config is just the fallback 404 + ACME stub, equivalent to
// "caddy not running", so the embedded loader skips caddy.Load to avoid the
// bug-009 once-per-second admin restart loop. The apps.layer4 key is only
// present when a TCP server has upstreams, so its presence alone is loadable.
func configHasLoadableRoute(cfg []byte) bool {
	return bytes.Contains(cfg, []byte("reverse_proxy")) || bytes.Contains(cfg, []byte(`"layer4"`))
}

// shouldLoad decides whether to push cfg to caddy. Before caddy has ever
// loaded a route-bearing config we skip route-less configs so the daemon
// doesn't stand up a bare 404 stub at startup (bug-009). But once caddy is
// running we MUST load even a route-less config — otherwise deleting the last
// route never tears its listeners down and stale TCP listeners linger
// cluster-wide. The Reloader's byte-equality short-circuit keeps this to a
// single teardown load.
func shouldLoad(started bool, cfg []byte) bool {
	return started || configHasLoadableRoute(cfg)
}

// ingressLoaderEmbedded calls caddy.Load on configs that carry at least one
// forwarding route (HTTP reverse_proxy or TCP layer4), and on route-less
// configs once caddy is already running (to drain removed listeners).
func ingressLoaderEmbedded() func(ctx context.Context, cfg []byte) error {
	var started atomic.Bool
	return func(_ context.Context, cfg []byte) error {
		if !shouldLoad(started.Load(), cfg) {
			log.Printf("ingress: skipping caddy.Load (no reverse_proxy or layer4 route yet)")
			return nil
		}
		if err := caddy.Load(cfg, false); err != nil {
			log.Printf("ingress: caddy.Load FAILED: %v", err)
			return fmt.Errorf("caddy.Load: %w", err)
		}
		if started.CompareAndSwap(false, true) {
			log.Printf("ingress: caddy loaded + listening on :80/:443")
		}
		return nil
	}
}

// ingressLoaderExec is the v0 fallback: write the config to disk + exec
// `caddy reload`. Skips silently when caddy isn't on PATH.
func ingressLoaderExec() func(ctx context.Context, cfg []byte) error {
	caddyBin, _ := exec.LookPath("caddy")
	return func(ctx context.Context, cfg []byte) error {
		if caddyBin == "" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(ingressConfigPath), 0o755); err != nil {
			return fmt.Errorf("mkdir caddy config dir: %w", err)
		}
		if err := os.WriteFile(ingressConfigPath, cfg, 0o644); err != nil {
			return fmt.Errorf("write caddy config: %w", err)
		}
		cmd := exec.CommandContext(ctx, caddyBin, "reload", "--config", ingressConfigPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("caddy reload: %w: %s", err, string(out))
		}
		return nil
	}
}

// caddyAvailable reports whether the daemon can do ingress reloads —
// always true when the embedded path is on (default; caddy/v2 is
// imported), and falls back to "caddy binary on PATH" when the operator
// flips JACO_INGRESS_EXEC=1.
func caddyAvailable() bool {
	if os.Getenv("JACO_INGRESS_EXEC") == "1" {
		_, err := exec.LookPath("caddy")
		return err == nil
	}
	return true
}

func replicaStateString(s pb.ReplicaState) string {
	switch s {
	case pb.ReplicaState_REPLICA_STATE_RUNNING:
		return "running"
	case pb.ReplicaState_REPLICA_STATE_DEGRADED:
		return "degraded"
	case pb.ReplicaState_REPLICA_STATE_FAILED:
		return "failed"
	case pb.ReplicaState_REPLICA_STATE_PENDING:
		return "pending"
	}
	return ""
}

// replicaIDDeployment / replicaIDService unpack replica ids back to their
// deployment / service. ReplicaObserved doesn't carry deployment+service
// directly so we look it up via the matching ReplicaDesired entry.
func replicaIDDeployment(id string, st *state.State) string {
	if r, ok := st.ReplicasDesired.Get(id); ok {
		return r.GetDeployment()
	}
	return ""
}
func replicaIDService(id string, st *state.State) string {
	if r, ok := st.ReplicasDesired.Get(id); ok {
		return r.GetService()
	}
	return ""
}

// serviceNetworks returns the networks a service is attached to, read from
// its deployment's ServiceSpec — the key needed to look up the right
// per-network container IP in ReplicaObserved.Details.
func serviceNetworks(st *state.State, deployment, service string) []string {
	dep, ok := st.Deployments.Get(deployment)
	if !ok {
		return nil
	}
	for _, svc := range dep.GetServices() {
		if svc.GetName() == service {
			return svc.GetNetworks()
		}
	}
	return nil
}
