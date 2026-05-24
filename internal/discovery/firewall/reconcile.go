package firewall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReconcileInterval is the safety-tick cadence of Loop().
const ReconcileInterval = 30 * time.Second

// Lister returns the live `nft -j list table inet jaco` JSON output. The
// production implementation shells out to `nft`; tests inject a fake that
// returns a canned document.
type Lister interface {
	List(ctx context.Context) ([]byte, error)
}

// AuditFn raft-Applies an audit event for the reconciler. Production wires
// this to Internal.Submit / raft.Apply; tests inject a recording fake.
type AuditFn func(ctx context.Context, code string, details map[string]string) error

// IsolationStatusFn raft-Applies a NodeStatusUpdate with status=ready (when
// reconcile succeeds after a failure) or status=isolation_unavailable (when
// Apply itself fails).
type IsolationStatusFn func(ctx context.Context, status string, reason string) error

// Reconciler glues SelfTest + Render + Apply into one drift-detection loop.
type Reconciler struct {
	Lister       Lister
	Applier      Applier
	Audit        AuditFn
	UpdateStatus IsolationStatusFn
	Render       func() RuleInput

	// Pool is the IPAM /16; EnsureSNAT re-asserts the intra-pool SNAT
	// exemption in Docker's nat POSTROUTING each tick (issue #28). Both are
	// optional — when either is unset the SNAT step is skipped (tests, or
	// hosts without iptables).
	Pool       string
	EnsureSNAT func(ctx context.Context, pool string) error

	// EnsureOverlay re-asserts the intra-pool ACCEPT exemptions that let
	// cross-host container traffic past Docker's container-isolation drops
	// (raw PREROUTING direct-routing + FORWARD inter-network isolation, issue
	// #28). Optional and Pool-gated, same as EnsureSNAT.
	EnsureOverlay func(ctx context.Context, pool string) error

	// degraded tracks whether the last Tick saw an Apply failure.
	degraded bool
}

// Tick runs one reconcile pass. Returns nil when the live ruleset matches
// expected (no work to do) or an Apply succeeded; returns an error when
// Apply failed (the daemon should already have been marked
// isolation_unavailable via UpdateStatus).
func (r *Reconciler) Tick(ctx context.Context) error {
	// Re-assert the intra-pool SNAT exemption first — it lives in Docker's
	// nat POSTROUTING (outside table inet jaco / its SelfTest), so it must be
	// checked every tick. Best-effort: a failure here is independent of the
	// inet jaco isolation status.
	if r.EnsureSNAT != nil && r.Pool != "" {
		if err := r.EnsureSNAT(ctx, r.Pool); err != nil {
			_ = r.Audit(ctx, "SNAT_EXEMPT_FAILED", map[string]string{"error": err.Error()})
		}
	}

	// Re-assert the overlay-isolation exemptions too — same rationale, also in
	// Docker-owned chains (raw PREROUTING + DOCKER-USER) outside table inet
	// jaco. Best-effort and independent of the inet jaco isolation status.
	if r.EnsureOverlay != nil && r.Pool != "" {
		if err := r.EnsureOverlay(ctx, r.Pool); err != nil {
			_ = r.Audit(ctx, "OVERLAY_EXEMPT_FAILED", map[string]string{"error": err.Error()})
		}
	}

	expected := r.Render()
	listBytes, err := r.Lister.List(ctx)
	if err != nil {
		// Can't read live state — surface the error but don't flip status yet
		// (a transient `nft` exec failure shouldn't mark the node down).
		//
		// NOTE: `nft -j list table inet jaco` also errors when the table is
		// absent, so the isolation table does not auto-bootstrap here. That's
		// intentional for now: the rendered `input` chain (policy drop) does
		// not yet permit SSH or the Tailscale interface, so applying it on a
		// remotely-managed host would lock the operator out. Enabling the
		// table safely (SSH/Tailscale allow + correct WG iface name) is a
		// separate change tracked outside issue #28.
		return fmt.Errorf("nft list: %w", err)
	}

	selfErr := SelfTestFromJSON(listBytes, expected)
	if selfErr == nil {
		// Live state matches expected. If we were degraded, signal recovery.
		if r.degraded {
			r.degraded = false
			if err := r.UpdateStatus(ctx, "ready", "isolation_reload_recovered"); err != nil {
				return err
			}
			if err := r.Audit(ctx, "ISOLATION_RULESET_RECONCILED", map[string]string{
				"action": "recovered",
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// Mismatch detected — re-render + apply.
	ruleset := Render(r.Render())
	if applyErr := r.Applier.Apply(ctx, ruleset); applyErr != nil {
		r.degraded = true
		_ = r.UpdateStatus(ctx, "isolation_unavailable", applyErr.Error())
		return fmt.Errorf("apply ruleset: %w", applyErr)
	}

	// Apply succeeded. Audit the reconcile with a compact diff summary.
	summary := summarizeDrift(selfErr)
	if err := r.Audit(ctx, "ISOLATION_RULESET_RECONCILED", map[string]string{
		"action":  "applied",
		"summary": summary,
	}); err != nil {
		return err
	}
	if r.degraded {
		r.degraded = false
		_ = r.UpdateStatus(ctx, "ready", "isolation_reload_recovered")
	}
	return nil
}

// Loop runs Tick on a 30s ticker until ctx is cancelled.
func (r *Reconciler) Loop(ctx context.Context) error {
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		// Run once at start so drift gets fixed quickly.
		_ = r.Tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// summarizeDrift collapses a SelfTestError into a short comma-list suitable
// for audit details.
func summarizeDrift(err error) string {
	if err == nil {
		return ""
	}
	if ste, ok := err.(*SelfTestError); ok {
		parts := make([]string, 0, len(ste.Missing)+len(ste.Extra))
		for _, m := range ste.Missing {
			parts = append(parts, "missing:"+m)
		}
		for _, e := range ste.Extra {
			parts = append(parts, "extra:"+e)
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	return err.Error()
}
