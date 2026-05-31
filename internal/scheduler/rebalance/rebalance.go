package rebalance

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Applier wraps raft.Apply, matching the scheduler's signature so the
// daemon can pass the same `func(cmd []byte) error` to both.
type Applier func(cmd []byte) error

// Rebalancer is the leader-only pressure-based rebalance loop. One
// instance per daemon; Run blocks until ctx is cancelled. The cycle
// loop self-gates on LeaderStatus.IsLeader() (same pattern as
// scheduler.Scheduler.Run), so it's safe to spawn unconditionally —
// followers tick the loop but commit nothing.
type Rebalancer struct {
	state  *state.State
	leader scheduler.LeaderStatus
	apply  Applier
	source PressureSource
	cfg    Config

	// Logger is the rebalance subsystem logger. nil → discard. Set by
	// the daemon after construction; tests leave it nil.
	Logger *slog.Logger

	// clock returns the current wall-clock time. Tests override this
	// to drive deterministic EWMA decay and cooldown checks.
	clock func() time.Time

	mu sync.Mutex
	// pressureEWMA maps host → smoothed composite (5-minute window).
	// Built incrementally; leader-local, so a failover loses it and
	// the new leader rebuilds it from the next few cycles.
	pressureEWMA map[string]*EWMA
	// consecutiveOverThreshold counts how many cycles in a row a host
	// has stayed at or above cfg.TriggerThreshold. Reset to 0 when
	// the host drops below.
	consecutiveOverThreshold map[string]int
	// lastReplicaMoveAt tracks per-replica move timestamps for the
	// cfg.CooldownReplica gate. A replica that landed on its current
	// host less than CooldownReplica ago is "still hot" and won't
	// be re-moved.
	lastReplicaMoveAt map[string]time.Time
	// lastNodeMoveAt tracks per-host destination timestamps for
	// cfg.CooldownNode (don't pile fresh work on a node that's still
	// settling).
	lastNodeMoveAt map[string]time.Time
}

// New constructs a Rebalancer. apply MUST be the same raft.Apply
// closure the scheduler uses (moves are committed as
// ReplicaDesiredUpsert + AuditAppend just like any other placement
// change). source is the pressure-data dependency; pass NoopSource
// from the daemon while real cgroup collection is a follow-up.
func New(s *state.State, leader scheduler.LeaderStatus, apply Applier, source PressureSource, cfg Config) *Rebalancer {
	return &Rebalancer{
		state:                    s,
		leader:                   leader,
		apply:                    apply,
		source:                   source,
		cfg:                      cfg,
		clock:                    time.Now,
		pressureEWMA:             map[string]*EWMA{},
		consecutiveOverThreshold: map[string]int{},
		lastReplicaMoveAt:        map[string]time.Time{},
		lastNodeMoveAt:           map[string]time.Time{},
	}
}

// SetClock replaces the time source. Tests use this; production
// leaves the default time.Now.
func (r *Rebalancer) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	r.clock = now
}

func (r *Rebalancer) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
}

// Run drives the rebalance loop. Blocks until ctx is cancelled.
// Cadence is cfg.CycleInterval (default 30s); on every tick Cycle is
// invoked. The loop runs on every node — Cycle is a no-op on
// followers, so spawning unconditionally is safe.
func (r *Rebalancer) Run(ctx context.Context) error {
	interval := r.cfg.CycleInterval
	if interval <= 0 {
		interval = DefaultConfig().CycleInterval
	}
	r.log().Info("rebalance loop started",
		"cycle_interval", interval,
		"trigger_threshold", r.cfg.TriggerThreshold,
		"imbalance_gap", r.cfg.ImbalanceGap)
	t := time.NewTicker(interval)
	defer t.Stop()

	// One immediate cycle on boot so the first sample's EWMA seed
	// lands without waiting for the first tick.
	r.Cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.Cycle(ctx)
		}
	}
}

