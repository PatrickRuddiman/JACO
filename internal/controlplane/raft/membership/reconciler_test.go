package membership

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
)

// fakeRaft is the smallest possible stand-in for the Raft interface the
// reconciler depends on. It records every AddVoter/DemoteVoter call so a
// test can assert "exactly one promotion happened this tick".
type fakeRaft struct {
	mu          sync.Mutex
	leader      bool
	servers     []hraft.Server
	cfgErr      error
	addVoterErr error
	demoteErr   error

	addVoterCalls    []hraft.ServerID
	demoteVoterCalls []hraft.ServerID
}

func newFakeRaft(leader bool, servers []hraft.Server) *fakeRaft {
	return &fakeRaft{leader: leader, servers: servers}
}

func (f *fakeRaft) IsLeader() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leader
}

func (f *fakeRaft) GetConfiguration() hraft.ConfigurationFuture {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := hraft.Configuration{Servers: append([]hraft.Server(nil), f.servers...)}
	return &fakeCfgFuture{cfg: c, err: f.cfgErr}
}

func (f *fakeRaft) AddVoter(id hraft.ServerID, addr hraft.ServerAddress, _ uint64, _ time.Duration) hraft.IndexFuture {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addVoterCalls = append(f.addVoterCalls, id)
	if f.addVoterErr != nil {
		return &fakeIdxFuture{err: f.addVoterErr}
	}
	// Flip the suffrage in-place so a follow-up Tick sees the new config.
	for i, s := range f.servers {
		if s.ID == id {
			f.servers[i].Suffrage = hraft.Voter
			f.servers[i].Address = addr
		}
	}
	return &fakeIdxFuture{}
}

func (f *fakeRaft) DemoteVoter(id hraft.ServerID, _ uint64, _ time.Duration) hraft.IndexFuture {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.demoteVoterCalls = append(f.demoteVoterCalls, id)
	if f.demoteErr != nil {
		return &fakeIdxFuture{err: f.demoteErr}
	}
	for i, s := range f.servers {
		if s.ID == id {
			f.servers[i].Suffrage = hraft.Nonvoter
		}
	}
	return &fakeIdxFuture{}
}

type fakeCfgFuture struct {
	cfg hraft.Configuration
	err error
}

func (f *fakeCfgFuture) Configuration() hraft.Configuration { return f.cfg }
func (f *fakeCfgFuture) Error() error                       { return f.err }
func (f *fakeCfgFuture) Index() uint64                      { return 0 }

type fakeIdxFuture struct{ err error }

func (f *fakeIdxFuture) Error() error  { return f.err }
func (f *fakeIdxFuture) Index() uint64 { return 0 }

func srv(id, addr string, suf hraft.ServerSuffrage) hraft.Server {
	return hraft.Server{ID: hraft.ServerID(id), Address: hraft.ServerAddress(addr), Suffrage: suf}
}

// steppingClock returns a monotonic time the test advances manually so
// PromoteAfter gate transitions are deterministic without real sleeps.
type steppingClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSteppingClock() *steppingClock {
	return &steppingClock{t: time.Unix(0, 0)}
}

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *steppingClock) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestTick_FollowerIsNoop(t *testing.T) {
	r := newFakeRaft(false, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Nonvoter),
	})
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: 0, RaftOpTimeout: time.Second})
	rec.Tick(context.Background())
	if len(r.addVoterCalls) != 0 || len(r.demoteVoterCalls) != 0 {
		t.Errorf("follower must be no-op, got add=%v demote=%v", r.addVoterCalls, r.demoteVoterCalls)
	}
}

func TestTick_PromoteAfterGate(t *testing.T) {
	// 3 members (1 voter + 2 nonvoters), target=3, so two promotions
	// are needed; but PromoteAfter is 5s and nothing's been seen yet.
	// First tick should NOT promote — only stamp firstSeen.
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Nonvoter),
		srv("jaco-3", "10.0.0.3:7001", hraft.Nonvoter),
	})
	now := time.Now()
	clock := func() time.Time { return now }
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: 5 * time.Second, RaftOpTimeout: time.Second})
	rec.SetClock(clock)
	rec.Tick(context.Background())
	if got := len(r.addVoterCalls); got != 0 {
		t.Fatalf("first tick: AddVoter calls = %d, want 0 (gate blocks)", got)
	}

	// Advance past the gate; next tick must promote exactly one
	// (one change per tick), picking jaco-2 by hostname-asc order.
	now = now.Add(6 * time.Second)
	rec.Tick(context.Background())
	if got := len(r.addVoterCalls); got != 1 || r.addVoterCalls[0] != "jaco-2" {
		t.Errorf("post-gate first promote: calls=%v, want [jaco-2]", r.addVoterCalls)
	}

	// Third tick promotes jaco-3 to reach target.
	rec.Tick(context.Background())
	if got := r.addVoterCalls; len(got) != 2 || got[1] != "jaco-3" {
		t.Errorf("post-gate second promote: calls=%v, want [jaco-2 jaco-3]", got)
	}

	// Fourth tick: now at target, no further changes.
	rec.Tick(context.Background())
	if got := len(r.addVoterCalls); got != 2 {
		t.Errorf("at target, expected no further promotions; calls=%v", r.addVoterCalls)
	}
	if got := len(r.demoteVoterCalls); got != 0 {
		t.Errorf("at target, expected no demotions; calls=%v", r.demoteVoterCalls)
	}
}

