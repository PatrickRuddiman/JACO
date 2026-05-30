package lifecycle

import (
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestBuildConfig_ForwardsBatch1Fields — issues #114, #117, #118: every new
// ContainerSpec field must land on the matching docker Config/HostConfig
// field. One field per assertion so a regression names exactly which one
// stopped being forwarded.
func TestBuildConfig_ForwardsBatch1Fields(t *testing.T) {
	graceSecs := 45
	initTrue := true
	spec := compose.ContainerSpec{
		Image:                  "nginx:1.27",
		ReplicaID:              "r",
		StopSignal:             "SIGINT",
		StopGracePeriodSeconds: &graceSecs,
		Hostname:               "web",
		Domainname:             "example.com",
		ExtraHosts:             []string{"host1:1.2.3.4"},
		DNS:                    []string{"1.1.1.1"},
		DNSSearch:              []string{"example.com"},
		DNSOptions:             []string{"ndots:2"},
		Init:                   &initTrue,
		ShmSizeBytes:           64 * 1024 * 1024,
		IpcMode:                "shareable",
		PidMode:                "host",
		UTSMode:                "host",
		UsernsMode:             "host",
		CgroupnsMode:           "private",
		CgroupParent:           "/custom.slice",
	}
	cfg, hc, _ := buildConfig(spec)

	// Config.* (#114 shutdown + #117 host/domain)
	if cfg.StopSignal != "SIGINT" {
		t.Errorf("Config.StopSignal = %q, want SIGINT", cfg.StopSignal)
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != 45 {
		t.Errorf("Config.StopTimeout = %v, want *45", cfg.StopTimeout)
	}
	if cfg.Hostname != "web" {
		t.Errorf("Config.Hostname = %q, want web", cfg.Hostname)
	}
	if cfg.Domainname != "example.com" {
		t.Errorf("Config.Domainname = %q, want example.com", cfg.Domainname)
	}

	// HostConfig.* (#117 passthroughs)
	if len(hc.ExtraHosts) != 1 || hc.ExtraHosts[0] != "host1:1.2.3.4" {
		t.Errorf("HostConfig.ExtraHosts = %v, want [host1:1.2.3.4]", hc.ExtraHosts)
	}
	if len(hc.DNS) != 1 || hc.DNS[0] != "1.1.1.1" {
		t.Errorf("HostConfig.DNS = %v, want [1.1.1.1]", hc.DNS)
	}
	if len(hc.DNSSearch) != 1 || hc.DNSSearch[0] != "example.com" {
		t.Errorf("HostConfig.DNSSearch = %v, want [example.com]", hc.DNSSearch)
	}
	if len(hc.DNSOptions) != 1 || hc.DNSOptions[0] != "ndots:2" {
		t.Errorf("HostConfig.DNSOptions = %v, want [ndots:2]", hc.DNSOptions)
	}
	if hc.Init == nil || *hc.Init != true {
		t.Errorf("HostConfig.Init = %v, want *true", hc.Init)
	}
	if hc.ShmSize != 64*1024*1024 {
		t.Errorf("HostConfig.ShmSize = %d, want %d", hc.ShmSize, 64*1024*1024)
	}

	// HostConfig.* (#118 namespace knobs)
	if hc.IpcMode != container.IpcMode("shareable") {
		t.Errorf("HostConfig.IpcMode = %q, want shareable", hc.IpcMode)
	}
	if hc.PidMode != container.PidMode("host") {
		t.Errorf("HostConfig.PidMode = %q, want host", hc.PidMode)
	}
	if hc.UTSMode != container.UTSMode("host") {
		t.Errorf("HostConfig.UTSMode = %q, want host", hc.UTSMode)
	}
	if hc.UsernsMode != container.UsernsMode("host") {
		t.Errorf("HostConfig.UsernsMode = %q, want host", hc.UsernsMode)
	}
	if hc.CgroupnsMode != container.CgroupnsMode("private") {
		t.Errorf("HostConfig.CgroupnsMode = %q, want private", hc.CgroupnsMode)
	}
	if hc.CgroupParent != "/custom.slice" {
		t.Errorf("HostConfig.CgroupParent = %q, want /custom.slice", hc.CgroupParent)
	}
}

// TestBuildConfig_DNSPrecedence — compose `dns:` overrides the
// runtime-resolved per-bridge DNSServers when non-empty; empty `dns:` keeps
// the runtime servers so JACO's per-bridge DNS Manager wins (no regression
// from pre-#117 behavior).
func TestBuildConfig_DNSPrecedence(t *testing.T) {
	// compose dns: present → wins
	spec := compose.ContainerSpec{
		Image:      "nginx",
		ReplicaID:  "r",
		DNS:        []string{"1.1.1.1"},
		DNSServers: []string{"10.42.0.1"}, // runtime-resolved
	}
	_, hc, _ := buildConfig(spec)
	if len(hc.DNS) != 1 || hc.DNS[0] != "1.1.1.1" {
		t.Errorf("compose dns precedence failed: HostConfig.DNS = %v, want [1.1.1.1]", hc.DNS)
	}

	// compose dns: empty → runtime resolvers
	spec2 := compose.ContainerSpec{
		Image:      "nginx",
		ReplicaID:  "r",
		DNSServers: []string{"10.42.0.1"},
	}
	_, hc2, _ := buildConfig(spec2)
	if len(hc2.DNS) != 1 || hc2.DNS[0] != "10.42.0.1" {
		t.Errorf("runtime DNS fallback failed: HostConfig.DNS = %v, want [10.42.0.1]", hc2.DNS)
	}
}

// TestBuildConfig_Batch1ZeroValuesEmitDockerDefaults — when ContainerSpec
// carries zero values, docker fields stay zero so the engine applies its own
// defaults (no override emitted). Catches accidental non-zero defaults
// leaking into the engine call (e.g. ShmSize=0 must NOT become "0 bytes —
// reject" on docker's side).
func TestBuildConfig_Batch1ZeroValuesEmitDockerDefaults(t *testing.T) {
	cfg, hc, _ := buildConfig(compose.ContainerSpec{Image: "nginx", ReplicaID: "r"})
	if cfg.StopSignal != "" {
		t.Errorf("Config.StopSignal = %q, want empty", cfg.StopSignal)
	}
	if cfg.StopTimeout != nil {
		t.Errorf("Config.StopTimeout = %v, want nil", cfg.StopTimeout)
	}
	if cfg.Hostname != "" || cfg.Domainname != "" {
		t.Errorf("Hostname/Domainname leaked: %q/%q", cfg.Hostname, cfg.Domainname)
	}
	if hc.Init != nil {
		t.Errorf("HostConfig.Init = %v, want nil", hc.Init)
	}
	if hc.ShmSize != 0 {
		t.Errorf("HostConfig.ShmSize = %d, want 0", hc.ShmSize)
	}
	if hc.IpcMode != "" || hc.PidMode != "" || hc.UTSMode != "" || hc.UsernsMode != "" {
		t.Errorf("namespace modes leaked: ipc=%q pid=%q uts=%q userns=%q",
			hc.IpcMode, hc.PidMode, hc.UTSMode, hc.UsernsMode)
	}
	if hc.CgroupnsMode != "" || hc.CgroupParent != "" {
		t.Errorf("cgroup leaked: cgroup=%q parent=%q", hc.CgroupnsMode, hc.CgroupParent)
	}
}