// Cycle runs one rebalance pass. No-op when the local node isn't the
// raft leader. Exposed publicly so tests can drive single passes
// without spinning up Run.
//
// Pipeline:
//  1. Update per-node EWMAs from PressureSource.NodePressure.
//  2. Pick the most-pressured node; bail if no node is over
//     cfg.TriggerThreshold for cfg.ConsecutiveCycles, or if the
//     max-min gap across nodes is under cfg.ImbalanceGap.
//  3. Enumerate candidates: every ReplicaDesired on the hot node.
//  4. For each (replica, candidate dst) pair, apply HardFilter and
//     hysteresis gates (cooldowns, dst cap, relief floor). Emit a
//     SKIPPED audit event per filtered candidate.
//  5. Score the surviving candidates; pick the highest.
//  6. Commit at most ONE move this cycle: ReplicaDesiredUpsert with
//     the new Host, plus an AUDIT_EVENT_TYPE_REBALANCE_MOVED event.
func (r *Rebalancer) Cycle(_ context.Context) {
	if !r.leader.IsLeader() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock()

	// 1. Sample + EWMA-fold every known node. Skip nodes the source
	//    has no data for.
	nodes := r.state.Nodes.List()
	snapshotByHost := map[string]Snapshot{}
	for _, n := range nodes {
		snap, ok := r.source.NodePressure(n.GetHostname())
		if !ok {
			continue
		}
		snapshotByHost[n.GetHostname()] = snap
		e, ok := r.pressureEWMA[n.GetHostname()]
		if !ok {
			e = NewEWMA(5 * time.Minute)
			r.pressureEWMA[n.GetHostname()] = e
		}
		e.Update(now, Composite(snap))
	}
	if len(snapshotByHost) < 2 {
		// Need at least 2 nodes worth of data to have an imbalance.
		return
	}

	// 2. Trigger + imbalance gates.
	hotHost := ""
	hotEWMA := 0.0
	coldEWMA := 1.1 // sentinel above any plausible pressure
	for host := range snapshotByHost {
		v := r.pressureEWMA[host].Value()
		if v > hotEWMA {
			hotHost, hotEWMA = host, v
		}
		if v < coldEWMA {
			coldEWMA = v
		}
	}
	// Update consecutive-over counters for every node.
	for host := range snapshotByHost {
		if r.pressureEWMA[host].Value() >= r.cfg.TriggerThreshold {
			r.consecutiveOverThreshold[host]++
		} else {
			r.consecutiveOverThreshold[host] = 0
		}
	}
	// Per-cycle telemetry — Info because the cluster fires this
	// once per CycleInterval (default 30s), low enough volume to
	// keep, high enough signal to debug why a move didn't happen.
	r.log().Info("rebalance cycle",
		"hot", hotHost,
		"hot_ewma", hotEWMA,
		"cold_ewma", coldEWMA,
		"gap", hotEWMA-coldEWMA,
		"consec_over", r.consecutiveOverThreshold[hotHost],
		"trigger", r.cfg.TriggerThreshold,
		"imbalance_gap_req", r.cfg.ImbalanceGap,
		"hosts_seen", len(snapshotByHost),
	)
	if r.consecutiveOverThreshold[hotHost] < r.cfg.ConsecutiveCycles {
		return // hot node hasn't been hot long enough
	}
	if hotEWMA-coldEWMA < r.cfg.ImbalanceGap {
		return // cluster busy but uniform; no relief target
	}

	// 3. Build per-host replica counts (used for SPREAD anti-affinity).
	perHostCount := r.buildPerHostCount()

	// 4. Enumerate candidates: every ReplicaDesired on hotHost.
	type scored struct {
		c     *MoveCandidate
		score float64
	}
	var survived []scored
	for _, rep := range r.state.ReplicasDesired.List() {
		if rep.GetHost() != hotHost {
			continue
		}
		spec := r.serviceSpec(rep)
		fp := r.source.ReplicaFootprint(rep.GetId())

		// Per-replica cooldown — fail fast before iterating dsts.
		if lastMove, ok := r.lastReplicaMoveAt[rep.GetId()]; ok {
			if now.Sub(lastMove) < r.cfg.CooldownReplica {
				r.skip(&MoveCandidate{
					Replica:     rep,
					Src:         hotHost,
					Dst:         "",
					Spec:        spec,
					Footprint:   fp,
					SrcPressure: snapshotByHost[hotHost],
					DstPressure: Snapshot{},
				}, SkipCooldownReplica, 0, 0, Dominant(snapshotByHost[hotHost]))
				continue
			}
		}

		// Try every other known host as a dst.
		for dst := range snapshotByHost {
			if dst == hotHost {
				continue
			}
			c := &MoveCandidate{
				Replica:         rep,
				Src:             hotHost,
				Dst:             dst,
				Spec:            spec,
				Footprint:       fp,
				SrcPressure:     snapshotByHost[hotHost],
				DstPressure:     snapshotByHost[dst],
				PerHostCount:    perHostCount[hostServiceKey{dst, rep.GetDeployment(), rep.GetService()}],
				DstResourceFits: dstResourceFits(snapshotByHost[dst], fp),
			}
			dom := Dominant(c.SrcPressure)

			// Node cooldown — fast fail before HardFilter.
			if lastNode, ok := r.lastNodeMoveAt[dst]; ok {
				if now.Sub(lastNode) < r.cfg.CooldownNode {
					r.skip(c, SkipCooldownNode, 0, 0, dom)
					continue
				}
			}

			if reason := HardFilter(c); reason != SkipNone {
				r.skip(c, reason, 0, 0, dom)
				continue
			}

			// dst_cap + relief_floor gates use post-move pressure.
			postSrc, postDst := PostMovePressure(c)
			if Composite(postDst) >= r.cfg.DstCap {
				r.skip(c, SkipDstCap, 0, 0, dom)
				continue
			}
			relief := Composite(c.SrcPressure) - Composite(postSrc)
			if relief < r.cfg.ReliefFloor {
				r.skip(c, SkipReliefFloor, 0, relief, dom)
				continue
			}

			survived = append(survived, scored{c, Score(c)})
		}
	}

	if len(survived) == 0 {
		return
	}

	// 5. Pick the highest-scoring survivor. Ties broken by replica
	//    id lexicographically so the choice is deterministic in
	//    tests (Go map iteration order is randomised).
	sort.Slice(survived, func(i, j int) bool {
		if survived[i].score != survived[j].score {
			return survived[i].score > survived[j].score
		}
		return survived[i].c.Replica.GetId() < survived[j].c.Replica.GetId()
	})
	winner := survived[0]
	dom := Dominant(winner.c.SrcPressure)
	relief := Composite(winner.c.SrcPressure) - Composite(mustPostSrc(winner.c))

	// 6. Commit the chosen move.
	if err := r.commitMove(winner.c); err != nil {
		r.log().Warn("rebalance commit failed",
			"replica_id", winner.c.Replica.GetId(),
			"src", winner.c.Src, "dst", winner.c.Dst, "error", err)
		return
	}
	r.lastReplicaMoveAt[winner.c.Replica.GetId()] = now
	r.lastNodeMoveAt[winner.c.Dst] = now
	if auditErr := r.emitAudit(
		pb.AuditEventType_AUDIT_EVENT_TYPE_REBALANCE_MOVED,
		auditPayload(winner.c, winner.score, relief, dom, SkipNone),
	); auditErr != nil {
		r.log().Warn("rebalance audit emit failed",
			"event", "rebalance_moved", "replica_id", winner.c.Replica.GetId(), "error", auditErr)
	}
	r.log().Info("rebalance move committed",
		"replica_id", winner.c.Replica.GetId(),
		"src", winner.c.Src, "dst", winner.c.Dst,
		"score", winner.score, "relief", relief, "dominant", dom.String())
}

