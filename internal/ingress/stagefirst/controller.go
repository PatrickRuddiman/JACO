package stagefirst

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Controller drives stage-first issuance in embedded mode: it owns the set of
// domains currently in their staging dry-run, decides when a new domain
// should be staged, runs the cheap self-check once the staging cert lands in
// storage, and promotes the domain to prod (which triggers a re-issuance on
// the next config rebuild). Per Q6 this only runs in embedded mode —
// JACO_INGRESS_EXEC=1 leaves issuance to an external caddy and never stages.
//
// Per Q4 the rate-limit backoff window is tracked on the issuing node only
// (NOT replicated via raft) — a follower that becomes leader restarts its own
// backoff clock. Documented as a v1 limitation.
type Controller struct {
	// ConfiguredCA is jacod.yaml.acme_ca (empty → LE prod).
	ConfiguredCA string
	// SkipStaging is jacod.yaml.acme_skip_staging.
	SkipStaging bool

	// Domains lists the current `tls: auto` domains. Re-supplied on each
	// Reconcile from state.Routes.
	// (passed as an argument to Reconcile, not stored.)

	// LoadStagingChain returns the staging-issued leaf chain (PEM) for a
	// domain, ok=false when it isn't in storage yet. The daemon wires this to
	// the cert storage Load keyed by the staging cert path.
	LoadStagingChain func(domain string) (pem []byte, ok bool)
	// IssuedProd reports whether the cluster already holds a prod cert for the
	// domain (so it's not "new" and shouldn't be staged).
	IssuedProd func(domain string) bool
	// OnPromote is called when a domain passes its staging self-check and is
	// promoted to prod. The daemon emits the CERTIFICATE_ISSUED(staging) audit
	// event + triggers a rebuild here.
	OnPromote func(domain string)
	// OnProdIssued is called exactly once per promotion, the moment the
	// controller observes a prod cert landing in raft for a previously
	// promoted domain (pendingProd → cleared because IssuedProd flipped
	// true). The daemon emits CERTIFICATE_ISSUED(prod) here so `jaco status`
	// reports the right environment as soon as the prod cert is real.
	// See issue #147.
	OnProdIssued func(domain string)
	// OnProdFail is called when PendingProdWindow expires without a prod cert
	// landing. The domain enters prodBackoff and the daemon emits
	// CERTIFICATE_FAILED(prod, failure_class=rate_limit). retryAfter is the
	// computed backoff duration (15m first failure, doubling to 1h cap).
	// nil → no-op. Issue #189.
	OnProdFail func(domain string, retryAfter time.Duration)
	// OnStageFail is called when a staging chain is present but fails the
	// self-check. The daemon emits CERTIFICATE_FAILED{stage_failed_at:staging}.
	OnStageFail func(domain string, err error)
	// ClearStagingCert is called on each promotion BEFORE OnPromote fires.
	// The daemon wipes the staging-issued cert blobs for the domain from
	// raft (and the on-disk fallback cache) so the next config reload does
	// not see a staging key under the prod-CA's storage namespace and the
	// stale leaf cannot be served forever. See issue #158. nil → no-op,
	// which preserves pre-#158 behavior for callers that don't wire it.
	ClearStagingCert func(domain string)
	// Logger receives structured progress lines. nil → a discard logger.
	Logger *slog.Logger
	// Now is the clock (tests pin it). nil → time.Now.
	Now func() time.Time

	mu      sync.Mutex
	staging map[string]bool // domains currently in staging dry-run
	// backoff tracks a per-domain window during which we won't re-stage after
	// a rate-limit (issuing-node-local, not raft-replicated).
	backoff map[string]time.Time
	// prodFails counts consecutive prod-issuance failures (window expired
	// without IssuedProd flipping true) per domain. Used to compute the
	// exponential backoff. Issuing-node-local, not raft-replicated — a
	// failover to a new leader restarts the counter (v1 limitation, #189).
	prodFails map[string]int
	// prodBackoff tracks the earliest time the controller will re-stage a
	// domain after a prod-issuance failure. Issuing-node-local (#189).
	prodBackoff map[string]time.Time
	// pendingProd tracks domains that were just promoted from staging→prod
	// and are now waiting for Caddy's prod ACME order to complete. Without
	// this, the next Reconcile tick (10s later) would see prodCertIssued
	// still false (Caddy isn't done yet) and re-add the domain to the
	// staging set, flipping the policy back and forcing Caddy to abandon
	// the in-flight prod issuance — the controller would flip-flop the
	// domain forever and no prod cert would ever land. See issue #154.
	// Cleared when (a) prodCertIssued returns true (prod cert landed,
	// promotion stuck) or (b) the per-domain deadline passes (prod
	// issuance evidently failed; let ShouldStage retry from scratch).
	pendingProd map[string]time.Time
}

