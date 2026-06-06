// Package membership implements the voter-set policy and the leader-side
// reconciler that keeps the raft configuration aligned with it.
//
// Policy (issue #143):
//
//	voters_target(N):
//	  if N >= 7: return 7
//	  if N is odd: return N
//	  return N - 1   // (even)
//
// Why odd-only: a 4-voter cluster tolerates the same single failure as 3
// voters but pays one extra ack per commit; even voter counts buy nothing.
// Cap at 7: matches the etcd/consul recommendation — 7 voters already
// tolerate 3 simultaneous failures, and more voters cost commit latency
// without buying meaningful resilience.
//
// The package is intentionally raft-library-free so the policy + pickers
// can be unit-tested without spinning up real raft nodes; the reconciler
// in reconciler.go is the only file that imports hashicorp/raft.
package membership

import "sort"

// MaxVoters is the cap on the voter set, applied by Target.
const MaxVoters = 7

// Target returns the desired voter count for a cluster with members
// nodes total (voters + nonvoters). The mapping is the pure function from
// issue #143:
//
//	members | target
//	      0 |      0   (no cluster yet)
//	      1 |      1
//	      2 |      1
//	      3 |      3
//	      4 |      3
//	      5 |      5
//	      6 |      5
//	      7 |      7
//	     8+ |      7
func Target(members int) int {
	switch {
	case members <= 0:
		return 0
	case members >= MaxVoters:
		return MaxVoters
	case members%2 == 1:
		return members
	default:
		return members - 1
	}
}

// PickPromote returns the hostname of the nonvoter that should be
// promoted next, or "" if none are eligible. Ordering is hostname-ascending
// so two successive leaders agree on the choice without coordinating
// state. eligible(host) is the catch-up gate (typically: been a nonvoter
// for at least N seconds); the reconciler supplies it.
func PickPromote(nonvoters []string, eligible func(string) bool) string {
	if len(nonvoters) == 0 {
		return ""
	}
	candidates := make([]string, 0, len(nonvoters))
	for _, h := range nonvoters {
		if eligible == nil || eligible(h) {
			candidates = append(candidates, h)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	return candidates[0]
}

// PickDemote returns the hostname of the voter that should be demoted
// next, or "" if voters is empty. self is the leader's own hostname: the
// leader will never pick itself (demoting yourself loses the leadership
// race mid-reconcile). Ordering is hostname-descending so promotion and
// demotion don't pick the same node when the cluster is at exactly the
// target count and oscillating.
func PickDemote(voters []string, self string) string {
	if len(voters) == 0 {
		return ""
	}
	candidates := make([]string, 0, len(voters))
	for _, h := range voters {
		if h == self {
			continue
		}
		candidates = append(candidates, h)
	}
	if len(candidates) == 0 {
		// Only the leader is left; refuse to demote self.
		return ""
	}
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	return candidates[0]
}
