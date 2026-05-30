package firewall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/logging"
)

// ReconcileInterval is the safety-tick cadence of Loop().
const ReconcileInterval = 30 * time.Second

// ListFn returns the live `nft -j list table inet jaco` JSON output. The
// production implementation shells out to `nft` (firewall.NftList); tests
// inject a fake that returns a canned document.
type ListFn func(ctx context.Context) ([]byte, error)

// ApplyFn writes a ruleset to disk and runs `nft -f` against it. Production
// uses firewall.NftApply; tests inject a recording fake.
type ApplyFn func(ctx context.Context, ruleset string) error

// AuditFn raft-Applies an audit event for the reconciler. Production wires
// this to Internal.Submit / raft.Apply; tests inject a recording fake.
type AuditFn func(ctx context.Context, code string, details map[string]string) error

// IsolationStatusFn raft-Applies a NodeStatusUpdate with status=ready (when
// reconcile succeeds after a failure) or status=isolation_unavailable (when
// Apply itself fails).
type IsolationStatusFn func(ctx context.Context, status string, reason string) error

// Reconciler glues SelfTest + Render + Apply into one drift-detection loop.
type Reconciler struct {
	Lister       ListFn
	Applier      ApplyFn
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

	// Logger receives per-tick error lines (apply failures, UpdateStatus
	// failures, Audit failures) plus the per-Tick drift/apply summary.
	// Nil-safe: falls back to a discard logger.
	Logger *slog.Logger

	// ReadyGate, when set, must return true before Loop will run a Tick.
	// While it returns false, Loop quietly waits for the next tick instead
	// of running and logging an error. Used to suppress the boot-window
	// race where a freshly-joined follower runs its first Tick before raft
	// has discovered the leader's address — Audit/UpdateStatus then both
	// fail to forward and the reconciler logs "Audit failed" + "Tick
	// failed" even though the underlying nft apply succeeded (issue #113).
	// Nil-safe: when unset, Tick runs every interval as before.
	ReadyGate func() bool

	// degraded tracks whether the last Tick saw an Apply failure.
	degraded bool
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return logging.Discard()
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
	listBytes, err := r.Lister(ctx)
	if err != nil {
		// Can't read live state — surface the error but don't flip status yet
		// (a transient `nft` exec failure shouldn't mark the node down).
		//
		// NOTE: `nft -j list table inet jaco` also errors when the table is
		// absent, so the isolation table does not auto-bootstrap here. The
		// rendered chains are all policy accept (Render never blanket-drops
		// host traffic — the no-host-disruption invariant), so applying the
		// table on a remotely-managed host is safe and wouldn't lock the
		// operator out. Auto-bootstrapping on a bare table is simply deferred
		// to a separate change tracked outside issue #28.
		return fmt.Errorf("nft list: %w", err)
	}

	selfErr := SelfTestFromJSON(listBytes, expected)
	if selfErr == nil {
		// Live state matches expected. If we were degraded, signal recovery.
		if r.degraded {
			r.degraded = false
			if err := r.UpdateStatus(ctx, "ready", "isolation_reload_recovered"); err != nil {
				r.logger().Error("UpdateStatus(ready) failed", "error", err)
				return err
			}
			if err := r.Audit(ctx, "ISOLATION_RULESET_RECONCILED", map[string]string{
				"action": "recovered",
			}); err != nil {
				r.logger().Error("Audit(ISOLATION_RULESET_RECONCILED action=recovered) failed", "error", err)
				return err
			}
		}
		return nil
	}

	// Mismatch detected — re-render + apply.
	summary := summarizeDrift(selfErr)
	r.logger().Info("firewall drift detected, applying ruleset", "drift", summary)
	ruleset := Render(r.Render())
	if applyErr := r.Applier(ctx, ruleset); applyErr != nil {
		r.degraded = true
		// Log the apply error directly — operators reading jacod logs need
		// to see this even when raft node-status isn't being watched.
		r.logger().Error("apply ruleset failed", "error", applyErr)
		// Log even if UpdateStatus fails — the previous behavior swallowed
		// this with `_ =`, masking the real reason `nft list table inet jaco`
		// stays missing on a live cluster (issue #45).
		if err := r.UpdateStatus(ctx, "isolation_unavailable", applyErr.Error()); err != nil {
			r.logger().Error("UpdateStatus(isolation_unavailable) failed", "error", err, "apply_error", applyErr)
		}
		return fmt.Errorf("apply ruleset: %w", applyErr)
	}

	// Apply succeeded. Audit the reconcile with a compact diff summary.
	r.logger().Info("firewall ruleset reconciled", "drift", summary)
	if err := r.Audit(ctx, "ISOLATION_RULESET_RECONCILED", map[string]string{
		"action":  "applied",
		"summary": summary,
	}); err != nil {
		r.logger().Error("Audit(ISOLATION_RULESET_RECONCILED action=applied) failed", "error", err)
		return err
	}
	if r.degraded {
		r.degraded = false
		if err := r.UpdateStatus(ctx, "ready", "isolation_reload_recovered"); err != nil {
			r.logger().Error("UpdateStatus(ready) failed after recovery apply", "error", err)
		}
	}
	return nil
}

// Loop runs Tick on a 30s ticker until ctx is cancelled.
// Loop runs Tick on a 30s ticker until ctx is cancelled. When ReadyGate is
// set and returns false, the tick is skipped and we wait for the next one —
// this hides the boot-window startup-race (issue #113) where forward-to-
// leader can't yet resolve the leader's address.
func (r *Reconciler) Loop(ctx context.Context) error {
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		if r.ReadyGate == nil || r.ReadyGate() {
			if err := r.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.logger().Error("firewall.Reconciler.Tick failed", "error", err)
			}
		} else {
			r.logger().Debug("firewall.Reconciler.Tick skipped: not ready (no leader yet)")
		}
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
