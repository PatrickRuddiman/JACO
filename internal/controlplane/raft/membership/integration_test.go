package membership_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sort"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/raft/membership"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

// TestReconciler_PromotesAndDemotesAcrossMembershipChanges drives the
// 1 → 2 → 3 → 4 → 5 → 4 → 3 → 2 → 1 sequence from issue #143's
// acceptance criteria against three real raft nodes (one leader, two
// followers added/removed). At each step it asserts the leader's voter
// count matches Target(N), proving the reconciler converges without
// quorum collapse.
func TestReconciler_PromotesAndDemotesAcrossMembershipChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("real raft spin-up; skipped in -short")
	}

	// Bind 5 ports up front so we can re-add/remove without leaking
	// ephemeral collisions.
	addrs := make([]string, 5)
	for i := range addrs {
		addrs[i] = freePort(t)
	}
	leader, leaderState, _ := bootNode(t, "node-1", addrs[0], true)
	waitForLeaderLocal(t, leader, 5*time.Second)

	// Spawn a reconciler with a quick tick + tiny PromoteAfter so the
	// test is snappy. PromoteAfter still > 0 so we exercise the gate.
	rec := membership.New(leader, "node-1", membership.Config{
		TickInterval:  150 * time.Millisecond,
		PromoteAfter:  100 * time.Millisecond,
		RaftOpTimeout: 2 * time.Second,
	})
	rec.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rec.Run(ctx) }()

	// Helper: assert the leader's view of voters matches want within timeout.
	mustVoters := func(t *testing.T, want int, why string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		var got int
		for time.Now().Before(deadline) {
			snap := membership.Snapshot(leader)
			got = 0
			for _, s := range snap {
				if s == hraft.Voter {
					got++
				}
			}
			if got == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("voters at %s: got %d, want %d (snapshot=%v)", why, got, want, membership.Snapshot(leader))
	}

	// Step 1 -> 1 member, target=1, 1 voter (bootstrap state).
	mustVoters(t, 1, "1 member")

	// Step 1 -> 2: bug-003 case. Add node-2 as nonvoter (matching what
	// the controlplane handler does), wait for the reconciler to
	// observe the new configuration. Target stays at 1 — joiner stays
	// nonvoter.
	_, _, node2 := bootNode(t, "node-2", addrs[1], false)
	addNonvoter(t, leader, "node-2", addrs[1])
	mustVoters(t, 1, "2 members (bug-003 case)")
	requireSuffrage(t, leader, "node-2", hraft.Nonvoter)

	// Step 2 -> 3: target jumps to 3; both nonvoters get promoted.
	_, _, node3 := bootNode(t, "node-3", addrs[2], false)
	addNonvoter(t, leader, "node-3", addrs[2])
	mustVoters(t, 3, "3 members")

	// Step 3 -> 4: target stays at 3, 4th node stays nonvoter.
	_, _, node4 := bootNode(t, "node-4", addrs[3], false)
	addNonvoter(t, leader, "node-4", addrs[3])
	mustVoters(t, 3, "4 members")
	requireSuffrage(t, leader, "node-4", hraft.Nonvoter)

	// Step 4 -> 5: target jumps to 5; both pending nonvoters promote.
	_, _, node5 := bootNode(t, "node-5", addrs[4], false)
	addNonvoter(t, leader, "node-5", addrs[4])
	mustVoters(t, 5, "5 members")

	// Step 5 -> 4: remove node-5. Target drops to 3, so reconciler
	// must also demote one voter. Net effect: 5 voters -> 3 voters.
	removeServer(t, leader, "node-5")
	_ = node5.Shutdown()
	mustVoters(t, 3, "4 members after removing node-5")

	// Step 4 -> 3: remove node-4 (a nonvoter). Target stays at 3.
	removeServer(t, leader, "node-4")
	_ = node4.Shutdown()
	mustVoters(t, 3, "3 members after removing node-4")

	// Step 3 -> 2: remove node-3 (a voter). Target drops to 1; one
	// remaining voter (the leader stays a voter — refuses to demote
	// self) plus node-2 demoted from voter to nonvoter.
	removeServer(t, leader, "node-3")
	_ = node3.Shutdown()
	mustVoters(t, 1, "2 members after removing node-3")
	requireSuffrage(t, leader, "node-1", hraft.Voter)
	requireSuffrage(t, leader, "node-2", hraft.Nonvoter)

	// Step 2 -> 1: remove node-2. Target stays at 1.
	removeServer(t, leader, "node-2")
	_ = node2.Shutdown()
	mustVoters(t, 1, "1 member after removing node-2")

	_ = leaderState // silence unused
}

// bootNode constructs a raft node bound to addr. If bootstrap is true,
// it bootstraps as a single-voter cluster (only valid for the very
// first node). Returns the raft handle so the test can issue follower-
// only operations, plus the underlying State for the few tests that
// peek at the FSM.
func bootNode(t *testing.T, id, addr string, bootstrap bool) (*raftnode.Node, *state.State, *raftnode.Node) {
	t.Helper()
	dir := t.TempDir()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	n, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: addr, LocalID: id, Bootstrap: bootstrap, FSM: f, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("raft.New(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	return n, st, n
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitForLeaderLocal(t *testing.T, n *raftnode.Node, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.IsLeader() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never became leader within %s", timeout)
}

func addNonvoter(t *testing.T, leader *raftnode.Node, id, addr string) {
	t.Helper()
	f := leader.Raft.AddNonvoter(hraft.ServerID(id), hraft.ServerAddress(addr), 0, 5*time.Second)
	if err := f.Error(); err != nil {
		t.Fatalf("AddNonvoter(%s): %v", id, err)
	}
}

func removeServer(t *testing.T, leader *raftnode.Node, id string) {
	t.Helper()
	// Mirror the production NodeRemove pre-shrink: demote excess voters
	// before removing, so the cluster never lands in a (voters >
	// remaining_members - failure_budget) window.
	cfg := leader.Raft.GetConfiguration().Configuration()
	postMembers := 0
	var voters []string
	for _, s := range cfg.Servers {
		if string(s.ID) == id {
			continue
		}
		postMembers++
		if s.Suffrage == hraft.Voter {
			voters = append(voters, string(s.ID))
		}
	}
	target := membership.Target(postMembers)
	sort.Sort(sort.Reverse(sort.StringSlice(voters)))
	self := "node-1" // leader by construction in this test
	for len(voters) > target {
		var pick string
		for _, v := range voters {
			if v != self {
				pick = v
				break
			}
		}
		if pick == "" {
			break
		}
		f := leader.Raft.DemoteVoter(hraft.ServerID(pick), 0, 5*time.Second)
		if err := f.Error(); err != nil {
			t.Fatalf("DemoteVoter(%s): %v", pick, err)
		}
		// Drop pick from voters.
		newV := voters[:0]
		for _, v := range voters {
			if v != pick {
				newV = append(newV, v)
			}
		}
		voters = newV
	}
	f := leader.Raft.RemoveServer(hraft.ServerID(id), 0, 5*time.Second)
	if err := f.Error(); err != nil {
		t.Fatalf("RemoveServer(%s): %v", id, err)
	}
}

func requireSuffrage(t *testing.T, leader *raftnode.Node, id string, want hraft.ServerSuffrage) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := membership.Snapshot(leader)
		if got, ok := snap[id]; ok && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := membership.Snapshot(leader)
	t.Fatalf("suffrage(%s): want %v, snapshot=%v", id, want, snap)
}