// commitMove raft-Applies a ReplicaDesiredUpsert with the candidate's
// destination host. The scheduler's reconcile loop sees the resulting
// host change on its next pass and emits stop-on-src / start-on-dst
// via the existing rebuild path — the rebalancer doesn't directly
// touch the runtime.
func (r *Rebalancer) commitMove(c *MoveCandidate) error {
	rep := proto.Clone(c.Replica).(*pb.ReplicaDesired)
	rep.Host = c.Dst
	cmd := &pb.Command{
		Identity: "scheduler/rebalance",
		Ts:       timestamppb.New(r.clock()),
		Payload: &pb.Command_ReplicaDesiredUpsert{ReplicaDesiredUpsert: &pb.ReplicaDesiredUpsert{
			Replica: rep,
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return r.apply(data)
}

// skip emits a SKIPPED audit event for one filtered candidate. Best-
// effort; an audit failure does not abort the cycle.
func (r *Rebalancer) skip(c *MoveCandidate, reason SkipReason, score, relief float64, dom Dimension) {
	if auditErr := r.emitAudit(
		pb.AuditEventType_AUDIT_EVENT_TYPE_REBALANCE_SKIPPED,
		auditPayload(c, score, relief, dom, reason),
	); auditErr != nil {
		r.log().Warn("rebalance audit emit failed",
			"event", "rebalance_skipped", "replica_id", c.Replica.GetId(),
			"reason", string(reason), "error", auditErr)
	}
}

// hostServiceKey indexes "how many replicas of (dep, svc) live on
// host" for the SPREAD anti-affinity check.
type hostServiceKey struct {
	host       string
	deployment string
	service    string
}

// buildPerHostCount counts current replicas of each service on each
// host from ReplicasDesired. Used by the scorer's anti-affinity gate
// to refuse co-locating two replicas of a SPREAD service.
func (r *Rebalancer) buildPerHostCount() map[hostServiceKey]int {
	counts := map[hostServiceKey]int{}
	for _, rep := range r.state.ReplicasDesired.List() {
		counts[hostServiceKey{rep.GetHost(), rep.GetDeployment(), rep.GetService()}]++
	}
	return counts
}

// serviceSpec resolves the ServiceSpec for a ReplicaDesired, or nil if
// the deployment / service has been removed since the replica was
// placed. nil specs are handled by HardFilter (anti-affinity treats
// nil as "no constraint").
func (r *Rebalancer) serviceSpec(rep *pb.ReplicaDesired) *pb.ServiceSpec {
	dep, ok := r.state.Deployments.Get(rep.GetDeployment())
	if !ok {
		return nil
	}
	for _, svc := range dep.GetServices() {
		if svc.GetName() == rep.GetService() {
			return svc
		}
	}
	return nil
}

// dstResourceFits is the "Dst can host the replica without violating
// resource limits" check. CPU + Memory must stay ≤ 1.0 after the
// move. Real per-replica limit modelling (memory_limit, cpus) lives
// in the runtime; this conservative utilisation check is enough to
// keep the rebalancer from piling onto a node that's already 95%
// memory just because its CPU is idle.
func dstResourceFits(dst Snapshot, fp Footprint) bool {
	if dst.CPU+fp.CPU > 1.0 {
		return false
	}
	if dst.Memory+fp.Memory > 1.0 {
		return false
	}
	return true
}

// mustPostSrc returns post-move Src pressure — a sliver of
// PostMovePressure used inline by the commit path so we can compute
// relief without re-allocating both halves of the tuple.
func mustPostSrc(c *MoveCandidate) Snapshot {
	p, _ := PostMovePressure(c)
	return p
}
