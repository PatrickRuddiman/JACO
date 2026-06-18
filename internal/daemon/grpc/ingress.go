package grpc

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
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
	// caddy-l4 registers the `layer4` app + `layer4.handlers.proxy` (and its
	// round-robin selection policy) so caddy.Load resolves the apps.layer4
	// block BuildCaddyConfig emits for TCP ingress (issue #37).
	_ "github.com/mholt/caddy-l4/layer4"
	_ "github.com/mholt/caddy-l4/modules/l4proxy"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/ingress/cachepoke"
	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
	"github.com/PatrickRuddiman/jaco/internal/ingress/rebuild"
	"github.com/PatrickRuddiman/jaco/internal/ingress/stagefirst"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
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
	// of domains currently in their staging dry-run — the builder points those
	// domains' automation policy at the staging directory. On the leader this
	// unions the stage-first controller's in-flight in-memory set with the set
	// derived from replicated cert-blob state; on followers (which never run
	// the controller) it is the replicated-state set alone, so a follower
	// renders the staging policy and serves the replicated staging leaf during
	// the transient staging window (issue #182). nil means no stage-first
	// controller is running.
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
// It reconciles on a ticker (to pick up landed staging chains), on every Routes
// event (so a brand-new tls:auto domain is staged BEFORE the debounced reload
// loop would otherwise render it against prod), and on every CertBlobs event
// (so a follower flips its automation policy the moment a promotion replicates).
//
// The promotion controller is leader-gated (issue #182): only the raft leader
// stages new domains, runs the self-check, clears the cluster's staging cert
// blobs, and promotes. Followers must never run the promotion or they would
// wipe the replicated staging cert and break prod issuance cluster-wide; they
// instead render the staging-vs-prod policy from replicated state (see
// stagingDomainsForBuilder) and serve the replicated leaf. On any staging-set
// change the leader forces a config rebuild so the issuer flips a domain's
// automation policy between the staging and prod directories.
func (s *Server) runStageFirst(ctx context.Context, isLeader func() bool, ctrl *stagefirst.Controller, st *state.State, brokers *watch.Registry, rl *rebuild.Reloader) {
	routes := brokers.Routes.Subscribe()
	defer routes.Cancel()
	certBlobs := brokers.CertBlobs.Subscribe()
	defer certBlobs.Cancel()

	t := time.NewTicker(stageFirstInterval)
	defer t.Stop()

	// seenStaging tracks domains this node has observed in their staging window
	// (staging blob present, no prod blob) so the follower cache-eviction pass
	// can fire exactly once per promotion. See reconcileStagingCache.
	seenStaging := map[string]bool{}

	// loadedProd latches tls:auto domains whose replicated prod leaf this
	// follower has confirmed Caddy is serving, so the level-triggered reload
	// stops once the cert is actually loaded (not merely after one reload
	// attempt). See ensureFollowerProdCerts.
	loadedProd := map[string]bool{}

	reconcile := func() {
		leader := isLeader()

		// Cache reconcile runs on every tick regardless of role: on followers
		// it drops a stale cached staging leaf once a promotion replicates. The
		// leader evicts precisely via ClearStagingCert, so this only tracks
		// (does not evict) on the leader.
		s.reconcileStagingCache(st, seenStaging, leader)

		// Promotion is leader-only (issue #182). Followers re-render from
		// replicated CertBlobs via the rebuild Reloader's CertBlobs
		// subscription; they must not run the controller.
		if !leader {
			// A follower's automation policy is prod from the start (the
			// staging window is leader-only), so when the leader's prod leaf
			// replicates the follower's re-rendered config is byte-identical
			// and the debounced Reloader skips caddy.Load — Caddy never re-runs
			// certmagic's Manage to load the new leaf, so the follower serves
			// no TLS until a daemon restart. Force reloads (level-triggered,
			// retried until the cache actually serves the leaf) so Manage loads
			// the replicated prod cert from storage.
			s.ensureFollowerProdCerts(ctx, st, loadedProd, rl)
			return
		}
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
		case <-certBlobs.Events():
			reconcile()
		}
	}
}

