package membership_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/raft/membership"
)

func TestTarget(t *testing.T) {
	// Worked table from issue #143. The "tolerates" column documents the
	// failure budget for the chosen voter count; we don't assert it but
	// it's why the mapping is what it is.
	cases := []struct {
		members int
		want    int
	}{
		{0, 0},  // no cluster yet
		{1, 1},  // single-voter bootstrap
		{2, 1},  // even — keep target at 1 (joiner stays nonvoter; bug-003 case)
		{3, 3},  // promote both new nodes
		{4, 3},  // 4th stays nonvoter
		{5, 5},  // promote
		{6, 5},  // 6th stays nonvoter
		{7, 7},  // promote
		{8, 7},  // cap
		{9, 7},
		{10, 7},
		{15, 7},
		{100, 7},
	}
	for _, tc := range cases {
		if got := membership.Target(tc.members); got != tc.want {
			t.Errorf("Target(%d) = %d, want %d", tc.members, got, tc.want)
		}
	}
}

func TestPickPromote_DeterministicHostnameOrder(t *testing.T) {
	// All eligible: should pick the lexicographically smallest hostname
	// so two leaders pick identically without coordinating.
	nonvoters := []string{"jaco-3", "jaco-2", "jaco-5"}
	got := membership.PickPromote(nonvoters, func(string) bool { return true })
	if got != "jaco-2" {
		t.Errorf("PickPromote(...) = %q, want %q", got, "jaco-2")
	}
}

func TestPickPromote_NoneEligible(t *testing.T) {
	got := membership.PickPromote([]string{"a", "b"}, func(string) bool { return false })
	if got != "" {
		t.Errorf("PickPromote with no eligible candidates = %q, want \"\"", got)
	}
}

func TestPickPromote_SkipsIneligible(t *testing.T) {
	// "a" is eligible but lexicographically smallest; the gate excludes
	// it, so "b" must win — the picker must NOT just sort and pick first.
	got := membership.PickPromote([]string{"a", "b", "c"}, func(h string) bool { return h != "a" })
	if got != "b" {
		t.Errorf("PickPromote skipping a = %q, want %q", got, "b")
	}
}

func TestPickPromote_Empty(t *testing.T) {
	if got := membership.PickPromote(nil, func(string) bool { return true }); got != "" {
		t.Errorf("PickPromote(nil) = %q, want \"\"", got)
	}
}

func TestPickDemote_HostnameDescendingExcludingSelf(t *testing.T) {
	// Reverse-order pick keeps demotion and promotion from oscillating on
	// the same node when the cluster sits at target. With self="jaco-1",
	// the picker should never return "jaco-1".
	voters := []string{"jaco-1", "jaco-2", "jaco-3"}
	got := membership.PickDemote(voters, "jaco-1")
	if got != "jaco-3" {
		t.Errorf("PickDemote(...) = %q, want %q", got, "jaco-3")
	}
}

func TestPickDemote_RefusesToDemoteSelfWhenAlone(t *testing.T) {
	got := membership.PickDemote([]string{"jaco-1"}, "jaco-1")
	if got != "" {
		t.Errorf("PickDemote with only self = %q, want \"\" (refuse to demote leader)", got)
	}
}

func TestPickDemote_Empty(t *testing.T) {
	if got := membership.PickDemote(nil, "jaco-1"); got != "" {
		t.Errorf("PickDemote(nil) = %q, want \"\"", got)
	}
}
