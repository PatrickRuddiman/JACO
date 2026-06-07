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
	// OnStageFail is called when a staging chain is present but fails the
	// self-check. The daemon emits CERTIFICATE_FAILED{stage_failed_at:staging}.
	OnStageFail func(domain string, err error)
	// Logger receives structured progress lines. nil → a discard logger.
	Logger *slog.Logger
	// Now is the clock (tests pin it). nil → time.Now.
	Now func() time.Time

	mu      sync.Mutex
	staging map[string]bool // domains currently in staging dry-run
	// backoff tracks a per-domain window during which we won't re-stage after
	// a rate-limit (issuing-node-local, not raft-replicated).
	backoff map[string]time.Time
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
				delete(c.pendingProd, domain)
			case now.Before(until):
				// Prod ACME order is still in flight; do NOT re-stage.
				continue
			default:
				// Window expired without a prod cert landing. Treat as a
				// failed promotion: clear the marker and let ShouldStage
				// retry the dry-run from scratch on this same tick.
				c.logger().Warn("prod ACME issuance window expired without a cert landing",
					"domain", domain, "window", PendingProdWindow)
				delete(c.pendingProd, domain)
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
