package grpc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	// Register Caddy's standard modules (http, tls, reverse_proxy, acme,
	// static_response, …). Importing caddy/v2 alone only pulls the core, so
	// caddy.Load rejects every real config with "unknown module: http/tls".
	// Without this the embedded ingress never binds :80/:443 (issue #28).
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
	"github.com/PatrickRuddiman/jaco/internal/ingress/rebuild"
	"github.com/PatrickRuddiman/jaco/internal/ingress/stagefirst"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ingressConfigPath is where the daemon writes the rendered Caddy config.
// Operators can repoint this with an env override in a follow-up iter.
const ingressConfigPath = "/etc/caddy/jaco.json"

// ingressACMEOpts is the daemon-resolved ACME configuration the builder
// projects onto config.BuildOpts. Sourced from jacod.yaml (acme_email,
// acme_ca, acme_enabled).
type ingressACMEOpts struct {
	Email   string
	CA      string
	Enabled bool
	// StagingCA is the LE staging directory used for stage-first dry runs.
	// Empty disables stage-first (e.g. when the configured CA is already
	// non-prod or acme_skip_staging is set).
	StagingCA string
	// StagingDomains, when non-nil, is consulted on every rebuild for the set
	// of domains currently in their staging dry-run. The stage-first
	// controller owns this; nil means no stage-first controller is running.
	StagingDomains func() map[string]bool
}

// leProdCA / leStagingCA mirror internal/daemon/config so the grpc package
// can classify the configured directory without importing config (which
// would create an import cycle — config doesn't import grpc, but keeping the
// constants local avoids coupling the ingress wiring to the loader).
const (
	leProdCA    = "https://acme-v02.api.letsencrypt.org/directory"
	leStagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// acmeCAOrDefault returns the configured ACME directory URL, or LE prod when
// the operator left acme_ca unset.
func acmeCAOrDefault(ca string) string {
	if ca == "" {
		return leProdCA
	}
	return ca
}

// ingressCacheDir is the on-disk fallback cache path for cert blobs:
// $dataDir/ingress/cache. Empty dataDir → "" (disk fallback disabled, e.g.
// in tests that don't set DataDir).
func (s *Server) ingressCacheDir() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "ingress", "cache")
}

// embeddedIngress reports whether the daemon owns issuance in-process
// (embedded caddy). Stage-first programmatic re-issuance/reload needs the
// embedded path; JACO_INGRESS_EXEC=1 hands issuance to an external caddy that
// JACO can't drive (issue #41 Q6).
func embeddedIngress() bool { return os.Getenv("JACO_INGRESS_EXEC") != "1" }

// stageFirstInterval is how often the stage-first controller re-evaluates the
// staging set + checks for landed staging chains.
const stageFirstInterval = 5 * time.Second

// runStageFirst drives the stage-first reconcile loop until ctx cancellation.
// It reconciles on a ticker (to pick up landed staging chains) AND on every
// Routes event (so a brand-new tls:auto domain is staged BEFORE the debounced
// reload loop would otherwise render it against prod). On any staging-set
// change it forces a config rebuild so the issuer flips a domain's automation
// policy between the staging and prod directories.
func (s *Server) runStageFirst(ctx context.Context, ctrl *stagefirst.Controller, st *state.State, brokers *watch.Registry, rl *rebuild.Reloader) {
	routes := brokers.Routes.Subscribe()
	defer routes.Cancel()

	t := time.NewTicker(stageFirstInterval)
	defer t.Stop()

	reconcile := func() {
		if ctrl.Reconcile(ctx, tlsAutoDomains(st)) {
			if err := rl.Rebuild(ctx); err != nil {
				s.logger.Error("stagefirst rebuild after staging change failed",
					"subsystem", "stagefirst", "error", err)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		case <-routes.Events():
			reconcile()
		}
	}
}

// tlsAutoDomains returns the deduped set of domains with at least one
// `tls: auto` route.
func tlsAutoDomains(st *state.State) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range st.Routes.List() {
		if !r.GetTlsAuto() || seen[r.GetDomain()] {
			continue
		}
		seen[r.GetDomain()] = true
		out = append(out, r.GetDomain())
	}
	return out
}