// BackoffWindow is how long a domain waits after a staging rate-limit before
// JACO re-attempts the staging dry-run. Matches LE's failed-validation
// rate-limit reset (~1h).
const BackoffWindow = time.Hour

// PendingProdWindow is how long the controller refuses to re-stage a domain
// after promoting it. The window must be long enough for Caddy to complete
// a prod ACME order (HTTP-01 with rate-limit retries can take a couple of
// minutes worst case) but short enough that a genuinely failed prod
// issuance retries the dry-run promptly. See issue #154.
const PendingProdWindow = 5 * time.Minute

// ProdBackoffBase is the initial backoff after a prod-issuance failure.
// Issue #189: without this, the pendingProd-expire→re-stage→re-promote loop
// fires a fresh prod ACME order every ~5 min. Each order is HTTP-429-rejected,
// which EXTENDS LE's failed-auth window so the limit never clears. LE's
// Retry-After header is not observable here (the controller only sees prod
// success/failure via the cert landing in raft), so we use exponential-capped
// backoff instead.
const ProdBackoffBase = 15 * time.Minute

// ProdBackoffMax is the ceiling for the exponential prod-issuance backoff.
// After 3 consecutive failures the domain waits at most 1h between attempts,
// matching LE's failed-auth rate-limit reset window.
const ProdBackoffMax = time.Hour

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// prodBackoffFor returns the backoff duration for n consecutive prod-issuance
// failures: 15m, 30m, 1h, 1h, ... (doubles each time, capped at ProdBackoffMax).
// n ≤ 0 returns ProdBackoffBase. Issue #189.
func prodBackoffFor(n int) time.Duration {
	if n <= 0 {
		return ProdBackoffBase
	}
	d := ProdBackoffBase << uint(n-1)
	if d <= 0 || d > ProdBackoffMax { // overflow guard + cap
		return ProdBackoffMax
	}
	return d
}

// StagingDomains returns a snapshot of the domains currently in their staging
// dry-run. The config builder reads this to point those domains' automation
// policy at the staging directory.
func (c *Controller) StagingDomains() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.staging))
	for d := range c.staging {
		out[d] = true
	}
	return out
}

