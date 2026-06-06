// reconciler.go: leader-only loop that nudges raft's voter count toward
// Target(len(members)) one suffrage change per tick. The body is wrapped
// in NewReconciler/Run so callers (the daemon) can spawn it with the same
// shape the scheduler/rebalancer use.
package membership

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/logging"
)

// Defaults govern the cadence and the catch-up gate. Exposed via Config
// so tests can drive single passes with short intervals.
const (
	DefaultTickInterval = 3 * time.Second
	// DefaultPromoteAfter is the minimum time a nonvoter must have been
	// in the raft configuration before the reconciler will promote it.
	// This is the catch-up gate that defends against the bug-003 race:
	// AddNonvoter returns the moment the config-change log entry
	// commits, but the joiner's transport may still be racing to
	// replicate the existing log. PromoteAfter buys raft enough time
	// to NAK / heal before we make the new server count toward quorum.
	DefaultPromoteAfter = 3 * time.Second
	// DefaultRaftOpTimeout bounds each AddVoter / DemoteVoter future.
	// Picked to be shorter than the tick interval so a wedged config
	// change doesn't pile up.
	DefaultRaftOpTimeout = 2 * time.Second
)

// Config controls the reconciler. Zero values fall back to the Default*
// constants above.
type Config struct {
	TickInterval  time.Duration
	PromoteAfter  time.Duration
	RaftOpTimeout time.Duration
}

// Raft is the slice of hashicorp/raft the reconciler touches. Defined as
// an interface so tests don't need a real raft node.
type Raft interface {
	IsLeader() bool
	GetConfiguration() hraft.ConfigurationFuture
	AddVoter(id hraft.ServerID, addr hraft.ServerAddress, prevIndex uint64, timeout time.Duration) hraft.IndexFuture
	DemoteVoter(id hraft.ServerID, prevIndex uint64, timeout time.Duration) hraft.IndexFuture
}

// Reconciler is the leader-only voter-set reconciler. Spawn one per
// daemon and call Run; the loop self-gates on raft leadership so it's
// safe to start unconditionally. Kick() forces an immediate pass and
// is called from NodeJoin / NodeRemove handlers after their applies
// commit so promotions don't wait a full tick.
type Reconciler struct {
	raft     Raft
	selfID   string
	cfg      Config
	clock    func() time.Time
	kick     chan struct{}

	mu sync.Mutex
	// firstSeen tracks when each server was first observed in the raft
	// configuration. Used by the PromoteAfter catch-up gate. Servers
	// that leave the configuration are evicted from this map on the
	// next tick (their entry is stale; the reset is harmless since
	// only nonvoters consult the gate).
	firstSeen map[string]time.Time

	// Logger is the reconciler's subsystem logger. nil → discard. Set
	// by the daemon after construction; tests leave it nil.
	Logger *slog.Logger
}

// New constructs a Reconciler. selfID is the leader's own raft ServerID
// (hostname); it's needed so PickDemote never picks the leader.
func New(r Raft, selfID string, cfg Config) *Reconciler {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = DefaultTickInterval
	}
	if cfg.PromoteAfter <= 0 {
		cfg.PromoteAfter = DefaultPromoteAfter
	}
	if cfg.RaftOpTimeout <= 0 {
		cfg.RaftOpTimeout = DefaultRaftOpTimeout
	}
	return &Reconciler{
		raft:      r,
		selfID:    selfID,
		cfg:       cfg,
		clock:     time.Now,
		kick:      make(chan struct{}, 1),
		firstSeen: map[string]time.Time{},
	}
}

// SetClock replaces the time source. Tests use this; production keeps
// the default time.Now.
func (r *Reconciler) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	r.clock = now
}

// Kick requests an immediate reconcile pass. Non-blocking: a kick that
// arrives while one is already queued is coalesced. Called by NodeJoin
// and NodeRemove after their raft applies commit, so the joiner /
// leaver's effect on the voter set is visible without waiting for the
// next tick.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *Reconciler) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
}

// Run drives the reconcile loop. Blocks until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	r.log().Info("membership reconciler started",
		"tick_interval", r.cfg.TickInterval,
		"promote_after", r.cfg.PromoteAfter)
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.Tick(ctx)
		case <-r.kick:
			r.Tick(ctx)
		}
	}
}

