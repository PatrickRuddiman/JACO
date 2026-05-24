package lifecycle

import (
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-units"

	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
)

// TestToDockerMounts_TranslatesEveryMountType — toDockerMounts is the
// adapter between compose.Mount and docker's mount.Mount; it maps the
// type string to mount.Type. Volume, bind, empty (=bind), and any
// other string passed through verbatim.
func TestToDockerMounts_TranslatesEveryMountType(t *testing.T) {
	got := toDockerMounts([]compose.Mount{
		{Type: "volume", Source: "vol", Target: "/data", ReadOnly: true},
		{Type: "bind", Source: "/src", Target: "/dst"},
		{Type: "", Source: "/src2", Target: "/dst2"},
		{Type: "tmpfs", Source: "", Target: "/tmp"},
	})
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	wantTypes := []mount.Type{mount.TypeVolume, mount.TypeBind, mount.TypeBind, mount.Type("tmpfs")}
	for i, m := range got {
		if m.Type != wantTypes[i] {
			t.Errorf("got[%d].Type = %v, want %v", i, m.Type, wantTypes[i])
		}
	}
	if got[0].ReadOnly != true {
		t.Errorf("got[0].ReadOnly = false; want true")
	}
}

// TestToDockerMounts_EmptyReturnsNil — defensive default.
func TestToDockerMounts_EmptyReturnsNil(t *testing.T) {
	if got := toDockerMounts(nil); got != nil {
		t.Errorf("toDockerMounts(nil) = %v, want nil", got)
	}
	if got := toDockerMounts([]compose.Mount{}); got != nil {
		t.Errorf("toDockerMounts([]) = %v, want nil", got)
	}
}