// loadStagingChain finds the staging-issued leaf chain for a domain in the
// cert blob store. certmagic keys the blob under the CA host, so a staging
// cert's key contains "staging" + the domain. Returns (pem, true) once the
// staging cert has landed.
func loadStagingChain(st *state.State, domain string) ([]byte, bool) {
	for _, b := range st.CertBlobs.List() {
		key := b.GetKey()
		if !strings.HasSuffix(key, ".crt") {
			continue
		}
		if !strings.Contains(key, "staging") {
			continue
		}
		if !strings.Contains(key, "/"+domain+"/") {
			continue
		}
		return b.GetValue(), true
	}
	return nil, false
}

// prodCertIssued reports whether a non-staging (prod) leaf cert for the
// domain is already in the cert blob store — i.e. the domain isn't new.
func prodCertIssued(st *state.State, domain string) bool {
	for _, b := range st.CertBlobs.List() {
		key := b.GetKey()
		if !strings.HasSuffix(key, ".crt") {
			continue
		}
		if strings.Contains(key, "staging") {
			continue
		}
		if strings.Contains(key, "/"+domain+"/") {
			return true
		}
	}
	return false
}

// ingressBuilder is the rebuild.Builder concrete impl. Reads state.Routes
// + state.ReplicasObserved + state.Deployments, projects them into the
// config package's typed views, and calls BuildCaddyConfig.
func ingressBuilder(st *state.State, acme ingressACMEOpts, logger *slog.Logger) func() ([]byte, error) {
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

		var stagingDomains map[string]bool
		if acme.StagingDomains != nil {
			stagingDomains = acme.StagingDomains()
		}
		cfg, err := config.BuildCaddyConfig(routes, replicas, services, config.BuildOpts{
			ACMEEmail:      acme.Email,
			ACMECA:         acme.CA,
			ACMEEnabled:    acme.Enabled,
			ACMEStagingCA:  acme.StagingCA,
			StagingDomains: stagingDomains,
		})
		if err != nil {
			logger.Error("build caddy config failed",
				"routes", len(routes), "observed_replicas", len(replicas), "error", err)
		} else {
			logger.Debug("built caddy config",
				"routes", len(routes), "observed_replicas", len(replicas), "bytes", len(cfg))
		}
		return cfg, err
	}
}

// ingressLoader is the rebuild.Loader concrete impl. Default mode is
// embedded — calls caddy.Load directly, no IPC, no exec (task 32
// deferral). JACO_INGRESS_EXEC=1 falls back to the v0 path that writes
// /etc/caddy/jaco.json + execs `caddy reload`, useful when the operator
// wants caddy crashes to stay isolated from jacod.
func ingressLoader(logger *slog.Logger) func(ctx context.Context, cfg []byte) error {
	if os.Getenv("JACO_INGRESS_EXEC") == "1" {
		return ingressLoaderExec()
	}
	return ingressLoaderEmbedded(logger)
}

// ingressLoaderEmbedded calls caddy.Load on configs that carry at least
// one reverse_proxy route. With zero routes the rendered config is just
// the fallback 404 + ACME stub — equivalent to "caddy not running" —
// so we skip Load entirely to avoid the bug-009 once-per-second admin
// restart loop. Once a Route lands in state.Routes, subsequent loads
// fire normally.
func ingressLoaderEmbedded(logger *slog.Logger) func(ctx context.Context, cfg []byte) error {
	var started atomic.Bool
	return func(_ context.Context, cfg []byte) error {
		if !bytes.Contains(cfg, []byte("reverse_proxy")) {
			logger.Debug("skipping caddy.Load (no reverse_proxy route yet)")
			return nil
		}
		if err := caddy.Load(cfg, false); err != nil {
			logger.Error("caddy.Load failed", "error", err)
			return fmt.Errorf("caddy.Load: %w", err)
		}
		if started.CompareAndSwap(false, true) {
			logger.Info("caddy loaded and listening", "addrs", ":80/:443")
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