// ensureFollowerProdCerts makes a follower serve the prod leaf the leader
// obtained and replicated, with no daemon restart. The leader issues the cert
// and certmagic caches it in-process on the issuing node only; a follower must
// load it from replicated storage via certmagic's Manage, which runs during
// Caddy provisioning. But a follower's automation policy is prod-from-the-start
// (the staging window is leader-only, issue #182), so when the prod blob
// replicates the follower's re-rendered config is byte-identical and the
// debounced Reloader skips caddy.Load — Manage never re-runs and the follower
// serves no TLS until a restart.
//
// This drives a LEVEL-triggered forced reload: on every reconcile tick, for
// each tls:auto domain whose COMPLETE prod resource (leaf .crt AND key .key,
// non-staging) has replicated but whose leaf is not yet in Caddy's cert cache,
// it forces one caddy.Load so Manage loads the leaf from storage. A domain is
// latched done only once the cache actually serves that exact leaf
// (followerServesProdLeaf) — so a single attempt that loses the race with
// replication (certmagic finds the blob half-written and falls back to a
// leader-locked, doomed ACME obtain) is retried next tick instead of giving up
// forever. Gating on the complete resource keeps certmagic on the load path,
// never the obtain path, so a follower never starts ACME itself.
func (s *Server) ensureFollowerProdCerts(ctx context.Context, st *state.State, loaded map[string]bool, rl *rebuild.Reloader) {
	targets := followerProdReloadTargets(st, loaded, func(d string) bool {
		return followerServesProdLeaf(st, d)
	})
	if len(targets) == 0 {
		return
	}
	if err := rl.ForceReload(ctx); err != nil {
		// Leave targets unlatched so the next tick retries.
		s.logger.Error("stagefirst follower force-reload to load replicated prod cert failed",
			"subsystem", "stagefirst", "domains", targets, "error", err)
		return
	}
	// Do NOT latch here: a later tick confirms via the cert cache that certmagic
	// actually loaded each leaf before marking it done. That confirmation is
	// what turns a one-shot edge trigger into a self-healing loop.
	s.logger.Info("stagefirst follower force-reloading to load replicated prod cert",
		"subsystem", "stagefirst", "domains", targets)
}

// followerProdReloadTargets is the pure core of ensureFollowerProdCerts: given
// replicated state, the per-promotion latch map, and a probe reporting whether
// the follower's TLS cache already serves a domain's replicated prod leaf, it
// returns the tls:auto domains still needing a forced reload and updates loaded
// in place — latching domains the cache now serves and re-arming (dropping)
// domains whose complete prod resource isn't present yet. Split out so the
// level-trigger semantics are unit-testable without a provisioned Caddy.
func followerProdReloadTargets(st *state.State, loaded map[string]bool, serves func(domain string) bool) []string {
	live := map[string]bool{}
	var targets []string
	for _, d := range tlsAutoDomains(st) {
		live[d] = true
		if !prodCertResourceComplete(st, d) {
			// New domain, mid-replication, or cleared for re-issue: forget any
			// prior latch so the next complete landing reloads again.
			delete(loaded, d)
			continue
		}
		if loaded[d] {
			continue // already confirmed serving this promotion's leaf
		}
		if serves(d) {
			loaded[d] = true // cache now serves the replicated leaf — done
			continue
		}
		targets = append(targets, d)
	}
	// Drop domains no longer tls:auto so the map stays bounded.
	for d := range loaded {
		if !live[d] {
			delete(loaded, d)
		}
	}
	return targets
}

