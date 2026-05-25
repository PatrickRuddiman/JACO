package stagefirst

import (
	"context"
	"log"
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
	// Logger receives structured progress lines. nil → log.Default().
	Logger *log.Logger
	// Now is the clock (tests pin it). nil → time.Now.
	Now func() time.Time

	mu      sync.Mutex
	staging map[string]bool // domains currently in staging dry-run
	// backoff tracks a per-domain window during which we won't re-stage after
	// a rate-limit (issuing-node-local, not raft-replicated).
	backoff map[string]time.Time
}

// BackoffWindow is how long a domain waits after a staging rate-limit before
// JACO re-attempts the staging dry-run. Matches LE's failed-validation
// rate-limit reset (~1h).
const BackoffWindow = time.Hour

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) logger() *log.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return log.Default()
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
				c.logger().Printf("stagefirst: %s staging self-check FAILED: %v (NOT escalating to prod)", domain, err)
				delete(c.staging, domain)
				c.backoff[domain] = now.Add(BackoffWindow)
				if c.OnStageFail != nil {
					c.OnStageFail(domain, err)
				}
				changed = true
				continue
			}
			c.logger().Printf("stagefirst: %s staging self-check passed; promoting to prod", domain)
			delete(c.staging, domain)
			if c.OnPromote != nil {
				c.OnPromote(domain)
			}
			changed = true
			continue
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
		c.logger().Printf("stagefirst: %s — %s", domain, dec.Reason)
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
