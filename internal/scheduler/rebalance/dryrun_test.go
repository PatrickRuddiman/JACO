package rebalance_test

import (
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/scheduler/rebalance"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TestDryRun_EmitsAuditButCommitsNoMove — with cfg.Enabled=false,
// the cycle still computes a candidate and writes a DRY_RUN audit
// event, but does NOT raft-Apply a ReplicaDesiredUpsert. Operators
// rely on this to evaluate the policy from the audit log before
// turning it on.
func TestDryRun_EmitsAuditButCommitsNoMove(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = false // dry-run
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedDeployment("dep", 1)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedObserved("dep-web-0", "node-a")
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})

	r.rebal.Cycle(nil)

	// The replica MUST NOT have moved.
	if got := r.replicaHost("dep-web-0"); got != "node-a" {
		t.Errorf("dry-run committed a move; replica now on %q", got)
	}

	// Exactly one DRY_RUN audit must exist, payload tagged dry_run=true.
	dryRuns := r.rebalanceAuditsByType(pbAuditDryRun())
	if len(dryRuns) != 1 {
		t.Fatalf("DRY_RUN audit count = %d, want 1", len(dryRuns))
	}
	ev := dryRuns[0]
	if got := ev.GetPayload()["dry_run"]; got != "true" {
		t.Errorf("DRY_RUN audit dry_run field = %q, want %q", got, "true")
	}
	if got := ev.GetPayload()["src"]; got != "node-a" {
		t.Errorf("DRY_RUN audit src = %q, want %q", got, "node-a")
	}
	if got := ev.GetPayload()["dst"]; got != "node-b" {
		t.Errorf("DRY_RUN audit dst = %q, want %q", got, "node-b")
	}
	if _, err := strconv.ParseFloat(ev.GetPayload()["relief"], 64); err != nil {
		t.Errorf("DRY_RUN audit relief is not a float: %v", err)
	}

	// No ReplicaDesiredUpsert applies for this replica must have
	// landed (the only Applies should be the audit append).
	if applies := countReplicaUpserts(r, "dep-web-0"); applies != 0 {
		t.Errorf("dry-run produced %d ReplicaDesiredUpsert applies, want 0", applies)
	}
}

// TestDryRun_DryRunFlagPropagatedOnSkips — when the cycle skips a
// candidate (e.g. dst_cap), the SKIPPED audit's dry_run flag also
// matches cfg.Enabled. This lets operators tell apart "skipped
// during a real run" vs "skipped during a dry-run shadow".
func TestDryRun_DryRunFlagPropagatedOnSkips(t *testing.T) {
	cfg := rebalance.DefaultConfig()
	cfg.Enabled = false
	cfg.ConsecutiveCycles = 1
	cfg.CooldownReplica = 0
	cfg.CooldownNode = 0
	r := newRig(t, cfg)
	r.seedNode("node-a")
	r.seedNode("node-b")
	r.seedDeployment("dep", 1)
	r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
	r.seedObserved("dep-web-0", "node-a")
	r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
	r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
	// node-b at 0.6 + 0.3 footprint = 0.9 post-move > DstCap 0.75 → skip.
	r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.6})

	r.rebal.Cycle(nil)

	skipped := r.rebalanceAuditsByType(pbAuditSkipped())
	if len(skipped) == 0 {
		t.Fatalf("expected at least one SKIPPED audit; got 0")
	}
	for _, ev := range skipped {
		if got := ev.GetPayload()["dry_run"]; got != "true" {
			t.Errorf("SKIPPED audit dry_run field = %q, want %q (cfg.Enabled=false)", got, "true")
		}
	}
}

// TestDryRun_EnabledFlagToggleSwitchesCommitBehaviour — with the
// same inputs, Enabled=true commits + emits MOVED; Enabled=false
// emits DRY_RUN and commits nothing. Verifies the dry-run path is
// the live path minus the apply step (a regression here means
// operators' dry-run evaluation no longer maps to live behaviour).
func TestDryRun_EnabledFlagToggleSwitchesCommitBehaviour(t *testing.T) {
	build := func(enabled bool) *rig {
		cfg := rebalance.DefaultConfig()
		cfg.Enabled = enabled
		cfg.ConsecutiveCycles = 1
		cfg.CooldownReplica = 0
		cfg.CooldownNode = 0
		cfg.CycleInterval = 30 * time.Second
		r := newRig(t, cfg)
		r.seedNode("node-a")
		r.seedNode("node-b")
		r.seedDeployment("dep", 1)
		r.seedReplica("dep-web-0", "dep", "web", "node-a", 0)
		r.seedObserved("dep-web-0", "node-a")
		r.source.setReplica("dep-web-0", rebalance.Footprint{CPU: 0.3, Memory: 0.1})
		r.source.setNode("node-a", rebalance.Snapshot{CPU: 0.95})
		r.source.setNode("node-b", rebalance.Snapshot{CPU: 0.2})
		return r
	}

	live := build(true)
	live.rebal.Cycle(nil)
	if live.replicaHost("dep-web-0") != "node-b" {
		t.Errorf("Enabled=true: expected replica on node-b, got %q", live.replicaHost("dep-web-0"))
	}
	if len(live.rebalanceAuditsByType(pbAuditMoved())) != 1 {
		t.Errorf("Enabled=true: expected exactly one MOVED audit, got %d",
			len(live.rebalanceAuditsByType(pbAuditMoved())))
	}
	if len(live.rebalanceAuditsByType(pbAuditDryRun())) != 0 {
		t.Errorf("Enabled=true: expected zero DRY_RUN audits, got %d",
			len(live.rebalanceAuditsByType(pbAuditDryRun())))
	}

	dry := build(false)
	dry.rebal.Cycle(nil)
	if dry.replicaHost("dep-web-0") != "node-a" {
		t.Errorf("Enabled=false: replica should not have moved, got %q", dry.replicaHost("dep-web-0"))
	}
	if len(dry.rebalanceAuditsByType(pbAuditMoved())) != 0 {
		t.Errorf("Enabled=false: expected zero MOVED audits, got %d",
			len(dry.rebalanceAuditsByType(pbAuditMoved())))
	}
	if len(dry.rebalanceAuditsByType(pbAuditDryRun())) != 1 {
		t.Errorf("Enabled=false: expected exactly one DRY_RUN audit, got %d",
			len(dry.rebalanceAuditsByType(pbAuditDryRun())))
	}
}

// countReplicaUpserts scans the rig's captured raft Apply payloads
// for ReplicaDesiredUpsert commands targeting the given replica id.
func countReplicaUpserts(r *rig, replicaID string) int {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	n := 0
	for _, data := range r.applies {
		cmd := &pb.Command{}
		if err := proto.Unmarshal(data, cmd); err != nil {
			continue
		}
		up, ok := cmd.GetPayload().(*pb.Command_ReplicaDesiredUpsert)
		if !ok {
			continue
		}
		if up.ReplicaDesiredUpsert.GetReplica().GetId() == replicaID {
			n++
		}
	}
	return n
}