func TestTick_DemoteWhenOverTarget(t *testing.T) {
	// 4 members (3 voters + 1 nonvoter), target=3 — but actual=3 already.
	// To exercise demote, build 4 voters / 0 nonvoters, target = Target(4) = 3:
	// must demote one. Picker is hostname-desc excluding self, so jaco-4.
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Voter),
		srv("jaco-3", "10.0.0.3:7001", hraft.Voter),
		srv("jaco-4", "10.0.0.4:7001", hraft.Voter),
	})
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: 0, RaftOpTimeout: time.Second})
	rec.Tick(context.Background())
	if got := r.demoteVoterCalls; len(got) != 1 || got[0] != "jaco-4" {
		t.Errorf("first demote: calls=%v, want [jaco-4]", got)
	}
	rec.Tick(context.Background())
	if got := len(r.demoteVoterCalls); got != 1 {
		t.Errorf("at target, expected no further demotions; calls=%v", r.demoteVoterCalls)
	}
}

func TestTick_NeverDemotesSelf(t *testing.T) {
	// Pathological: leader is the only voter and target is somehow
	// lower. Picker must refuse — losing leadership mid-tick wedges
	// the cluster.
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter), // self + leader
	})
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: 0, RaftOpTimeout: time.Second})
	// 1 member -> target=1, so this is steady-state; force the imbalance
	// by injecting an extra phantom voter that the picker must skip.
	// We do that by directly setting servers from another voter ID.
	r.servers = append(r.servers, srv("jaco-2", "10.0.0.2:7001", hraft.Voter))
	// Now 2 members -> target=1. Picker must demote jaco-2 (not jaco-1).
	rec.Tick(context.Background())
	if got := r.demoteVoterCalls; len(got) != 1 || got[0] != "jaco-2" {
		t.Errorf("demote pick: %v, want [jaco-2]", got)
	}
}

func TestTick_OneChangePerTick(t *testing.T) {
	// 5 members (1 voter + 4 nonvoters), Target(5)=5 -> 4 promotions
	// queued, but a single Tick must do at most one.
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Nonvoter),
		srv("jaco-3", "10.0.0.3:7001", hraft.Nonvoter),
		srv("jaco-4", "10.0.0.4:7001", hraft.Nonvoter),
		srv("jaco-5", "10.0.0.5:7001", hraft.Nonvoter),
	})
	clk := newSteppingClock()
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: time.Nanosecond, RaftOpTimeout: time.Second})
	rec.SetClock(clk.now)
	rec.Tick(context.Background()) // first tick stamps firstSeen
	clk.step(10 * time.Millisecond)
	rec.Tick(context.Background())
	if got := len(r.addVoterCalls); got != 1 {
		t.Errorf("expected exactly one AddVoter per tick, got %d (%v)", got, r.addVoterCalls)
	}
}

func TestTick_RaftError_DoesNotPanicAndRetries(t *testing.T) {
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Nonvoter),
		srv("jaco-3", "10.0.0.3:7001", hraft.Nonvoter),
	})
	r.addVoterErr = errors.New("boom")
	clk := newSteppingClock()
	rec := New(r, "jaco-1", Config{TickInterval: time.Second, PromoteAfter: time.Nanosecond, RaftOpTimeout: time.Second})
	rec.SetClock(clk.now)
	rec.Tick(context.Background()) // stamp firstSeen
	clk.step(10 * time.Millisecond)
	// Members=3 -> target=3, voters=1: one promotion attempted; raft
	// returns error; Tick must NOT panic and the call must be recorded.
	rec.Tick(context.Background())
	if got := r.addVoterCalls; len(got) != 1 {
		t.Errorf("expected one AddVoter attempt that errored, got %v", got)
	}

	// Clear the error and verify the next tick retries successfully.
	r.addVoterErr = nil
	clk.step(10 * time.Millisecond)
	rec.Tick(context.Background())
	if got := len(r.addVoterCalls); got != 2 {
		t.Errorf("expected retry on next tick, got %d AddVoter calls", got)
	}
}

func TestSnapshot_LeaderReturnsSuffrageMap(t *testing.T) {
	r := newFakeRaft(true, []hraft.Server{
		srv("jaco-1", "10.0.0.1:7001", hraft.Voter),
		srv("jaco-2", "10.0.0.2:7001", hraft.Nonvoter),
	})
	got := Snapshot(r)
	if got["jaco-1"] != hraft.Voter || got["jaco-2"] != hraft.Nonvoter {
		t.Errorf("Snapshot: %+v", got)
	}
}

func TestSnapshot_FollowerReturnsNil(t *testing.T) {
	r := newFakeRaft(false, []hraft.Server{srv("jaco-1", "10.0.0.1:7001", hraft.Voter)})
	if got := Snapshot(r); got != nil {
		t.Errorf("Snapshot on follower = %+v, want nil", got)
	}
}