// followerServesProdLeaf reports whether Caddy's in-process cert cache already
// serves the exact prod leaf for domain that has replicated into state — the
// confirmation that lets the follower stop force-reloading. A nil cache (Caddy
// not yet provisioned) or a missing / non-matching cached leaf reads as "not
// yet", so the caller keeps retrying.
func followerServesProdLeaf(st *state.State, domain string) bool {
	want := prodLeafDER(st, domain)
	if want == nil {
		return false
	}
	have, err := cachepoke.LeafDERs(domain)
	if err != nil {
		return false
	}
	return leafDERMatches(have, want)
}

// leafDERMatches reports whether any DER in have equals want.
func leafDERMatches(have [][]byte, want []byte) bool {
	for _, der := range have {
		if bytes.Equal(der, want) {
			return true
		}
	}
	return false
}

// prodCertResourceComplete reports whether BOTH halves of a non-staging cert
// resource — the leaf (.crt) and its private key (.key) — have replicated for
// domain. certmagic can only LOAD (never needs to OBTAIN) a cert whose full
// resource is in storage, so gating the follower's forced reload on this keeps
// it strictly on the load path and never starts ACME from a follower.
func prodCertResourceComplete(st *state.State, domain string) bool {
	var haveCrt, haveKey bool
	seg := "/" + domain + "/"
	for _, b := range st.CertBlobs.List() {
		key := b.GetKey()
		if strings.Contains(key, "staging") || !strings.Contains(key, seg) {
			continue
		}
		switch {
		case strings.HasSuffix(key, ".crt"):
			haveCrt = true
		case strings.HasSuffix(key, ".key"):
			haveKey = true
		}
	}
	return haveCrt && haveKey
}

// prodLeafDER returns the DER bytes of the leaf certificate from the
// non-staging (prod) .crt blob for domain — the first CERTIFICATE block of the
// stored PEM chain (certmagic stores the chain leaf-first). Returns nil when no
// prod leaf has replicated or the blob can't be decoded.
func prodLeafDER(st *state.State, domain string) []byte {
	seg := "/" + domain + "/"
	for _, b := range st.CertBlobs.List() {
		key := b.GetKey()
		if !strings.HasSuffix(key, ".crt") || strings.Contains(key, "staging") {
			continue
		}
		if !strings.Contains(key, seg) {
			continue
		}
		rest := b.GetValue()
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			if blk.Type == "CERTIFICATE" {
				return blk.Bytes
			}
		}
	}
	return nil
}

// stagingDomainsFromState derives the set of `tls: auto` domains currently in
// their staging window from replicated cert-blob state: a domain that has a
// staging-issued cert blob but no prod cert blob. Every node (leader and
// follower) renders the staging automation policy for these so it can serve the
// replicated staging leaf; once the leader promotes (clears the staging blob,
// lands a prod blob) the domain drops out of this set cluster-wide. See issue
// #182.
func stagingDomainsFromState(st *state.State) map[string]bool {
	out := map[string]bool{}
	for _, domain := range tlsAutoDomains(st) {
		if prodCertIssued(st, domain) {
			continue
		}
		if _, ok := loadStagingChain(st, domain); ok {
			out[domain] = true
		}
	}
	return out
}

// stagingDomainsForBuilder computes the staging-policy domain set the config
// builder consults on each rebuild. It unions:
//   - the replicated-state set (stagingDomainsFromState) — served on every
//     node, and
//   - the stage-first controller's in-flight in-memory set, but ONLY on the
//     leader. The leader needs the in-memory set to bootstrap a brand-new
//     domain into staging before any staging blob has landed in raft;
//     followers never run the controller, so their in-memory set is always
//     empty and must be ignored (issue #182).
func stagingDomainsForBuilder(st *state.State, staging func() map[string]bool, isLeader func() bool) map[string]bool {
	out := stagingDomainsFromState(st)
	if staging != nil && isLeader() {
		for d := range staging() {
			out[d] = true
		}
	}
	return out
}