// TestToTmpfsMap_NilAndEmpty — both return nil so docker doesn't trip
// on a non-nil zero-len map.
func TestToTmpfsMap_NilAndEmpty(t *testing.T) {
	if got := toTmpfsMap(nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := toTmpfsMap([]string{}); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
}

// TestToTmpfsMap_PopulatesEmptyOptions — each entry becomes a key with
// "" value (caller doesn't pass per-mount options in v1).
func TestToTmpfsMap_PopulatesEmptyOptions(t *testing.T) {
	got := toTmpfsMap([]string{"/tmp", "/run"})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if v, ok := got["/tmp"]; !ok || v != "" {
		t.Errorf("/tmp entry = %v / %q", ok, v)
	}
	if v, ok := got["/run"]; !ok || v != "" {
		t.Errorf("/run entry = %v / %q", ok, v)
	}
}

// TestToUlimitsList_NilAndEmpty — both return nil.
func TestToUlimitsList_NilAndEmpty(t *testing.T) {
	if got := toUlimitsList(nil); got != nil {
		t.Errorf("nil input: got %v", got)
	}
	if got := toUlimitsList(map[string]compose.Ulimit{}); got != nil {
		t.Errorf("empty map: got %v", got)
	}
}

// TestToUlimitsList_TranslatesEachKey — keys become Name; Soft/Hard
// are preserved verbatim.
func TestToUlimitsList_TranslatesEachKey(t *testing.T) {
	got := toUlimitsList(map[string]compose.Ulimit{
		"nofile": {Soft: 1024, Hard: 4096},
		"nproc":  {Soft: 2048, Hard: 8192},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byName := map[string]*units.Ulimit{}
	for _, u := range got {
		byName[u.Name] = u
	}
	if u := byName["nofile"]; u == nil || u.Soft != 1024 || u.Hard != 4096 {
		t.Errorf("nofile entry wrong: %+v", u)
	}
	if u := byName["nproc"]; u == nil || u.Soft != 2048 || u.Hard != 8192 {
		t.Errorf("nproc entry wrong: %+v", u)
	}
}

// TestBuildConfig_RespectsEveryFieldOnSpec — drives the larger
// buildConfig branches: healthcheck, host config (ReadonlyRootfs,
// CapAdd/Drop, Sysctls, Mounts, Tmpfs, DNS, Ulimits), and the
// "no networks" path which leaves NetworkMode unset.
func TestBuildConfig_RespectsEveryFieldOnSpec(t *testing.T) {
	spec := compose.ContainerSpec{
		Image:      "nginx:1.27",
		Command:    []string{"nginx", "-g", "daemon off;"},
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/app",
		User:       "1000:1000",
		Labels:     map[string]string{"a": "b"},
		Healthcheck: &compose.Healthcheck{
			Test: []string{"CMD", "curl", "-f", "http://localhost/"},
		},
		ReadOnly:   true,
		CapAdd:     []string{"NET_ADMIN"},
		CapDrop:    []string{"ALL"},
		Sysctls:    map[string]string{"net.ipv4.ip_forward": "1"},
		Mounts:     []compose.Mount{{Type: "bind", Source: "/src", Target: "/dst"}},
		Tmpfs:      []string{"/tmp"},
		DNSServers: []string{"1.1.1.1"},
		Ulimits:    map[string]compose.Ulimit{"nofile": {Soft: 1024, Hard: 4096}},
	}
	cfg, hc, net := buildConfig(spec)
	if cfg.Image != "nginx:1.27" {
		t.Errorf("cfg.Image = %q", cfg.Image)
	}
	if cfg.Healthcheck == nil || len(cfg.Healthcheck.Test) == 0 {
		t.Errorf("Healthcheck not propagated")
	}
	if !hc.ReadonlyRootfs {
		t.Errorf("ReadonlyRootfs = false")
	}
	if len(hc.CapAdd) != 1 || hc.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("CapAdd = %v", hc.CapAdd)
	}
	if hc.Sysctls["net.ipv4.ip_forward"] != "1" {
		t.Errorf("Sysctls missing entry")
	}
	if len(hc.Mounts) != 1 {
		t.Errorf("Mounts = %v", hc.Mounts)
	}
	if hc.Tmpfs["/tmp"] != "" {
		t.Errorf("Tmpfs missing /tmp")
	}
	if len(hc.DNS) != 1 || hc.DNS[0] != "1.1.1.1" {
		t.Errorf("DNS = %v", hc.DNS)
	}
	if len(hc.Resources.Ulimits) != 1 {
		t.Errorf("Ulimits = %v", hc.Resources.Ulimits)
	}
	// No networks set → NetworkMode stays empty (not "" string literal
	// — the zero value of container.NetworkMode is "").
	if string(hc.NetworkMode) != "" {
		t.Errorf("NetworkMode = %q, want empty (no spec.Networks)", hc.NetworkMode)
	}
	if net.EndpointsConfig != nil {
		t.Errorf("EndpointsConfig = %v, want nil", net.EndpointsConfig)
	}
}

// TestBuildConfig_WithSingleNetwork — first declared network attaches
// at create-time via EndpointsConfig and sets NetworkMode (bug 010), and
// carries the service aliases for Docker's embedded DNS (issue #28).
func TestBuildConfig_WithSingleNetwork(t *testing.T) {
	spec := compose.ContainerSpec{Image: "x", Service: "web", Deployment: "app", Networks: []string{"jaco_app_frontend"}}
	_, hc, net := buildConfig(spec)
	if string(hc.NetworkMode) != "jaco_app_frontend" {
		t.Errorf("NetworkMode = %q, want jaco_app_frontend", hc.NetworkMode)
	}
	ep, ok := net.EndpointsConfig["jaco_app_frontend"]
	if !ok {
		t.Fatalf("EndpointsConfig missing jaco_app_frontend; got %v", net.EndpointsConfig)
	}
	want := []string{"web", "web.app", "web.app.jaco.internal"}
	if !reflect.DeepEqual(ep.Aliases, want) {
		t.Errorf("aliases = %v, want %v", ep.Aliases, want)
	}
}
