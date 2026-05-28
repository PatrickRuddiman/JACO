package ipam

import "github.com/PatrickRuddiman/jaco/internal/controlplane/state"

// AllocatedCIDRs exposes the unexported allocatedCIDRs helper to the external
// ipam_test package so tests can assert on the sorted set of allocated /24s.
func AllocatedCIDRs(s *state.State) []string { return allocatedCIDRs(s) }