// reconcileStagingCache drops this node's cached staging leaf for any domain
// just promoted cluster-wide — a domain this node previously observed in its
// staging window (tracked in seen) that has left the staging-derived set and
// now has a prod cert blob in replicated state. Followers never run the
// promotion controller, so without this a follower that served the staging leaf
// during the window would keep serving it from Caddy's cert cache (which
// outlives caddy.Load — the reason cachepoke/#163 exists) after the prod cert
// lands. Eviction is skipped on the leader, which already evicts precisely via
// ClearStagingCert. The seen map is pruned so it stays bounded to live
// in-flight domains. See issue #182.
func (s *Server) reconcileStagingCache(st *state.State, seen map[string]bool, leader bool) {
	staging := stagingDomainsFromState(st)
	for d := range staging {
		seen[d] = true
	}
	live := map[string]bool{}
	for _, d := range tlsAutoDomains(st) {
		live[d] = true
	}
	for d := range seen {
		if staging[d] {
			continue // still in its staging window
		}
		if prodCertIssued(st, d) {
			// Promotion landed: drop the (possibly stale) staging leaf so the
			// next handshake reloads the prod leaf from replicated storage.
			if !leader {
				if err := cachepoke.EvictManaged(d); err != nil && !errors.Is(err, cachepoke.ErrCacheUninitialized) {
					s.logger.Warn("stagefirst follower cache evict failed",
						"subsystem", "stagefirst", "domain", d, "error", err)
				}
			}
			delete(seen, d)
			continue
		}
		if !live[d] {
			// Domain no longer tls:auto and no prod cert — stop tracking.
			delete(seen, d)
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

// clearStagingCertBlobs deletes every staging-issued cert blob for domain
// from the JacoStorage (raft + on-disk fallback cache) and returns the
// count actually removed.
//
// Issue #158: after a staging→prod promotion the rebuild swaps the
// automation policy's Issuer.CA, but certmagic's in-process cert cache +
// the raft-replicated staging blob keep the cached staging leaf serving
// forever — the prod ACME order is never attempted. Wiping the staging
// blobs here removes the on-disk fallback resurrection path and makes a
// subsequent daemon restart land the prod cert without manual
// `rm -rf /var/lib/jaco/ingress/cache`. Eviction of certmagic's in-process
// cache requires a caddy API JACO doesn't yet expose; that's tracked as a
// follow-up in the PR body.
//
// Iteration is by full key (not a prefix delete) because certmagic stores
// multiple resources per cert (`.crt`, `.key`, `.json`) under the same
// CA-and-domain prefix, and JacoStorage exposes only single-key Delete;
// adding a bulk DeletePrefix helper isn't worth the FSM surface for the
// 2–3 keys a single domain ever has.
func clearStagingCertBlobs(ctx context.Context, store *storage.JacoStorage, st *state.State, domain string, logger *slog.Logger) int {
	if store == nil {
		return 0
	}
	// Snapshot the matching keys first — Delete raft-Applies asynchronously
	// and we don't want to iterate the live CertBlobs view while it may
	// shift under us.
	var keys []string
	needle := "/" + domain + "/"
	for _, b := range st.CertBlobs.List() {
		k := b.GetKey()
		if !strings.Contains(k, "staging") {
			continue
		}
		if !strings.Contains(k, needle) {
			continue
		}
		keys = append(keys, k)
	}
	removed := 0
	for _, k := range keys {
		if err := store.Delete(ctx, k); err != nil {
			// Best-effort: a single Delete failure (e.g., follower → leader
			// forward racing a leader change) should not block the
			// promotion. Log + continue so the remaining keys get cleared.
			if logger != nil {
				logger.Warn("clear staging cert blob failed",
					"domain", domain, "key", k, "error", err)
			}
			continue
		}
		removed++
	}
	if logger != nil {
		logger.Info("cleared staging cert blobs on promotion",
			"domain", domain, "removed", removed, "candidates", len(keys))
	}
	return removed
}

// ingressBuilder is the rebuild.Builder concrete impl. Reads state.Routes
// + state.ReplicasObserved + state.Deployments, projects them into the
// config package's typed views, and calls BuildCaddyConfig.
func ingressBuilder(st *state.State, acme ingressACMEOpts, logger *slog.Logger) func() ([]byte, error) {
	return func() ([]byte, error) {
		// Per-stack acme_email lookup (#102): each route inherits its
		// deployment's ACMEEmail, denormalized onto config.Route so the
		// ingress builder doesn't need to thread Deployment lookup further
		// down. Cached per-tick to avoid an N×M state walk for stacks with
		// many routes.
		deploymentEmail := map[string]string{}
		for _, d := range st.Deployments.List() {
			deploymentEmail[d.GetName()] = d.GetAcmeEmail()
		}
		var routes []config.Route
		for _, r := range st.Routes.List() {
			routes = append(routes, config.Route{
				Domain:     r.GetDomain(),
				Deployment: r.GetDeployment(),
				Service:    r.GetService(),
				Port:       int(r.GetPort()),
				TLSAuto:    r.GetTlsAuto(),
				Path:       r.GetPath(),
				StripPath:  r.GetStripPath(),
				ACMEEmail:  deploymentEmail[r.GetDeployment()],
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

		var stagingDomains map[string]bool
		if acme.StagingDomains != nil {
			stagingDomains = acme.StagingDomains()
		}
		cfg, err := config.BuildCaddyConfig(routes, tcpRoutes, replicas, services, config.BuildOpts{
			ACMEEmail:      acme.Email,
			ACMECA:         acme.CA,
			ACMEEnabled:    acme.Enabled,
			ACMEStagingCA:  acme.StagingCA,
			StagingDomains: stagingDomains,
		})
		if err != nil {
			logger.Error("build caddy config failed",
				"routes", len(routes), "tcp_routes", len(tcpRoutes), "observed_replicas", len(replicas), "error", err)
		} else {
			logger.Debug("built caddy config",
				"routes", len(routes), "tcp_routes", len(tcpRoutes), "observed_replicas", len(replicas), "bytes", len(cfg))
		}
		return cfg, err
	}
}

// ingressLoader is the rebuild.Loader concrete impl. Default mode is
// embedded — calls caddy.Load directly, no IPC, no exec (task 32
// deferral). JACO_INGRESS_EXEC=1 falls back to the v0 path that writes
// /etc/caddy/jaco.json + execs `caddy reload`, useful when the operator
// wants caddy crashes to stay isolated from jacod.
func ingressLoader(logger *slog.Logger) func(ctx context.Context, cfg []byte, force bool) error {
	if os.Getenv("JACO_INGRESS_EXEC") == "1" {
		return ingressLoaderExec()
	}
	return ingressLoaderEmbedded(logger)
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
func ingressLoaderEmbedded(logger *slog.Logger) func(ctx context.Context, cfg []byte, force bool) error {
	var started atomic.Bool
	return func(_ context.Context, cfg []byte, force bool) error {
		if !shouldLoad(started.Load(), cfg) {
			logger.Debug("skipping caddy.Load (no reverse_proxy or layer4 route yet)")
			return nil
		}
		if err := caddy.Load(cfg, force); err != nil {
			logger.Error("caddy.Load failed", "force", force, "error", err)
			return fmt.Errorf("caddy.Load: %w", err)
		}
		if started.CompareAndSwap(false, true) {
			logger.Info("caddy loaded and listening", "addrs", ":80/:443")
		}
		return nil
	}
}

// ingressLoaderExec is the v0 fallback: write the config to disk + exec
// `caddy reload`. Skips silently when caddy isn't on PATH. The exec path always
// re-applies the on-disk config, so the force flag (used by the embedded path
// to defeat caddy.Load's identical-config short-circuit) is a no-op here.
func ingressLoaderExec() func(ctx context.Context, cfg []byte, force bool) error {
	caddyBin, _ := exec.LookPath("caddy")
	return func(ctx context.Context, cfg []byte, _ bool) error {
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