// Tick runs one reconcile pass. No-op when not leader. Exposed so tests
// can drive single passes without spinning up Run.
func (r *Reconciler) Tick(ctx context.Context) {
	if !r.raft.IsLeader() {
		// Followers don't reconcile, but they do clear local state so
		// a re-election doesn't start with a stale firstSeen map (the
		// new leader will rebuild it from its own observations).
		r.mu.Lock()
		if len(r.firstSeen) > 0 {
			r.firstSeen = map[string]time.Time{}
		}
		r.mu.Unlock()
		return
	}

	cfgFuture := r.raft.GetConfiguration()
	if err := cfgFuture.Error(); err != nil {
		r.log().Warn("GetConfiguration failed", "error", err)
		return
	}
	servers := cfgFuture.Configuration().Servers
	now := r.clock()

	// Update firstSeen: stamp newly-observed servers, evict departed
	// ones. The PromoteAfter gate reads from this map.
	r.mu.Lock()
	present := make(map[string]struct{}, len(servers))
	for _, s := range servers {
		id := string(s.ID)
		present[id] = struct{}{}
		if _, ok := r.firstSeen[id]; !ok {
			r.firstSeen[id] = now
		}
	}
	for id := range r.firstSeen {
		if _, ok := present[id]; !ok {
			delete(r.firstSeen, id)
		}
	}
	// Snapshot firstSeen for the eligible closure below so we don't
	// hold r.mu across the raft call.
	seen := make(map[string]time.Time, len(r.firstSeen))
	for k, v := range r.firstSeen {
		seen[k] = v
	}
	r.mu.Unlock()

	// Partition voters vs nonvoters; record the joining-server addr
	// since AddVoter needs it.
	var voters, nonvoters []string
	addrByID := make(map[string]hraft.ServerAddress, len(servers))
	for _, s := range servers {
		addrByID[string(s.ID)] = s.Address
		switch s.Suffrage {
		case hraft.Voter:
			voters = append(voters, string(s.ID))
		case hraft.Nonvoter, hraft.Staging:
			nonvoters = append(nonvoters, string(s.ID))
		}
	}
	sort.Strings(voters)
	sort.Strings(nonvoters)

	target := Target(len(servers))
	actual := len(voters)
	r.log().Debug("membership tick",
		"members", len(servers),
		"voters", actual,
		"nonvoters", len(nonvoters),
		"target", target)

	switch {
	case actual < target:
		gate := r.cfg.PromoteAfter
		eligible := func(id string) bool {
			seenAt, ok := seen[id]
			if !ok {
				return false
			}
			return now.Sub(seenAt) >= gate
		}
		pick := PickPromote(nonvoters, eligible)
		if pick == "" {
			return
		}
		addr, ok := addrByID[pick]
		if !ok || addr == "" {
			r.log().Warn("promote candidate has no address; skipping",
				"id", pick)
			return
		}
		r.log().Info("promoting nonvoter to voter",
			"id", pick, "addr", addr,
			"voters_before", actual, "voters_target", target,
			"members", len(servers))
		f := r.raft.AddVoter(hraft.ServerID(pick), addr, 0, r.cfg.RaftOpTimeout)
		if err := f.Error(); err != nil {
			r.log().Error("AddVoter failed; will retry next tick",
				"id", pick, "error", err)
			return
		}
	case actual > target:
		pick := PickDemote(voters, r.selfID)
		if pick == "" {
			return
		}
		r.log().Info("demoting voter to nonvoter",
			"id", pick,
			"voters_before", actual, "voters_target", target,
			"members", len(servers))
		f := r.raft.DemoteVoter(hraft.ServerID(pick), 0, r.cfg.RaftOpTimeout)
		if err := f.Error(); err != nil {
			r.log().Error("DemoteVoter failed; will retry next tick",
				"id", pick, "error", err)
			return
		}
	}
}

// Snapshot is a leader-only view of the current voter / nonvoter split.
// Returned as a flat hostname→suffrage map; callers (ClusterStatus) only
// need to render it. Empty map on followers (raft.GetConfiguration() can
// still be called there, but the values would be stale across an
// election, so we refuse to mislead operators).
func Snapshot(r Raft) map[string]hraft.ServerSuffrage {
	if !r.IsLeader() {
		return nil
	}
	f := r.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil
	}
	out := make(map[string]hraft.ServerSuffrage, len(f.Configuration().Servers))
	for _, s := range f.Configuration().Servers {
		out[string(s.ID)] = s.Suffrage
	}
	return out
}
