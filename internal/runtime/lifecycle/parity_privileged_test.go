package lifecycle

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestBuildConfig_ForwardsPrivileged — issue #119: ContainerSpec.Privileged
// must land on HostConfig.Privileged, and ContainerSpec.SecurityOpt must
// pass through to HostConfig.SecurityOpt verbatim (same string order).
func TestBuildConfig_ForwardsPrivileged(t *testing.T) {
	spec := compose.ContainerSpec{
		Image:       "nginx:1.27",
		Privileged:  true,
		SecurityOpt: []string{"seccomp=unconfined", "apparmor=unconfined"},
	}
	_, hc, _ := buildConfig(spec)

	if !hc.Privileged {
		t.Errorf("HostConfig.Privileged = false, want true")
	}
	if len(hc.SecurityOpt) != 2 {
		t.Fatalf("len(HostConfig.SecurityOpt) = %d, want 2", len(hc.SecurityOpt))
	}
	if hc.SecurityOpt[0] != "seccomp=unconfined" {
		t.Errorf("SecurityOpt[0] = %q", hc.SecurityOpt[0])
	}
	if hc.SecurityOpt[1] != "apparmor=unconfined" {
		t.Errorf("SecurityOpt[1] = %q", hc.SecurityOpt[1])
	}
}

// TestBuildConfig_PrivilegedZeroValues — zero-value Privileged/SecurityOpt
// in the spec produce zero-value docker fields (no --privileged, nil
// SecurityOpt) so docker's default security profile applies.
func TestBuildConfig_PrivilegedZeroValues(t *testing.T) {
	_, hc, _ := buildConfig(compose.ContainerSpec{Image: "nginx:1.27"})
	if hc.Privileged {
		t.Errorf("HostConfig.Privileged = true, want false (no --privileged)")
	}
	if hc.SecurityOpt != nil {
		t.Errorf("HostConfig.SecurityOpt = %v, want nil", hc.SecurityOpt)
	}
}