// Reconcile is the per-tick stage-first pass. For the given `tls: auto`
// domains it:
//   - marks brand-new domains (not yet issued, not in backoff) as staging;
//   - for domains already staging, loads the staging chain and runs the
//     self-check — on success promotes to prod, on failure records the
//     failure and starts a backoff window.
//
// Returns true when the staging set changed (the caller should rebuild +
// reload Caddy so the new policy takes effect).
func (c *Controller) Reconcile(_ context.Context, domains []string) (changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.staging == nil {
		c.staging = map[string]bool{}
	}
	if c.backoff == nil {
		c.backoff = map[string]time.Time{}
	}
	if c.pendingProd == nil {
		c.pendingProd = map[string]time.Time{}
	}
	if c.prodFails == nil {
		c.prodFails = map[string]int{}
	}
	if c.prodBackoff == nil {
		c.prodBackoff = map[string]time.Time{}
	}
	now := c.now()

	live := map[string]bool{}
	for _, d := range domains {
		live[d] = true
	}
	// Drop staging entries for domains that no longer have a tls:auto route.
	for d := range c.staging {
		if !live[d] {
			delete(c.staging, d)
			changed = true
		}
	}

	for _, domain := range domains {
		if c.staging[domain] {
			// A prod cert already exists for a domain we're still staging.
			// Behind an L4 load balancer a peer node rendering the prod CA
			// can win the issuance race (its distributed HTTP-01 challenge
			// now succeeds — issue #189) while this node is mid-staging.
			// Without converging here the node stays pinned to the staging
			// CA forever, waiting for a staging cert that will never land,
			// and never serves the prod leaf. Drop staging, clear any
			// pending/backoff bookkeeping, and fire OnProdIssued once so the
			// daemon emits CERTIFICATE_ISSUED(prod) and the rebuild renders
			// the prod policy. Issue #189.
			if c.issuedProd(domain) {
				delete(c.staging, domain)
				delete(c.pendingProd, domain)
				delete(c.prodFails, domain)
				delete(c.prodBackoff, domain)
				c.logger().Info("prod cert observed while staging; converging to prod",
					"domain", domain)
				if c.OnProdIssued != nil {
					c.OnProdIssued(domain)
				}
				changed = true
				continue
			}
			// Already staging: check whether the staging cert has landed.
			pem, ok := c.loadStagingChain(domain)
			if !ok {
				continue // still issuing against staging
			}
			if err := SelfCheck(domain, pem); err != nil {
				c.logger().Warn("staging self-check failed; not escalating to prod",
					"domain", domain, "error", err)
				delete(c.staging, domain)
				c.backoff[domain] = now.Add(BackoffWindow)
				if c.OnStageFail != nil {
					c.OnStageFail(domain, err)
				}
				changed = true
				continue
			}
			c.logger().Info("staging self-check passed; promoting to prod", "domain", domain)
			delete(c.staging, domain)
			// Mark the domain as awaiting prod issuance. The "not staging
			// yet" branch below will skip it until prodCertIssued returns
			// true or PendingProdWindow elapses. Without this, the next
			// tick would re-stage the domain (because Caddy hasn't finished
			// the prod ACME order yet → prodCertIssued=false →
			// ShouldStage=true) and the flip-flop would never let prod
			// issuance complete. Issue #154.
			c.pendingProd[domain] = now.Add(PendingProdWindow)
			// Wipe the staging cert blobs from storage BEFORE the rebuild
			// fires. Without this, certmagic's in-process cache + the prod
			// automation policy keep serving the cached staging leaf for its
			// full 90-day validity and the prod ACME order is never
			// attempted — the operator sees the "(STAGING) Let's Encrypt"
			// issuer indefinitely. See issue #158. We fire this BEFORE
			// OnPromote so the rebuild OnPromote schedules sees an empty
			// staging-namespaced storage prefix and any peer's raft-driven
			// reseed cannot resurrect it.
			if c.ClearStagingCert != nil {
				c.ClearStagingCert(domain)
			}
			if c.OnPromote != nil {
				c.OnPromote(domain)
			}
			changed = true
			continue
		}

		// Skip domains awaiting prod issuance from a recent promotion
		// (issue #154). Caddy's prod ACME order needs ~30s+ to complete
		// HTTP-01/TLS-ALPN-01 challenges; without this guard the per-tick
		// Reconcile would see prodCertIssued still false and re-stage the
		// domain, abandoning the in-flight prod order.
		if until, pending := c.pendingProd[domain]; pending {
			switch {
			case c.issuedProd(domain):
				// Prod cert landed in raft — promotion stuck. Clear the
				// marker. ShouldStage's "already issued" rule (#3) keeps
				// this domain out of staging permanently from here on.
				// Fire OnProdIssued exactly once for this promotion so the
				// daemon can emit CERTIFICATE_ISSUED(prod) at the precise
				// moment the prod cert becomes real (issue #147 — without
				// this `jaco status` reports `staging` forever because the
				// only ISSUED audit event ever emitted was the staging one).
				delete(c.pendingProd, domain)
				// Reset the failure counter so a future renewal failure
				// restarts the backoff schedule at ProdBackoffBase (#189).
				delete(c.prodFails, domain)
				delete(c.prodBackoff, domain)
				if c.OnProdIssued != nil {
					c.OnProdIssued(domain)
				}
			case now.Before(until):
				// Prod ACME order is still in flight; do NOT re-stage.
				continue
			default:
				// Window expired without a prod cert landing — treat as a
				// failed prod-issuance attempt. Apply exponential backoff
				// (#189) to prevent the re-stage→re-promote loop from firing
				// a fresh prod ACME order every ~5 min, which would EXTEND
				// LE's failed-auth window and prevent recovery.
				c.prodFails[domain]++
				d := prodBackoffFor(c.prodFails[domain])
				c.prodBackoff[domain] = now.Add(d)
				c.logger().Warn("prod ACME issuance window expired without a cert landing",
					"domain", domain, "window", PendingProdWindow,
					"consecutive_failures", c.prodFails[domain], "retry_after", d)
				delete(c.pendingProd, domain)
				if c.OnProdFail != nil {
					c.OnProdFail(domain, d)
				}
				// prodBackoff gate below will suppress re-staging this tick.
				continue
			}
		}

		// Not staging yet — decide whether this new domain should stage.
		dec := ShouldStage(Params{
			Domain:        domain,
			ConfiguredCA:  c.ConfiguredCA,
			SkipStaging:   c.SkipStaging,
			AlreadyIssued: c.issuedProd(domain),
		})
		if !dec.Stage {
			continue
		}
		if until, inProdBackoff := c.prodBackoff[domain]; inProdBackoff {
			if now.Before(until) {
				continue // #189: prod-issuance backoff active; suppress re-stage
			}
			delete(c.prodBackoff, domain)
		}
		if until, inBackoff := c.backoff[domain]; inBackoff {
			if now.Before(until) {
				continue // honor the rate-limit backoff window
			}
			delete(c.backoff, domain)
		}
		c.logger().Debug("staging decision", "domain", domain, "reason", dec.Reason)
		c.staging[domain] = true
		changed = true
	}
	return changed
}

func (c *Controller) loadStagingChain(domain string) ([]byte, bool) {
	if c.LoadStagingChain == nil {
		return nil, false
	}
	return c.LoadStagingChain(domain)
}

func (c *Controller) issuedProd(domain string) bool {
	if c.IssuedProd == nil {
		return false
	}
	return c.IssuedProd(domain)
}
