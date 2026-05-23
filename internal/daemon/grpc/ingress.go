package grpc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
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

		// Service metadata: replica id → IP. Until the discovery slice
		// publishes IPs into state, this is empty — Caddy will route to
		// the deployment by hostname (deployment_service.jaco.internal)
		// which docker DNS resolves once bridges are up.
		services := map[string]config.ServiceMeta{}

		return config.BuildCaddyConfig(routes, replicas, services, config.BuildOpts{
			ACMEEmail: acmeEmail,
		})
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

// ingressLoaderEmbedded calls caddy.Load on configs that carry at least
// one reverse_proxy route. With zero routes the rendered config is just
// the fallback 404 + ACME stub — equivalent to "caddy not running" —
// so we skip Load entirely to avoid the bug-009 once-per-second admin
// restart loop. Once a Route lands in state.Routes, subsequent loads
// fire normally.
func ingressLoaderEmbedded() func(ctx context.Context, cfg []byte) error {
	var started atomic.Bool
	return func(_ context.Context, cfg []byte) error {
		if !bytes.Contains(cfg, []byte("reverse_proxy")) {
			return nil
		}
		if err := caddy.Load(cfg, false); err != nil {
			return fmt.Errorf("caddy.Load: %w", err)
		}
		started.Store(true)
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
